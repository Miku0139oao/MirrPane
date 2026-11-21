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

	dec *decode.Decoder
	pub *frames.Publisher
	rec *streamRecorder

	frames   atomic.Uint64
	width    atomic.Int64
	height   atomic.Int64
	hwMode   atomic.Bool
	haveMode atomic.Bool
	errRate  rateLimiter

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
	return &frameSink{dec: dec, pub: pub, rec: rec}
}

// Handle decodes one access unit and publishes it. It reports whether a picture
// was produced.
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

	decodeStart := time.Now()
	img, err := s.dec.DecodeAccessUnit(f.Data)
	decodeTime := time.Since(decodeStart)
	// Recovery can change modes on a failed access unit, before the next
	// keyframe arrives. Keep diagnostics current while waiting for it.
	hw := s.dec.Hardware()
	if err == nil || s.haveMode.Load() {
		s.hwMode.Store(hw)
		s.haveMode.Store(true)
	}
	if err != nil {
		if err != decode.ErrNoFrame {
			s.errRate.printf("decode: %v", err)
		}
		return false
	}

	publishErr := s.pub.Publish(img.Width, img.Height, img.Stride, img.Pixels)
	if publishErr != nil {
		s.errRate.printf("video: publish: %v", publishErr)
	}
	s.metrics.record(decodeTime, img.Timings, s.pub.LastPublishNanos())

	s.width.Store(int64(img.Width))
	s.height.Store(int64(img.Height))
	if s.frames.Add(1) == 1 {
		mode := "software"
		if hw {
			mode = "D3D11VA"
		}
		log.Printf("decode: first picture %dx%d, %s (%d bytes per frame)",
			img.Width, img.Height, mode, len(img.Pixels))
	}
	return true
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

// SnapshotPNG writes the most recent decoded picture to path as a PNG image.
func (s *frameSink) SnapshotPNG(path string) error {
	s.mu.Lock()
	if s.closed || s.dec == nil {
		s.mu.Unlock()
		return errors.New("no active decoder")
	}
	img := s.dec.LastImage()
	if img == nil {
		s.mu.Unlock()
		return errors.New("no picture available yet")
	}
	rgba := &image.RGBA{
		Pix:    append([]byte(nil), img.Pixels...),
		Stride: img.Stride,
		Rect:   image.Rect(0, 0, img.Width, img.Height),
	}
	s.mu.Unlock()

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

// Size reports the most recent picture size, or zero before the first frame.
func (s *frameSink) Size() (int, int) {
	return int(s.width.Load()), int(s.height.Load())
}

// Frames reports how many pictures have been produced.
func (s *frameSink) Frames() uint64 { return s.frames.Load() }

// Dropped reports pictures the UI never adopted: frames the producer announced
// minus frames the reader took. The old claim-contention counter could not fire
// with three slots and a one-slot reader, so this is the honest replacement.
func (s *frameSink) Dropped() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.pub == nil {
		return 0
	}
	return s.pub.ConsumerStats().LagFrames
}

// Stats reports the rolling pipeline telemetry and the consumer's progress. It
// takes the same lock as Handle, so the numbers are a consistent snapshot.
func (s *frameSink) Stats() SinkStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := &s.metrics
	out := SinkStats{
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
