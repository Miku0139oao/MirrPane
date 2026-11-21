package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// streamRecorder appends every decrypted access unit to video.h264 so the
// stream can be replayed offline with ffprobe/ffmpeg. This is the replay
// harness from the plan: it lets the decoder be developed and regression-tested
// without an iPhone attached.
type streamRecorder struct {
	f      *os.File
	path   string
	files  uint64
	bytes  uint64
	failed bool
}

// newStreamRecorder prepares dir/video.h264, replacing any earlier capture.
func newStreamRecorder(dir string) (*streamRecorder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("record: %w", err)
	}
	path := filepath.Join(dir, "video.h264")
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("record: %w", err)
	}
	log.Printf("record: capturing to %s", path)
	return &streamRecorder{f: f, path: path}, nil
}

// Write appends one access unit. Frames that failed their integrity checks are
// skipped: writing noise would make the capture look corrupt in a different way
// than it really is.
func (r *streamRecorder) Write(data []byte, valid bool) {
	if r == nil || r.f == nil || r.failed || !valid || len(data) == 0 {
		return
	}
	n, err := r.f.Write(data)
	if err != nil {
		log.Printf("record: write %s: %v (capture stopped)", r.path, err)
		r.failed = true
		return
	}
	r.files++
	r.bytes += uint64(n)
}

// Close flushes the capture and reports what was written.
func (r *streamRecorder) Close() {
	if r == nil || r.f == nil {
		return
	}
	if err := r.f.Close(); err != nil {
		log.Printf("record: close %s: %v", r.path, err)
		return
	}
	log.Printf("record: wrote %d frames, %.1f MiB to %s", r.files, float64(r.bytes)/(1<<20), r.path)
}
