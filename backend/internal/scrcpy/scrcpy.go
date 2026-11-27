package scrcpy

import (
	"bufio"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Miku0139oao/MirrPane/backend/internal/adb"
)

//go:embed server/scrcpy-server-v5.0.1.jar
var ServerJar []byte

// ServerVersion is the pinned scrcpy server protocol version.
const ServerVersion = "5.0.1"

const (
	deviceServerPath = "/data/local/tmp/scrcpy-server.jar"
	socketPrefix     = "scrcpy_"
)

// Options configures a scrcpy v5.0.1 mirroring session.
type Options struct {
	Serial   string // adb serial from Devices
	MaxSize  int    // 0 = device native
	MaxFPS   int    // 0 = unlimited
	BitRate  int    // bits/s, 0 = scrcpy default
	Audio    bool   // request raw PCM audio (48 kHz stereo s16le)
	LogLevel string // "" = info
	Logf     func(string, ...any)
}

// VideoPacket represents one H.264 Annex-B packet emitted by the scrcpy server.
type VideoPacket struct {
	Data     []byte // H.264 access unit, Annex-B, exactly as the server sent it
	PTS      uint64 // microseconds, as parsed from the packet header
	Config   bool   // SPS/PPS config packet
	Keyframe bool
}

// Info describes the connected Android device and active video stream.
type Info struct {
	DeviceName    string
	Codec         string // "h264"
	Width, Height int
}

// AudioInfo describes the audio stream when audio was requested. scrcpy's raw
// mode delivers 48 kHz stereo signed 16-bit little-endian PCM, which the
// receiver writes to waveOut without decoding.
type AudioInfo struct {
	Codec      string // "raw"
	SampleRate int    // 48000
	Channels   int    // 2
}

// AudioPacket is one chunk of the audio stream: raw PCM or encoded frames.
type AudioPacket struct {
	Data   []byte // PCM s16le when Info().Audio.Codec == "raw"
	PTS    uint64 // microseconds
	Config bool   // codec configuration packet (encoded codecs only)
}

// Session manages an active scrcpy v5.0.1 video + control session.
type Session struct {
	adbClient *adb.Client
	serial    string
	port      int

	videoConn   net.Conn
	videoReader *bufio.Reader
	controlConn net.Conn
	controller  *Controller

	audioConn   net.Conn
	audioReader *bufio.Reader
	audioInfo   AudioInfo
	audioOK     bool

	infoMu sync.RWMutex
	info   Info

	procCancel context.CancelFunc
	procDone   chan struct{}
	procErr    error

	drainDone chan struct{}

	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

// Start pushes the embedded scrcpy-server jar, establishes an adb forward tunnel,
// launches app_process, opens video and control sockets in scrcpy v5.0.1 order,
// and completes the initial handshake (dummy byte, device name, codec & session meta).
func Start(ctx context.Context, client *adb.Client, opt Options) (*Session, error) {
	if client == nil {
		return nil, errors.New("scrcpy: nil adb client")
	}
	if strings.TrimSpace(opt.Serial) == "" {
		return nil, errors.New("scrcpy: empty device serial")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	logLevel, err := normalizeLogLevel(opt.LogLevel)
	if err != nil {
		return nil, err
	}

	scidHex, err := generateSCID()
	if err != nil {
		return nil, err
	}

	if err := pushServerJar(ctx, client, opt.Serial); err != nil {
		return nil, fmt.Errorf("scrcpy: push server jar: %w", err)
	}

	removeOldForwards(ctx, client, opt.Serial)

	remoteSocket := "localabstract:" + socketPrefix + scidHex
	port, err := client.Forward(ctx, opt.Serial, "tcp:0", remoteSocket)
	if err != nil {
		return nil, fmt.Errorf("scrcpy: forward %s: %w", remoteSocket, err)
	}

	var (
		videoConn   net.Conn
		audioConn   net.Conn
		controlConn net.Conn
		procCancel  context.CancelFunc
		procDone    chan struct{}
		logBuf      = newLogRing(30)
	)

	cleanup := func() {
		if videoConn != nil {
			_ = videoConn.Close()
		}
		if audioConn != nil {
			_ = audioConn.Close()
		}
		if controlConn != nil {
			_ = controlConn.Close()
		}
		if procCancel != nil {
			procCancel()
		}
		if procDone != nil {
			select {
			case <-procDone:
			case <-time.After(500 * time.Millisecond):
			}
		}
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = client.ForwardRemove(rmCtx, opt.Serial, fmt.Sprintf("tcp:%d", port))
		rmCancel()
	}

	fail := func(err error) error {
		cleanup()
		if tail := logBuf.Tail(); tail != "" {
			return fmt.Errorf("%w\nserver output:\n%s", err, tail)
		}
		return err
	}

	procCtx, cancel := context.WithCancel(context.Background())
	procCancel = cancel

	args := buildServerArgs(opt, scidHex, logLevel)
	cmd := client.Command(procCtx, args...)
	logWriter := &serverLogWriter{ring: logBuf, logf: opt.Logf}
	cmd.Stdout = logWriter
	cmd.Stderr = logWriter

	if err := cmd.Start(); err != nil {
		return nil, fail(fmt.Errorf("scrcpy: start server: %w", err))
	}

	procDone = make(chan struct{})
	var procErr error
	go func() {
		procErr = cmd.Wait()
		logWriter.Flush()
		close(procDone)
	}()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	videoConn, err = connectVideoWithRetry(ctx, addr, procDone, func() error { return procErr })
	if err != nil {
		return nil, fail(fmt.Errorf("scrcpy: connect video socket: %w", err))
	}

	// The server accepts one socket per stream in this order: video, audio,
	// control, and only starts the streams once all are connected. Only the
	// first connection carries the dummy byte.
	var (
		audioReader *bufio.Reader
		audioInfo   AudioInfo
	)
	if opt.Audio {
		audioConn, err = (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fail(fmt.Errorf("scrcpy: connect audio socket: %w", err))
		}
		audioReader = bufio.NewReaderSize(audioConn, 64*1024)
	}

	controlConn, err = (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fail(fmt.Errorf("scrcpy: connect control socket: %w", err))
	}
	if tcpConn, ok := controlConn.(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
	}

	info, videoReader, err := readHandshake(ctx, videoConn)
	if err != nil {
		return nil, fail(fmt.Errorf("scrcpy: handshake: %w", err))
	}

	if audioReader != nil {
		_ = audioConn.SetReadDeadline(time.Now().Add(10 * time.Second))
		codec, err := readAudioHeader(audioReader)
		_ = audioConn.SetReadDeadline(time.Time{})
		if err != nil {
			return nil, fail(fmt.Errorf("scrcpy: audio header: %w", err))
		}
		if codec == "" {
			// The device could not capture audio (Android 10 and older). Video
			// continues; the receiver just has no sound.
			_ = audioConn.Close()
			audioConn, audioReader = nil, nil
		} else if codec != "raw" {
			return nil, fail(fmt.Errorf("scrcpy: unexpected audio codec %q (raw was requested)", codec))
		} else {
			audioInfo = AudioInfo{Codec: codec, SampleRate: 48000, Channels: 2}
		}
	}

	sess := &Session{
		adbClient:   client,
		serial:      opt.Serial,
		port:        port,
		videoConn:   videoConn,
		videoReader: videoReader,
		controlConn: controlConn,
		controller:  newController(controlConn),
		audioConn:   audioConn,
		audioReader: audioReader,
		audioInfo:   audioInfo,
		audioOK:     audioReader != nil,
		info:        info,
		procCancel:  procCancel,
		procDone:    procDone,
		drainDone:   make(chan struct{}),
	}

	go sess.drainControl(opt.Logf)

	return sess, nil
}

func newSessionFromConns(videoConn, controlConn net.Conn, info Info) *Session {
	sess := &Session{
		videoConn:   videoConn,
		videoReader: bufio.NewReaderSize(videoConn, 64*1024),
		controlConn: controlConn,
		controller:  newController(controlConn),
		info:        info,
		drainDone:   make(chan struct{}),
	}
	if controlConn != nil {
		go sess.drainControl(nil)
	} else {
		close(sess.drainDone)
	}
	return sess
}

func (s *Session) drainControl(logf func(string, ...any)) {
	defer close(s.drainDone)
	if err := drainDeviceMessages(s.controlConn); err != nil && !s.closed.Load() {
		if logf != nil {
			logf("scrcpy: control reader stopped: %v", err)
		}
	}
}

// Info returns the connected device name, codec, and current video dimensions.
func (s *Session) Info() Info {
	s.infoMu.RLock()
	defer s.infoMu.RUnlock()
	return s.info
}

// AudioInfo reports the audio stream's format; ok is false when audio was not
// requested or the device could not capture it.
func (s *Session) AudioInfo() (AudioInfo, bool) {
	return s.audioInfo, s.audioOK
}

// ReadAudio returns the next audio packet. Raw mode packets are PCM s16le,
// 48 kHz stereo, length-aligned to whole samples. It returns io.EOF when the
// session has no audio or the stream ends.
func (s *Session) ReadAudio(ctx context.Context) (AudioPacket, error) {
	if !s.audioOK || s.audioReader == nil || s.closed.Load() {
		return AudioPacket{}, io.EOF
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return AudioPacket{}, err
		}
	}

	done := make(chan struct{})
	defer close(done)
	if ctx != nil && ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = s.audioConn.SetReadDeadline(time.Now())
			case <-done:
			}
		}()
	}

	pkt, err := readAudioPacket(s.audioReader)
	if err != nil {
		if s.closed.Load() || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
			return AudioPacket{}, io.EOF
		}
		if ctx != nil && ctx.Err() != nil {
			return AudioPacket{}, ctx.Err()
		}
		return AudioPacket{}, err
	}
	return pkt, nil
}

// Control returns the thread-safe control message writer for this session.
func (s *Session) Control() *Controller {
	return s.controller
}

// ReadVideo reads the next H.264 packet (config or frame) from the video stream.
// Mid-stream session metadata headers update Session.Info() transparently.
// Returns io.EOF when the stream ends or the session is closed.
func (s *Session) ReadVideo(ctx context.Context) (VideoPacket, error) {
	if s.closed.Load() {
		return VideoPacket{}, io.EOF
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return VideoPacket{}, err
		}
	}

	done := make(chan struct{})
	defer close(done)

	if ctx != nil && ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = s.videoConn.SetReadDeadline(time.Now())
			case <-done:
			}
		}()
	}

	pkt, err := readVideoPacket(s.videoReader, func(w, h int) {
		s.infoMu.Lock()
		s.info.Width = w
		s.info.Height = h
		s.infoMu.Unlock()
	})
	if err != nil {
		if s.closed.Load() || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
			return VideoPacket{}, io.EOF
		}
		if ctx != nil && ctx.Err() != nil {
			return VideoPacket{}, ctx.Err()
		}
		return VideoPacket{}, err
	}
	return pkt, nil
}

// Close closes the video and control sockets, removes the adb tunnel, and waits
// briefly for the server process to terminate. It is idempotent and safe to call
// concurrently with ReadVideo.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		if s.controller != nil {
			s.controller.close()
		}

		var errs []error
		if s.videoConn != nil {
			if err := s.videoConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				errs = append(errs, err)
			}
		}
		if s.audioConn != nil {
			if err := s.audioConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				errs = append(errs, err)
			}
		}
		if s.controlConn != nil {
			if err := s.controlConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				errs = append(errs, err)
			}
		}

		if s.drainDone != nil {
			<-s.drainDone
		}

		if s.adbClient != nil && s.port > 0 && s.serial != "" {
			rmCtx, rmCancel := context.WithTimeout(context.Background(), 3*time.Second)
			if err := s.adbClient.ForwardRemove(rmCtx, s.serial, fmt.Sprintf("tcp:%d", s.port)); err != nil {
				// Do not treat missing forward as fatal if already torn down.
				if !strings.Contains(strings.ToLower(err.Error()), "not found") {
					errs = append(errs, err)
				}
			}
			rmCancel()
		}

		if s.procDone != nil {
			select {
			case <-s.procDone:
			case <-time.After(750 * time.Millisecond):
				if s.procCancel != nil {
					s.procCancel()
				}
				select {
				case <-s.procDone:
				case <-time.After(500 * time.Millisecond):
				}
			}
		} else if s.procCancel != nil {
			s.procCancel()
		}

		s.closeErr = errors.Join(errs...)
	})
	return s.closeErr
}

func normalizeLogLevel(level string) (string, error) {
	level = strings.ToLower(strings.TrimSpace(level))
	if level == "" {
		return "info", nil
	}
	switch level {
	case "verbose", "debug", "info", "warn", "error":
		return level, nil
	default:
		return "", fmt.Errorf("scrcpy: unsupported log level %q", level)
	}
}

func generateSCID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("scrcpy: generate scid: %w", err)
	}
	// 31-bit non-negative integer, matching scrcpy.c.
	scid := binary.BigEndian.Uint32(b[:]) & 0x7fffffff
	return fmt.Sprintf("%08x", scid), nil
}

func pushServerJar(ctx context.Context, client *adb.Client, serial string) error {
	if len(ServerJar) == 0 {
		return errors.New("embedded server jar is empty")
	}
	f, err := os.CreateTemp("", "scrcpy-server-*.jar")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	defer os.Remove(tmpPath)

	if _, err := f.Write(ServerJar); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return client.Push(ctx, serial, tmpPath, deviceServerPath)
}

func removeOldForwards(ctx context.Context, client *adb.Client, serial string) {
	entries, err := client.ForwardList(ctx, serial)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Remote, "localabstract:"+socketPrefix) {
			_ = client.ForwardRemove(ctx, serial, entry.Local)
		}
	}
}

func buildServerArgs(opt Options, scidHex, logLevel string) []string {
	args := []string{
		"-s", opt.Serial,
		"shell",
		"CLASSPATH=" + deviceServerPath,
		"app_process",
		"/",
		"com.genymobile.scrcpy.Server",
		ServerVersion,
		"scid=" + scidHex,
		"log_level=" + logLevel,
		"video=true",
		"video_codec=h264",
		"audio=" + strconv.FormatBool(opt.Audio),
		"control=true",
		"tunnel_forward=true",
	}
	if opt.Audio {
		// Raw PCM keeps the receiver free of a second audio decoder: scrcpy
		// sends 48 kHz stereo s16le, the format waveOut already plays.
		args = append(args, "audio_codec=raw")
	}
	if opt.MaxSize > 0 {
		args = append(args, fmt.Sprintf("max_size=%d", opt.MaxSize))
	}
	if opt.MaxFPS > 0 {
		args = append(args, fmt.Sprintf("max_fps=%d", opt.MaxFPS))
	}
	if opt.BitRate > 0 {
		args = append(args, fmt.Sprintf("video_bit_rate=%d", opt.BitRate))
	}
	return args
}

func connectVideoWithRetry(ctx context.Context, addr string, procDone <-chan struct{}, getProcErr func() error) (net.Conn, error) {
	const (
		maxAttempts = 100
		retryDelay  = 100 * time.Millisecond
	)

	dialer := net.Dialer{Timeout: 2 * time.Second}
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return nil, fmt.Errorf("%w (last error: %v)", ctx.Err(), lastErr)
			}
			return nil, ctx.Err()
		case <-procDone:
			return nil, fmt.Errorf("server exited before accepting connection (%v)", getProcErr())
		default:
		}

		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			// Wait for the server's dummy byte (with a bounded read deadline so
			// we detect a dead adb forward socket or cancelled ctx promptly).
			deadline := time.Now().Add(2 * time.Second)
			if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
				deadline = ctxDeadline
			}
			_ = conn.SetReadDeadline(deadline)
			err = readDummyByte(conn)
			_ = conn.SetReadDeadline(time.Time{})
			if err == nil {
				return conn, nil
			}
			_ = conn.Close()
		}
		lastErr = err

		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("%w (last error: %v)", ctx.Err(), lastErr)
		case <-procDone:
			timer.Stop()
			return nil, fmt.Errorf("server exited before accepting connection (%v)", getProcErr())
		case <-timer.C:
		}
	}

	return nil, fmt.Errorf("timed out waiting for server dummy byte: %w", lastErr)
}

func readHandshake(ctx context.Context, videoConn net.Conn) (Info, *bufio.Reader, error) {
	done := make(chan struct{})
	defer close(done)

	if ctx != nil && ctx.Done() != nil {
		if deadline, ok := ctx.Deadline(); ok {
			_ = videoConn.SetReadDeadline(deadline)
			defer func() { _ = videoConn.SetReadDeadline(time.Time{}) }()
		}
		go func() {
			select {
			case <-ctx.Done():
				_ = videoConn.SetReadDeadline(time.Now())
			case <-done:
			}
		}()
	}

	r := bufio.NewReaderSize(videoConn, 64*1024)

	name, err := readDeviceName(r)
	if err != nil {
		return Info{}, nil, err
	}
	codec, err := readCodecID(r)
	if err != nil {
		return Info{}, nil, err
	}
	w, h, err := readInitialSessionMeta(r)
	if err != nil {
		return Info{}, nil, err
	}

	return Info{
		DeviceName: name,
		Codec:      codec,
		Width:      w,
		Height:     h,
	}, r, nil
}

type logRing struct {
	mu    sync.Mutex
	max   int
	lines []string
}

func newLogRing(maxLines int) *logRing {
	return &logRing{max: maxLines}
}

func (r *logRing) Add(line string) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.lines) >= r.max {
		copy(r.lines, r.lines[1:])
		r.lines[len(r.lines)-1] = line
		return
	}
	r.lines = append(r.lines, line)
}

func (r *logRing) Tail() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

type serverLogWriter struct {
	mu   sync.Mutex
	ring *logRing
	logf func(string, ...any)
	buf  string
}

func (w *serverLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf += string(p)
	for {
		idx := strings.IndexByte(w.buf, '\n')
		if idx < 0 {
			break
		}
		line := strings.TrimRight(w.buf[:idx], "\r")
		w.buf = w.buf[idx+1:]
		if line != "" {
			w.ring.Add(line)
			if w.logf != nil {
				w.logf("scrcpy-server: %s", line)
			}
		}
	}
	return len(p), nil
}

func (w *serverLogWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	line := strings.TrimRight(w.buf, "\r\n")
	w.buf = ""
	if line != "" {
		w.ring.Add(line)
		if w.logf != nil {
			w.logf("scrcpy-server: %s", line)
		}
	}
}

// Ensure exec.Cmd is only referenced cleanly; unused import guard if needed.
var _ = (*exec.Cmd)(nil)
