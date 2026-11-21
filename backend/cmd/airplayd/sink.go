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

	s.rec.Write(f.Data, f.Valid)
	if !f.Valid {
		return false
	}

	img, err := s.dec.DecodeAccessUnit(f.Data)
	if err != nil {
		if err != decode.ErrNoFrame {
			s.errRate.printf("decode: %v", err)
		}
		return false
	}

	if err := s.pub.Publish(img.Width, img.Height, img.Stride, img.Pixels); err != nil {
		s.errRate.printf("video: publish: %v", err)
	}

	s.width.Store(int64(img.Width))
	s.height.Store(int64(img.Height))
	if s.frames.Add(1) == 1 {
		hw := s.dec.Hardware()
		s.hwMode.Store(hw)
		s.haveMode.Store(true)
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

// Dropped reports pictures the consumer did not take because every slot was
// busy.
func (s *frameSink) Dropped() uint64 {
	if s.pub == nil {
		return 0
	}
	return s.pub.Dropped()
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
