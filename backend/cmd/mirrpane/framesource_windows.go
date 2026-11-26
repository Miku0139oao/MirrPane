//go:build windows

package main

import (
	"image"
	"sync"
	"time"

	"github.com/miku0139/airplay-go/backend/internal/framefeed"
)

// feedFrameSource reads the pictures the sidecar publishes into the named
// shared-memory section and hands the newest one to the UI without copying.
// The section only exists once airplayd runs, so opening it is retried until
// it appears.
//
// Lifetime: the picture Latest returns stays backed by its claimed slot until a
// later call adopts a different picture, which releases that slot in the same
// call that returns the replacement, or until Close. A sidecar restart that
// reuses the section does not release the slot (framefeed keeps it), so the
// picture on screen is never invalidated or re-uploaded for the same frame.
type feedFrameSource struct {
	mapping string

	mu          sync.Mutex
	reader      *framefeed.Reader
	lastAttempt time.Time
	lastSeq     uint64
	haveSeq     bool
	// dead is set when the section's layout changed incompatibly. The mapping
	// stays alive so the last picture remains readable; no new pictures come.
	dead bool
	// closed is set by Close, so a build that still runs after shutdown cannot
	// map the section again and start handing out pictures nobody will close.
	closed bool
	// img describes the slot held by reader. It is reused so presenting a
	// frame allocates nothing; the pixels stay the slot's.
	img image.RGBA
}

// newPlatformFrameSource returns the real shared-memory frame source.
func newPlatformFrameSource(mapping string) FrameSource {
	if mapping == "" {
		mapping = framefeed.DefaultMappingName
	}
	return &feedFrameSource{mapping: mapping}
}

// Latest returns only newly published pictures. When the producer has not
// published a newer picture it reports false, so the UI skips the GPU upload
// entirely instead of re-uploading an unchanged frame.
func (s *feedFrameSource) Latest() (*image.RGBA, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.dead {
		return nil, false
	}
	if s.reader == nil {
		if time.Since(s.lastAttempt) < 500*time.Millisecond {
			return nil, false
		}
		s.lastAttempt = time.Now()
		r, err := framefeed.Open(s.mapping)
		if err != nil {
			return nil, false
		}
		s.reader = r
		s.haveSeq = false
	}
	f, ok := s.reader.Latest()
	if !ok {
		if s.reader.Stale() {
			s.dead = true
		}
		return nil, false
	}
	if s.haveSeq && f.Seq == s.lastSeq {
		return nil, false
	}
	s.lastSeq, s.haveSeq = f.Seq, true
	s.img = image.RGBA{
		Pix:    f.Pixels,
		Stride: f.Stride,
		Rect:   image.Rect(0, 0, f.Width, f.Height),
	}
	return &s.img, true
}

// Close releases the mapped section and any held slot.
func (s *feedFrameSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.haveSeq = false
	if s.reader == nil {
		return nil
	}
	err := s.reader.Close()
	s.reader = nil
	return err
}
