package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Phase represents the UI state machine derived from the sidecar lifecycle and
// /api/state responses.
type Phase int

const (
	PhaseBooting Phase = iota
	PhaseWaiting
	PhaseConnecting
	PhaseLive
	PhaseInterrupted
	PhaseFailed
)

func (p Phase) String() string {
	switch p {
	case PhaseBooting:
		return "booting"
	case PhaseWaiting:
		return "waiting"
	case PhaseConnecting:
		return "connecting"
	case PhaseLive:
		return "live"
	case PhaseInterrupted:
		return "interrupted"
	case PhaseFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// PendingSender matches receiver.PendingSender returned inside /api/state.
type PendingSender struct {
	Fingerprint string `json:"fingerprint"`
	Remote      string `json:"remote"`
	AgeSeconds  int64  `json:"ageSeconds"`
	Label       string `json:"label,omitempty"`
}

// ReceiverState matches apiState in backend/cmd/airplayd/state.go.
type ReceiverState struct {
	Status          string         `json:"status"`
	Reason          string         `json:"reason"`
	Name            string         `json:"name"`
	IP              string         `json:"ip"`
	Port            int            `json:"port"`
	Sender          string         `json:"sender"`
	Width           int            `json:"width"`
	Height          int            `json:"height"`
	VideoFrames     uint64         `json:"videoFrames"`
	AudioFrames     uint64         `json:"audioFrames"`
	Pictures        uint64         `json:"pictures"`
	Dropped         uint64         `json:"dropped"`
	Decoder         string         `json:"decoder"`
	Muted           bool           `json:"muted"`
	Volume          float64        `json:"volume"`
	UptimeSeconds   float64        `json:"uptimeSeconds"`
	Version         string         `json:"version"`
	TargetWidth     int            `json:"targetWidth"`
	TargetHeight    int            `json:"targetHeight"`
	MaxFPS          int            `json:"maxFps"`
	DecoderMode     string         `json:"decoderMode"`
	ColorRange      string         `json:"colorRange"`
	ScaleFilter     string         `json:"scaleFilter"`
	AudioBuffer     string         `json:"audioBuffer"`
	PendingSender   *PendingSender `json:"pendingSender"`
	FPS             float64        `json:"fps"`
	DecodeAvgMs     float64        `json:"decodeAvgMs"`
	DecodeP95Ms     float64        `json:"decodeP95Ms"`
	DownloadAvgMs   float64        `json:"downloadAvgMs"`
	DownloadP95Ms   float64        `json:"downloadP95Ms"`
	ConvertAvgMs    float64        `json:"convertAvgMs"`
	ConvertP95Ms    float64        `json:"convertP95Ms"`
	PublishAvgMs    float64        `json:"publishAvgMs"`
	PublishP95Ms    float64        `json:"publishP95Ms"`
	LagFrames       uint64         `json:"lagFrames"`
	ConsumerAgeMs   float64        `json:"consumerAgeMs"`
	ConsumerAdopted uint64         `json:"consumerAdopted"`
	HWMode          string         `json:"hwMode"`
	GPUZeroCopy     bool           `json:"gpuZeroCopy"`
	AudioOccupancy  float64        `json:"audioOccupancyMs"`
	AudioUnderruns  uint64         `json:"audioUnderruns"`
	AudioHighWater  float64        `json:"audioHighWaterMs"`
	AudioTrims      uint64         `json:"audioTrims"`
	AudioLateDrop   uint64         `json:"audioLateDropped"`
}

func defaultReceiverState() ReceiverState {
	return ReceiverState{
		Status:       "idle",
		Volume:       1.0,
		TargetWidth:  1920,
		TargetHeight: 1080,
		MaxFPS:       60,
		DecoderMode:  "auto",
		ColorRange:   "auto",
		ScaleFilter:  "fast",
		AudioBuffer:  "balanced",
	}
}

// Snapshot is an immutable UI state update emitted by Sidecar.
type Snapshot struct {
	Phase   Phase
	State   *ReceiverState
	Error   string
	Message string
}

// startupEvent is one line of the child's machine-readable startup protocol.
// ffmpeg_setup reports first-run provisioning; api_ready names the localhost
// API port the receiver actually bound.
type startupEvent struct {
	Event   string `json:"event"`
	Message string `json:"message"`
	Done    bool   `json:"done"`
	Failed  bool   `json:"failed"`
	Port    int    `json:"port"`
}

// Sidecar owns the airplayd.exe child process and communicates over its
// localhost HTTP API.
type Sidecar struct {
	installDir string
	// mapping is the shared-memory section name handed to the child with -shm.
	// Set it before Start; a change applies from the next launch.
	mapping    string
	onSnapshot func(Snapshot)

	// lifecycleMu serialises Start, Stop, Restart and Close, so two goroutines
	// asking for a restart can never launch two receivers or lose one process.
	lifecycleMu sync.Mutex
	emitMu      sync.Mutex

	mu sync.Mutex
	// gen identifies the current launch. Goroutines capture it when they are
	// spawned and every emission checks it, so a poll or stdout line from a
	// previous process is dropped instead of overwriting the new run's state.
	gen uint64
	// closed is set by Close and makes Start a permanent no-op.
	closed       bool
	cmd          *exec.Cmd
	waitDone     chan struct{}
	pollDone     chan struct{}
	stdoutDone   chan struct{}
	cancel       context.CancelFunc
	launchCtx    context.Context
	baseURL      string
	pollClient   *http.Client
	postClient   *http.Client
	lastProgress time.Time
	stopped      bool
	everReached  bool
	runtimeSetup bool
	startupError string
	// failureReported keeps a terminal failure from being announced once per
	// tick: a failed ffmpeg_setup, an unreachable startup or a dead process is
	// reported once, and a repeat timeout or the following exit must not
	// replace it. A successful poll clears it, so a later exit is still
	// reported after the backend has recovered.
	failureReported bool
	failureShown    bool
	pollTrigger     chan struct{}
}

// NewSidecar creates a sidecar controller rooted at installDir.
func NewSidecar(installDir string, onSnapshot func(Snapshot)) *Sidecar {
	return &Sidecar{
		installDir: installDir,
		onSnapshot: onSnapshot,
		pollClient: &http.Client{Timeout: 700 * time.Millisecond},
		postClient: &http.Client{Timeout: 2 * time.Second},
	}
}

func (s *Sidecar) InstallDir() string { return s.installDir }
func (s *Sidecar) LogPath() string    { return filepath.Join(s.installDir, "logs", "airplayd.log") }
func (s *Sidecar) ConfigPath() string { return ConfigPath(s.installDir) }

func (s *Sidecar) emit(snap Snapshot) {
	if s.onSnapshot != nil {
		s.onSnapshot(snap)
	}
}

// emitFor delivers a snapshot only if it belongs to the current launch. Every
// asynchronous producer checks through this, so a poll, stdout line or exit
// report from a previous launch is dropped instead of overwriting the new one.
func (s *Sidecar) emitFor(gen uint64, snap Snapshot) {
	// Keep the eligibility check and delivery in order across poll, startup
	// and exit goroutines. The callback queues work on the UI thread.
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	s.mu.Lock()
	ok := !s.stopped && !s.closed && s.gen == gen
	if snap.Phase == PhaseBooting && s.everReached || snap.Phase == PhaseFailed && !s.failureReported {
		ok = false
	}
	if ok {
		s.failureShown = snap.Phase == PhaseFailed
	}
	s.mu.Unlock()
	if ok {
		s.emit(snap)
	}
}

// Start launches airplayd.exe and starts polling /api/state every 350ms.
func (s *Sidecar) Start() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.stopLocked()
	s.startLocked()
}

// startLocked launches a fresh process. The caller holds lifecycleMu.
func (s *Sidecar) startLocked() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.stopped = false
	s.failureShown = false
	gen := s.gen
	s.mu.Unlock()

	exePath := filepath.Join(s.installDir, "backend", "airplayd.exe")
	if _, err := os.Stat(exePath); err != nil {
		s.startFailure(gen, fmt.Sprintf("找不到 backend\\airplayd.exe（%s）", exePath))
		return
	}

	// The child binds its loopback API on an ephemeral port and announces it
	// with api_ready; the UI never polls a port it guessed for itself.
	cmd := newSidecarCmd(exePath, s.installDir, sidecarArgs(s.installDir, s.mapping))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.startFailure(gen, fmt.Sprintf("無法建立輸出管線：%v", err))
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		s.startFailure(gen, fmt.Sprintf("無法建立錯誤管線：%v", err))
		return
	}

	if err := cmd.Start(); err != nil {
		s.startFailure(gen, fmt.Sprintf("無法啟動接收器：%v", err))
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	waitDone := make(chan struct{})
	pollDone := make(chan struct{})
	stdoutDone := make(chan struct{})
	trigger := make(chan struct{}, 1)

	s.mu.Lock()
	s.cmd = cmd
	s.waitDone = waitDone
	s.pollDone = pollDone
	s.stdoutDone = stdoutDone
	s.cancel = cancel
	s.launchCtx = ctx
	s.baseURL = ""
	s.lastProgress = time.Now()
	s.everReached = false
	s.runtimeSetup = false
	s.startupError = ""
	s.failureReported = false
	s.pollTrigger = trigger
	s.mu.Unlock()

	s.emitFor(gen, Snapshot{Phase: PhaseBooting, Message: "正在啟動接收器…"})

	go func() {
		_, _ = io.Copy(io.Discard, stderr)
	}()

	go func() {
		defer close(stdoutDone)
		s.readStartupStdout(gen, stdout)
	}()

	go func() {
		// Close waitDone only after the exit has been handled, so Stop (and the
		// Start behind it) cannot overtake the failure this launch reports.
		defer close(waitDone)
		s.handleExit(gen, cmd.Wait())
	}()

	go func() {
		defer close(pollDone)
		s.pollLoop(ctx, gen, trigger)
	}()
}

// startFailure reports a launch that never got a process running.
func (s *Sidecar) startFailure(gen uint64, msg string) {
	s.mu.Lock()
	if s.gen != gen || s.stopped || s.closed {
		s.mu.Unlock()
		return
	}
	s.failureReported = true
	s.mu.Unlock()
	s.emitFor(gen, Snapshot{Phase: PhaseFailed, Error: msg})
}

// sidecarArgs builds the child command line. The API port is left to the child
// (0) and learned from its api_ready line, so there is no window where the UI
// probes a port another process can take before the receiver binds. -shm is
// only passed when the UI names a mapping, so an empty mapping keeps the
// receiver's own default.
func sidecarArgs(installDir, mapping string) []string {
	args := []string{
		"-api-port", "0",
		"-data", filepath.Join(installDir, "config"),
		"-log", filepath.Join(installDir, "logs", "airplayd.log"),
		"-parent-pid", strconv.Itoa(os.Getpid()),
	}
	if mapping != "" {
		args = append(args, "-shm", mapping)
	}
	return args
}

// newSidecarCmd builds the child process. It is a variable so tests can run a
// fake receiver in place of backend\airplayd.exe.
var newSidecarCmd = func(exePath, dir string, args []string) *exec.Cmd {
	cmd := exec.Command(exePath, args...)
	cmd.Dir = dir
	configureSidecarProcess(cmd)
	return cmd
}

func (s *Sidecar) readStartupStdout(gen uint64, r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		s.handleStartupLine(gen, scanner.Text())
	}
}

// handleStdoutLine processes a startup line for the current launch.
func (s *Sidecar) handleStdoutLine(line string) {
	s.mu.Lock()
	gen := s.gen
	s.mu.Unlock()
	s.handleStartupLine(gen, line)
}

func (s *Sidecar) handleStartupLine(gen uint64, line string) {
	s.mu.Lock()
	if s.stopped || s.closed || s.gen != gen || s.everReached {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	var ev startupEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return
	}

	switch ev.Event {
	case "ffmpeg_setup":
		if ev.Message == "" {
			return
		}
		s.mu.Lock()
		if s.stopped || s.closed || s.gen != gen || s.everReached {
			s.mu.Unlock()
			return
		}
		report := false
		if ev.Failed {
			s.startupError = ev.Message
			if !s.failureReported {
				s.failureReported = true
				report = true
			}
			s.mu.Unlock()
			if report {
				s.emitFor(gen, Snapshot{Phase: PhaseFailed, Error: ev.Message})
			}
			return
		}
		s.runtimeSetup = !ev.Done
		if !s.runtimeSetup {
			s.lastProgress = time.Now()
		}
		trigger := s.pollTrigger
		s.mu.Unlock()

		s.emitFor(gen, Snapshot{Phase: PhaseBooting, Message: ev.Message})
		signalPoll(trigger)

	case "api_ready":
		s.mu.Lock()
		if s.stopped || s.closed || s.gen != gen || s.everReached {
			s.mu.Unlock()
			return
		}
		// The port the child bound is the only address worth polling; there is
		// no earlier, guessed endpoint that another process could occupy.
		if ev.Port > 0 && ev.Port <= 65535 {
			s.baseURL = fmt.Sprintf("http://127.0.0.1:%d", ev.Port)
		}
		trigger := s.pollTrigger
		s.mu.Unlock()
		signalPoll(trigger)
	}
}

func signalPoll(ch chan struct{}) {
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *Sidecar) pollLoop(ctx context.Context, gen uint64, trigger <-chan struct{}) {
	s.tickFor(ctx, gen)
	ticker := time.NewTicker(350 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tickFor(ctx, gen)
		case <-trigger:
			s.tickFor(ctx, gen)
		}
	}
}

func (s *Sidecar) triggerPoll() {
	s.mu.Lock()
	ch := s.pollTrigger
	idle := s.stopped || s.closed
	s.mu.Unlock()
	if idle {
		return
	}
	signalPoll(ch)
}

// tick polls the current launch once.
func (s *Sidecar) tick(ctx context.Context) {
	s.mu.Lock()
	gen := s.gen
	s.mu.Unlock()
	s.tickFor(ctx, gen)
}

func (s *Sidecar) tickFor(ctx context.Context, gen uint64) {
	s.mu.Lock()
	live := !s.stopped && !s.closed && s.gen == gen
	baseURL := s.baseURL
	s.mu.Unlock()
	if !live {
		return
	}
	if baseURL == "" {
		// The child has not announced its API yet. Keep the startup deadline
		// running instead of polling a port we do not know.
		s.handlePollError(gen)
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, baseURL+"/api/state", nil)
	if err != nil {
		return
	}
	resp, err := s.pollClient.Do(req)
	if err != nil {
		s.handlePollError(gen)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		s.handlePollError(gen)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		s.handlePollError(gen)
		return
	}

	state := defaultReceiverState()
	if err := json.Unmarshal(body, &state); err != nil {
		s.handlePollError(gen)
		return
	}

	s.mu.Lock()
	live = !s.stopped && !s.closed && s.gen == gen
	if live {
		s.everReached = true
		s.lastProgress = time.Now()
		s.startupError = ""
		s.runtimeSetup = false
		// A state now proves the process answers, so an earlier "no response"
		// failure has been recovered from: a later exit must be reported
		// instead of being deduplicated against that stale failure.
		s.failureReported = false
	}
	s.mu.Unlock()
	if !live {
		return
	}

	s.emitFor(gen, Snapshot{
		Phase: phaseFromStatus(state.Status),
		State: &state,
	})
}

func (s *Sidecar) handlePollError(gen uint64) {
	s.mu.Lock()
	deadline := 8 * time.Second
	if !s.everReached && s.runtimeSetup {
		deadline = 11 * time.Minute
	}
	if s.stopped || s.closed || s.gen != gen || s.failureReported || time.Since(s.lastProgress) <= deadline {
		s.mu.Unlock()
		return
	}
	s.failureReported = true
	msg := s.startupError
	if s.everReached {
		msg = "接收器已停止回應，正在重新連線…"
	} else if msg == "" {
		msg = "接收器沒有回應"
	}
	s.mu.Unlock()
	s.emitFor(gen, Snapshot{Phase: PhaseFailed, Error: msg})
}

// handleExit turns a child exit into the right UI state. A process that dies
// before the API ever answered is a startup failure; one that dies after the
// first successful poll is an unexpected exit, and it must be surfaced rather
// than leaving the UI on a stale "live" picture. An exit Stop asked for is
// ignored.
func (s *Sidecar) handleExit(gen uint64, waitErr error) {
	s.mu.Lock()
	if s.gen != gen || s.stopped || s.closed {
		s.mu.Unlock()
		return
	}
	ever := s.everReached
	startupErr := s.startupError
	// A timeout may have reserved a failure but not yet delivered it. An exit
	// invalidates that pending notification, so it must deliver one itself.
	report := !s.failureShown || ever
	s.failureReported = true
	// Nothing can answer on this launch any more. Advance the generation so an
	// in-flight poll cannot publish state after the failure, and stop polling.
	// The failure is emitted under the new generation, so a restart that has
	// already moved on drops it instead of showing it over the new run.
	s.gen++
	deadGen := s.gen
	s.baseURL = ""
	cancel := s.cancel
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if !report {
		return
	}

	msg := startupErr
	if msg == "" {
		code := -1
		if waitErr == nil {
			code = 0
		} else {
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				code = exitErr.ExitCode()
			}
		}
		if ever {
			msg = fmt.Sprintf("接收器意外結束（代碼 %d）", code)
		} else {
			msg = fmt.Sprintf("接收器結束了（代碼 %d）", code)
		}
	}
	s.emitFor(deadGen, Snapshot{Phase: PhaseFailed, Error: msg})
}

func phaseFromStatus(status string) Phase {
	switch status {
	case "live":
		return PhaseLive
	case "interrupted":
		return PhaseInterrupted
	case "connecting":
		return PhaseConnecting
	default:
		return PhaseWaiting
	}
}

func (s *Sidecar) postJSON(path string, payload any, out any) error {
	s.mu.Lock()
	if s.stopped || s.closed || s.baseURL == "" {
		s.mu.Unlock()
		return errors.New("sidecar is not running")
	}
	gen := s.gen
	baseURL := s.baseURL
	launchCtx := s.launchCtx
	s.mu.Unlock()

	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}

	if launchCtx == nil {
		launchCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(launchCtx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := s.postClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(respBytes))
		if msg == "" {
			msg = resp.Status
		}
		return errors.New(msg)
	}

	// A restart between sending and reading means the answer may describe a
	// process that is already gone; do not report success for it.
	s.mu.Lock()
	current := s.gen == gen && !s.stopped && !s.closed
	s.mu.Unlock()
	if !current {
		return errors.New("接收器已重新啟動")
	}

	if out != nil && len(bytes.TrimSpace(respBytes)) > 0 {
		if err := json.Unmarshal(respBytes, out); err != nil {
			return err
		}
	}
	return nil
}

// Disconnect drops the active mirroring session and returns to waiting.
func (s *Sidecar) Disconnect() error {
	err := s.postJSON("/api/disconnect", nil, nil)
	s.triggerPoll()
	return err
}

// SetAudio updates mute state and/or linear volume [0.0, 1.0].
func (s *Sidecar) SetAudio(muted *bool, volume *float64) error {
	payload := map[string]any{}
	if muted != nil {
		payload["muted"] = *muted
	}
	if volume != nil {
		payload["volume"] = clampFloat(*volume, 0.0, 1.0)
	}
	if len(payload) == 0 {
		return nil
	}
	err := s.postJSON("/api/audio", payload, nil)
	s.triggerPoll()
	return err
}

// TakeSnapshot captures the latest decoded frame to PNG and returns the file path.
func (s *Sidecar) TakeSnapshot() (string, error) {
	var resp struct {
		Path string `json:"path"`
	}
	if err := s.postJSON("/api/snapshot", nil, &resp); err != nil {
		return "", err
	}
	if resp.Path == "" {
		return "", errors.New("snapshot response missing path")
	}
	return resp.Path, nil
}

// ApproveSender approves a verified pending sender by fingerprint.
func (s *Sidecar) ApproveSender(fingerprint string) error {
	err := s.postJSON("/api/pairing/trust/approve", map[string]string{"fingerprint": fingerprint}, nil)
	s.triggerPoll()
	return err
}

// RejectSender rejects a verified pending sender by fingerprint.
func (s *Sidecar) RejectSender(fingerprint string) error {
	err := s.postJSON("/api/pairing/trust/reject", map[string]string{"fingerprint": fingerprint}, nil)
	s.triggerPoll()
	return err
}

// ResetTrustedSenders clears all remembered trusted senders.
func (s *Sidecar) ResetTrustedSenders() error {
	err := s.postJSON("/api/pairing/trust/reset", nil, nil)
	s.triggerPoll()
	return err
}

// LogTail reads recent lines from the end of the log without loading an
// entire long-running receiver log on the UI thread.
func (s *Sidecar) LogTail(lines int) []string {
	if lines <= 0 {
		lines = 12
	}
	file, err := os.Open(s.LogPath())
	if err != nil {
		return nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil
	}
	const maxBytes = 128 << 10
	start := max(int64(0), info.Size()-maxBytes)
	data, err := io.ReadAll(io.NewSectionReader(file, start, info.Size()-start))
	if err != nil {
		return nil
	}
	if start > 0 {
		if _, rest, ok := bytes.Cut(data, []byte{'\n'}); ok && len(rest) > 0 {
			data = rest
		}
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil
	}
	all := strings.Split(text, "\n")
	if len(all) <= lines {
		return all
	}
	return append([]string(nil), all[len(all)-lines:]...)
}

// Restart stops the sidecar if running and starts a fresh instance. Both steps
// happen under one lock, so a concurrent Start or Stop cannot interleave.
func (s *Sidecar) Restart() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.stopLocked()
	s.startLocked()
}

// Stop cancels polling, kills airplayd.exe if running, and waits for the
// launch's goroutines so no snapshot from it can arrive after Stop returns.
func (s *Sidecar) Stop() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.stopLocked()
}

// Close shuts the sidecar down for good. After Close, Start and Restart do
// nothing, so a restart racing the window teardown cannot leave a receiver
// running with no UI. Close is idempotent and safe to call with a launch in
// flight.
func (s *Sidecar) Close() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.stopLocked()
}

// stopLocked tears down the current launch. The caller holds lifecycleMu.
func (s *Sidecar) stopLocked() {
	s.mu.Lock()
	s.stopped = true
	s.gen++
	cancel := s.cancel
	cmd := s.cmd
	waitDone := s.waitDone
	pollDone := s.pollDone
	stdoutDone := s.stdoutDone
	s.cancel = nil
	s.launchCtx = nil
	s.cmd = nil
	s.waitDone = nil
	s.pollDone = nil
	s.stdoutDone = nil
	s.baseURL = ""
	s.pollTrigger = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	s.pollClient.CloseIdleConnections()
	s.postClient.CloseIdleConnections()

	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}

	// Join the launch's goroutines so their callbacks cannot outlive the stop.
	// The deadline only matters when a callback is wedged; the generation bump
	// above already keeps any late emission from being shown.
	deadline := time.Now().Add(2 * time.Second)
	waitClosed(waitDone, deadline)
	waitClosed(pollDone, deadline)
	waitClosed(stdoutDone, deadline)
}

func waitClosed(ch chan struct{}, deadline time.Time) {
	if ch == nil {
		return
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
	}
}
