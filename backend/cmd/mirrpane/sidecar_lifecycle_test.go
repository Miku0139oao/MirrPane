package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// snapshots collects what the app would have shown.
type snapshots struct {
	mu   sync.Mutex
	list []Snapshot
}

func (c *snapshots) add(sn Snapshot) {
	c.mu.Lock()
	c.list = append(c.list, sn)
	c.mu.Unlock()
}

func (c *snapshots) last() (Snapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.list) == 0 {
		return Snapshot{}, false
	}
	return c.list[len(c.list)-1], true
}

func (c *snapshots) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.list)
}

func newFakeInstallDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "backend"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "backend", "airplayd.exe"), []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func installFakeSidecar(t *testing.T, dir string) *fakeSidecar {
	t.Helper()
	f := &fakeSidecar{dir: dir}
	old := newSidecarCmd
	newSidecarCmd = func(exePath, workDir string, args []string) *exec.Cmd {
		f.mu.Lock()
		f.launches++
		f.mu.Unlock()
		cmd := exec.Command(os.Args[0], "-test.run=^TestFakeSidecarChild$")
		cmd.Dir = workDir
		cmd.Env = append(os.Environ(),
			"MIRRPANE_FAKE_SIDECAR=1",
			"MIRRPANE_FAKE_DIR="+dir,
		)
		return cmd
	}
	t.Cleanup(func() { newSidecarCmd = old })
	return f
}

type fakeSidecar struct {
	mu       sync.Mutex
	dir      string
	launches int
}

func (f *fakeSidecar) launchesCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.launches
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSidecarArgsPassMappingAndLogPath(t *testing.T) {
	dir := `C:\MirrPane`
	plain := sidecarArgs(dir, "")
	joined := strings.Join(plain, "\x00")
	if strings.Contains(joined, "-shm") {
		t.Fatalf("empty mapping must not add -shm: %v", plain)
	}
	if !strings.Contains(joined, "-api-port\x000") {
		t.Fatalf("the child must pick its own API port: %v", plain)
	}
	if !strings.Contains(joined, filepath.Join(dir, "logs", "airplayd.log")) {
		t.Fatalf("log path missing: %v", plain)
	}

	const mapping = `Local\MirrPaneTestMapping`
	mapped := sidecarArgs(dir, mapping)
	idx := -1
	for i, a := range mapped {
		if a == "-shm" {
			idx = i
		}
	}
	if idx < 0 || idx+1 >= len(mapped) || mapped[idx+1] != mapping {
		t.Fatalf("-shm %q missing from %v", mapping, mapped)
	}
}

func TestEmitDropsStaleGenerations(t *testing.T) {
	col := &snapshots{}
	s := NewSidecar(t.TempDir(), col.add)

	s.mu.Lock()
	s.gen = 2
	s.mu.Unlock()

	s.emitFor(1, Snapshot{Phase: PhaseWaiting, Message: "old"})
	s.emitFor(2, Snapshot{Phase: PhaseWaiting, Message: "new"})

	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.emitFor(2, Snapshot{Phase: PhaseWaiting, Message: "after-stop"})

	if col.count() != 1 {
		t.Fatalf("emitted %d snapshots, want 1", col.count())
	}
}

func TestStartupLineGenerationAndAPIRecord(t *testing.T) {
	col := &snapshots{}
	s := NewSidecar(t.TempDir(), col.add)
	s.mu.Lock()
	s.gen = 7
	s.mu.Unlock()

	// A line from a previous launch must not move the current launch.
	s.handleStartupLine(6, `{"event":"api_ready","port":45678}`)
	s.mu.Lock()
	baseURL := s.baseURL
	s.mu.Unlock()
	if baseURL != "" {
		t.Fatalf("stale api_ready changed baseURL to %q", baseURL)
	}

	s.handleStartupLine(7, `{"event":"api_ready","port":45678}`)
	s.mu.Lock()
	baseURL = s.baseURL
	s.mu.Unlock()
	if baseURL != "http://127.0.0.1:45678" {
		t.Fatalf("baseURL = %q, want api_ready port", baseURL)
	}

	s.handleStartupLine(6, `{"event":"ffmpeg_setup","message":"舊的失敗","failed":true}`)
	s.mu.Lock()
	startupErr := s.startupError
	s.mu.Unlock()
	if startupErr != "" {
		t.Fatalf("stale ffmpeg_setup set startup error %q", startupErr)
	}

	s.handleStartupLine(7, `{"event":"ffmpeg_setup","message":"FFmpeg 初始化失敗","failed":true}`)
	s.handleStartupLine(7, `{"event":"ffmpeg_setup","message":"FFmpeg 初始化失敗","failed":true}`)
	if col.count() != 1 {
		t.Fatalf("failure reported %d times, want exactly once", col.count())
	}
	last, _ := col.last()
	if last.Phase != PhaseFailed || last.Error != "FFmpeg 初始化失敗" {
		t.Fatalf("unexpected failure snapshot: %+v", last)
	}
}

func TestCloseBlocksLateStart(t *testing.T) {
	dir := newFakeInstallDir(t)
	f := installFakeSidecar(t, dir)
	s := NewSidecar(dir, nil)

	s.Close()
	s.Start()
	s.Restart()
	s.Close() // idempotent

	if n := f.launchesCount(); n != 0 {
		t.Fatalf("launched %d processes after Close, want 0", n)
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if !closed {
		t.Fatal("Close did not mark the sidecar closed")
	}
	muted := true
	if err := s.SetAudio(&muted, nil); err == nil {
		t.Fatal("SetAudio after Close should fail")
	}
}

func TestUnexpectedExitAfterFirstSuccessReportsFailure(t *testing.T) {
	dir := newFakeInstallDir(t)
	installFakeSidecar(t, dir)
	col := &snapshots{}
	s := NewSidecar(dir, col.add)
	s.mapping = `Local\MirrPaneTestMapping`
	s.Start()
	t.Cleanup(s.Close)

	waitFor(t, "first successful poll", func() bool {
		sn, ok := col.last()
		return ok && sn.Phase == PhaseWaiting && sn.State != nil && sn.State.Name == "fake"
	})

	if err := os.WriteFile(filepath.Join(dir, "exit"), []byte("exit"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "failure after unexpected exit", func() bool {
		sn, ok := col.last()
		return ok && sn.Phase == PhaseFailed &&
			strings.Contains(sn.Error, "意外結束") && strings.Contains(sn.Error, "7")
	})

	// After the failure the dead launch must stay quiet: a late poll or exit
	// report would overwrite the failure with stale state.
	before := col.count()
	time.Sleep(500 * time.Millisecond)
	if after := col.count(); after != before {
		t.Fatalf("dead launch emitted %d more snapshots after the failure", after-before)
	}
	s.Stop()
}

func TestConcurrentLifecycleIsSerialized(t *testing.T) {
	dir := newFakeInstallDir(t)
	installFakeSidecar(t, dir)
	s := NewSidecar(dir, nil)

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 3 {
			case 0:
				s.Start()
			case 1:
				s.Stop()
			case 2:
				s.Restart()
			}
		}(i)
	}
	wg.Wait()
	s.Close()
}

func TestPostJSONRejectsSupersededLaunch(t *testing.T) {
	var s *Sidecar
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The launch is replaced while the response is in flight.
		s.mu.Lock()
		s.gen++
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	s = NewSidecar(t.TempDir(), nil)
	s.mu.Lock()
	s.baseURL = srv.URL
	s.mu.Unlock()

	if err := s.Disconnect(); err == nil || !strings.Contains(err.Error(), "重新啟動") {
		t.Fatalf("Disconnect err = %v, want superseded-launch error", err)
	}
}

func TestTickDropsStateFromSupersededLaunch(t *testing.T) {
	var s *Sidecar
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.gen++
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"idle","name":"stale"}`))
	}))
	defer srv.Close()

	col := &snapshots{}
	s = NewSidecar(t.TempDir(), col.add)
	s.mu.Lock()
	s.baseURL = srv.URL
	s.mu.Unlock()

	s.tickFor(t.Context(), 0)
	if col.count() != 0 {
		t.Fatalf("superseded launch published %d snapshots", col.count())
	}
}

func TestSuccessfulPollReArmsExitReporting(t *testing.T) {
	col := &snapshots{}
	s := NewSidecar(t.TempDir(), col.add)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"idle","name":"ok"}`))
	}))
	defer srv.Close()

	s.mu.Lock()
	s.baseURL = srv.URL
	// An earlier startup-timeout failure was shown, then the API came up.
	s.failureReported = true
	s.mu.Unlock()

	s.tick(t.Context())

	s.mu.Lock()
	failureReported := s.failureReported
	s.mu.Unlock()
	if failureReported {
		t.Fatal("a successful poll must re-arm exit reporting")
	}
	if col.count() != 1 {
		t.Fatalf("emitted %d snapshots, want 1", col.count())
	}
}

func TestExitAfterFailureIsReportedOncePerCause(t *testing.T) {
	col := &snapshots{}
	s := NewSidecar(t.TempDir(), col.add)
	s.mu.Lock()
	s.gen = 5
	s.failureReported = true
	s.mu.Unlock()
	s.emitFor(5, Snapshot{Phase: PhaseFailed, Error: "接收器沒有回應"})

	s.handleExit(5, nil)
	if col.count() != 1 {
		t.Fatalf("already-reported failure re-announced: %v", col.count())
	}

	col2 := &snapshots{}
	s2 := NewSidecar(t.TempDir(), col2.add)
	s2.mu.Lock()
	s2.gen = 5
	s2.everReached = true
	s2.mu.Unlock()
	s2.handleExit(5, nil)
	if col2.count() != 1 {
		t.Fatalf("exit after first success emitted %d snapshots, want 1", col2.count())
	}
	sn, _ := col2.last()
	if sn.Phase != PhaseFailed || !strings.Contains(sn.Error, "意外結束") {
		t.Fatalf("unexpected exit snapshot: %+v", sn)
	}
	s2.mu.Lock()
	gen := s2.gen
	s2.mu.Unlock()
	if gen != 6 {
		t.Fatalf("exit did not advance generation: %d", gen)
	}
}

func TestStartupTimeoutReportedOnceWithoutAPI(t *testing.T) {
	col := &snapshots{}
	s := NewSidecar(t.TempDir(), col.add)
	s.mu.Lock()
	s.gen = 3
	s.lastProgress = time.Now().Add(-9 * time.Second)
	s.mu.Unlock()

	s.handlePollError(3)
	if col.count() != 1 {
		t.Fatalf("startup timeout emitted %d snapshots, want 1", col.count())
	}
	sn, _ := col.last()
	if sn.Phase != PhaseFailed || sn.Error != "接收器沒有回應" {
		t.Fatalf("unexpected timeout snapshot: %+v", sn)
	}

	// The poll loop keeps ticking while no API is known; the failure must not
	// be re-announced every tick.
	s.handlePollError(3)
	if col.count() != 1 {
		t.Fatalf("timeout re-announced: %d snapshots", col.count())
	}

	// The backend resuming late must still be able to hand over its address.
	s.handleStartupLine(3, `{"event":"api_ready","port":45679}`)
	s.mu.Lock()
	baseURL := s.baseURL
	s.mu.Unlock()
	if baseURL != "http://127.0.0.1:45679" {
		t.Fatalf("late api_ready ignored: baseURL = %q", baseURL)
	}
}

func TestUnresponsiveReceiverRecoversAfterFirstSuccessfulPoll(t *testing.T) {
	col := &snapshots{}
	s := NewSidecar(t.TempDir(), col.add)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"live"}`))
	}))
	defer srv.Close()
	s.baseURL = srv.URL
	s.tick(t.Context())
	s.handlePollError(s.gen)
	if col.count() != 1 {
		t.Fatal("a single missed poll cleared the live state")
	}
	s.lastProgress = time.Now().Add(-9 * time.Second)
	s.handlePollError(s.gen)
	s.handlePollError(s.gen)
	if last, _ := col.last(); col.count() != 2 || last.Phase != PhaseFailed {
		t.Fatalf("expected one outage notification, got %+v (%d)", last, col.count())
	}
	s.tick(t.Context())
	if last, _ := col.last(); col.count() != 3 || last.Phase != PhaseLive || s.failureReported {
		t.Fatalf("receiver did not recover: %+v (%d)", last, col.count())
	}
	s.lastProgress = time.Now().Add(-9 * time.Second)
	s.handlePollError(s.gen)
	s.handleExit(s.gen, nil)
	if last, _ := col.last(); !strings.Contains(last.Error, "意外結束") {
		t.Fatalf("dead receiver still claims to be reconnecting: %+v", last)
	}
}

// TestFakeSidecarChild is the fake receiver process: it binds its API on an
// ephemeral loopback port, announces it with api_ready, serves /api/state, and
// exits with code 7 when the test drops an "exit" file next to the install
// dir. It is skipped unless a test spawned it as the sidecar child.
func TestFakeSidecarChild(t *testing.T) {
	if os.Getenv("MIRRPANE_FAKE_SIDECAR") != "1" {
		t.Skip("helper process; spawned by the sidecar lifecycle tests")
	}
	dir := os.Getenv("MIRRPANE_FAKE_DIR")
	if dir == "" {
		os.Exit(9)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.Exit(8)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"idle","name":"fake","ip":"127.0.0.1","port":7000,`+
			`"targetWidth":1920,"targetHeight":1080,"maxFps":60,"decoderMode":"auto",`+
			`"colorRange":"auto","scaleFilter":"fast","audioBuffer":"balanced","volume":1}`)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()

	_ = json.NewEncoder(os.Stdout).Encode(struct {
		Event string `json:"event"`
		Port  int    `json:"port"`
	}{"api_ready", port})

	for {
		if _, err := os.Stat(filepath.Join(dir, "exit")); err == nil {
			os.Exit(7)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
