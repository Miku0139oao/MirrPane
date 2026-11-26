package main

import (
	"errors"
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miku0139/airplay-go/backend/internal/decode"
	"github.com/miku0139/airplay-go/backend/internal/frames"
	"github.com/miku0139/airplay-go/backend/internal/mirror"
)

// frameSink owns everything a decoded picture touches: the H.264 decoder, the
// shared-memory section and the optional capture file.
//
// The mirror stream calls Handle from its own goroutine while shutdown runs on
// the main goroutine. Freeing the FFmpeg decoder or unmapping the section while
// a frame is in flight is a use-after-free — an access violation that dumps
// registers, not a Go panic — so every use and the close are serialised here.
type frameSink struct {
	mu     sync.RWMutex
	closed bool

	dec  *decode.Decoder
	pub  *frames.Publisher
	rec  *streamRecorder
	gate *frames.ConsumerGate

	frames   atomic.Uint64
	width    atomic.Int64
	height   atomic.Int64
	hwMode   atomic.Bool
	haveMode atomic.Bool
	errRate  rateLimiter
	zeroCopy bool // most recently published picture, protected by mu

	// metrics is the rolling pipeline telemetry exposed by /api/state. It is
	// updated by Handle and read by Stats, so both take s.mu.
	metrics frameMetrics
}

// frameMetrics keeps the last frames.StatsWindow samples of the pipeline
// stages. Durations are milliseconds; fps is derived from the frame interval.
type frameMetrics struct {
	fps      frames.RollingStats
	decode   frames.RollingStats
	download frames.RollingStats
	convert  frames.RollingStats
	publish  frames.RollingStats

	lastFrame time.Time
	haveFrame bool
}

// record adds one decoded picture's timings to the windows.
func (m *frameMetrics) record(decodeTime time.Duration, timings decode.DecodeTimings, publishNanos int64) {
	now := time.Now()
	if m.haveFrame {
		m.fps.Add(float64(now.Sub(m.lastFrame)) / float64(time.Millisecond))
	}
	m.lastFrame = now
	m.haveFrame = true

	m.decode.Add(float64(decodeTime) / float64(time.Millisecond))
	m.download.Add(float64(timings.TransferNanos) / 1e6)
	m.convert.Add(float64(timings.ConvertNanos) / 1e6)
	m.publish.Add(float64(publishNanos) / 1e6)
}

// SinkStats is the aggregated pipeline telemetry in one snapshot.
type SinkStats struct {
	GPUZeroCopy     bool
	FPS             float64
	DecodeAvgMs     float64
	DecodeP95Ms     float64
	DownloadAvgMs   float64
	DownloadP95Ms   float64
	ConvertAvgMs    float64
	ConvertP95Ms    float64
	PublishAvgMs    float64
	PublishP95Ms    float64
	LagFrames       uint64
	ConsumerAgeMs   float64
	ConsumerAdopted uint64
	Published       uint64
	RawDropped      uint64
}

func newFrameSink(dec *decode.Decoder, pub *frames.Publisher, rec *streamRecorder) *frameSink {
	return &frameSink{dec: dec, pub: pub, rec: rec, gate: frames.DefaultConsumerGate()}
}

// Handle decodes one access unit and, when a consumer is rendering, publishes
// it. It reports whether a picture was produced.
//
// Every access unit is decoded: P and B frames are references, so skipping any
// of them would corrupt everything after. Only the GPU download, the colour
// conversion and the publish are gated on consumer liveness — that is the work
// that is wasted when the window is minimised or its raster thread has stalled.
func (s *frameSink) Handle(f *mirror.Frame) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if s.pub != nil {
		// Liveness includes access units that do not yield a picture, so a
		// decoder that is waiting for a keyframe is visible in the heartbeat.
		s.pub.Heartbeat()
	}

	s.rec.Write(f.Data, f.Valid)
	if !f.Valid {
		return false
	}

	// Decide before decoding so the decoder can skip the download and
	// conversion in the same call, without ever skipping the decode.
	live := s.pub != nil && s.gate.Allow(s.pub.ConsumerStats())

	decodeStart := time.Now()
	err := s.dec.ReceiveAccessUnit(f.Data, live)
	decodeTime := time.Since(decodeStart)
	if err != nil {
		if live {
			// The probe (or a full-rate picture) produced no frame: return the
			// slot in the probe schedule instead of wasting the interval.
			s.gate.Published(false)
		}
		s.refreshMode(false)
		if err != decode.ErrNoFrame {
			s.errRate.printf("decode: %v", err)
		}
		return false
	}

	width, height := s.dec.FrameSize()
	if width <= 0 || height <= 0 {
		if live {
			s.gate.Published(false)
		}
		return false
	}
	s.width.Store(int64(width))
	s.height.Store(int64(height))

	if !live {
		s.countPicture(width, height)
		s.refreshMode(true)
		return true
	}

	// Render straight into the published slot: no heap frame and no second copy
	// of the whole picture.
	claim, err := s.pub.Acquire(width, height)
	if err != nil {
		s.gate.Published(false)
		if !errors.Is(err, frames.ErrNoSlot) {
			s.errRate.printf("video: publish: %v", err)
		}
		s.refreshMode(true)
		return false
	}
	// Prefer GPU-only NV12 -> shared BGRA8888 texture conversion when D3D11VA
	// is active and the UI consumer accepts DXGI shared handles. If the GPU
	// video processor fails or the UI reports cross-adapter incompatibility,
	// fall straight through to CopyImageTo on the same decoded frame.
	if s.pub.ConsumerAllowsGPUShare() && s.dec.CanRenderGPU() {
		surf, gpuErr := s.dec.RenderFrameGPU(claim.Index())
		if gpuErr == nil {
			claim.CommitGPU(surf.SharedHandle, surf.AdapterLUID)
			s.zeroCopy = true
			s.gate.Published(true)
			s.countPicture(width, height)
			s.refreshMode(true)
			s.metrics.record(decodeTime, surf.Timings, s.pub.LastPublishNanos())
			return true
		}
		s.errRate.printf("video: gpu zero-copy unavailable (%v); falling back to CPU readback", gpuErr)
	}
	timings, err := s.dec.CopyImageTo(claim.Pixels(), claim.Stride())
	if err != nil {
		claim.Cancel()
		s.gate.Published(false)
		s.errRate.printf("video: convert: %v", err)
		s.refreshMode(true)
		return false
	}
	s.dec.ReleaseGPUSlot(claim.Index())
	claim.Commit()
	s.zeroCopy = false
	s.gate.Published(true)
	s.countPicture(width, height)
	// Conversion may have triggered a hardware fallback, so report the mode
	// only after the picture was produced.
	s.refreshMode(true)
	s.metrics.record(decodeTime, timings, s.pub.LastPublishNanos())
	return true
}

// countPicture updates the picture counter and logs the first frame.
func (s *frameSink) countPicture(width, height int) {
	if s.frames.Add(1) != 1 {
		return
	}
	mode := "software"
	if s.dec.Hardware() {
		if s.zeroCopy {
			mode = "D3D11VA (zero-copy)"
		} else {
			mode = "D3D11VA"
		}
	}
	log.Printf("decode: first picture %dx%d, %s (%d bytes per frame)",
		width, height, mode, width*height*4)
}

// refreshMode publishes the decoder mode for /api/state. Before the first
// decoded picture it stays empty; afterwards it follows fallbacks immediately.
func (s *frameSink) refreshMode(decoded bool) {
	if !decoded && !s.haveMode.Load() {
		return
	}
	s.hwMode.Store(s.dec.Hardware())
	s.haveMode.Store(true)
}

// Mode reports "D3D11VA", "software", or "" before the first decoded picture.
func (s *frameSink) Mode() string {
	if !s.haveMode.Load() {
		return ""
	}
	if s.hwMode.Load() {
		return "D3D11VA"
	}
	return "software"
}

// SnapshotPNG writes the most recent published picture to path as a PNG image.
// A GPU picture is read back on demand. The producer lock is released before
// PNG encoding and disk I/O.
func (s *frameSink) SnapshotPNG(path string) error {
	rgba, err := s.snapshotPicture()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, rgba); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (s *frameSink) snapshotPicture() (*image.RGBA, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.pub == nil {
		return nil, errors.New("no active decoder")
	}
	slot, format, ok := s.pub.LatestSlot()
	if !ok {
		return nil, errors.New("no picture available yet")
	}
	if format == frames.FormatDXGIBGRA {
		pic, err := s.dec.ReadbackSlotGPU(slot)
		if err != nil {
			return nil, err
		}
		return &image.RGBA{Pix: pic.Pixels, Stride: pic.Stride, Rect: image.Rect(0, 0, pic.Width, pic.Height)}, nil
	}
	pic, ok := s.pub.Latest()
	if !ok {
		return nil, errors.New("no picture available yet")
	}
	return &image.RGBA{Pix: pic.Pixels, Stride: pic.Stride, Rect: image.Rect(0, 0, pic.Width, pic.Height)}, nil
}

// Size reports the most recent picture size, or zero before the first frame.
func (s *frameSink) Size() (int, int) {
	return int(s.width.Load()), int(s.height.Load())
}

// Frames reports how many pictures have been produced.
func (s *frameSink) Frames() uint64 { return s.frames.Load() }

// Dropped reports pictures the writer could not publish because every slot was
// held. It is not the producer→consumer lag: use Stats().LagFrames for frames
// that were published but never adopted (expected while the window is hidden).
func (s *frameSink) Dropped() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.pub == nil {
		return 0
	}
	return s.pub.ConsumerStats().RawDropped
}

// Stats reports the rolling pipeline telemetry and the consumer's progress. It
// takes the same lock as Handle, so the numbers are a consistent snapshot.
func (s *frameSink) Stats() SinkStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := &s.metrics
	out := SinkStats{
		GPUZeroCopy:   s.zeroCopy,
		FPS:           m.fpsValue(),
		DecodeAvgMs:   m.decode.Mean(),
		DecodeP95Ms:   m.decode.P95(),
		DownloadAvgMs: m.download.Mean(),
		DownloadP95Ms: m.download.P95(),
		ConvertAvgMs:  m.convert.Mean(),
		ConvertP95Ms:  m.convert.P95(),
		PublishAvgMs:  m.publish.Mean(),
		PublishP95Ms:  m.publish.P95(),
	}
	if s.pub != nil {
		consumer := s.pub.ConsumerStats()
		out.LagFrames = consumer.LagFrames
		out.ConsumerAdopted = consumer.Adopted
		out.Published = consumer.Published
		out.RawDropped = consumer.RawDropped
		out.ConsumerAgeMs = float64(consumer.ConsumerAgeNanos) / 1e6
	}
	return out
}

// fpsValue converts the mean interval between pictures into frames per second.
func (m *frameMetrics) fpsValue() float64 {
	mean := m.fps.Mean()
	if mean <= 0 {
		return 0
	}
	return 1000 / mean
}

// Close waits for any in-flight frame to finish, then releases everything. A
// frame that arrives afterwards is dropped rather than touching freed memory.
func (s *frameSink) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.dec != nil {
		s.dec.Close()
	}
	if s.pub != nil {
		s.pub.Close()
	}
	if s.rec != nil {
		s.rec.Close()
	}
}

// rateLimiter prints a message only when it differs from the last one, so a
// corrupt stream that repeats one error every frame cannot flood the log.
type rateLimiter struct {
	mu   sync.Mutex
	last string
}

func (r *rateLimiter) printf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	r.mu.Lock()
	seen := r.last == msg
	r.last = msg
	r.mu.Unlock()
	if !seen {
		log.Print(msg)
	}
}
