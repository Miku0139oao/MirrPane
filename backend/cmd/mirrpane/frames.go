package main

import (
	"image"
	"math"
	"sync"
	"time"
)

// FrameSource supplies decoded RGBA frames for the video surface. Latest is
// called on the UI thread at display refresh, so implementations must be
// cheap: the real source only adopts a picture the sidecar already decoded,
// and never renders inside Latest.
//
// Lifetime: a picture handed out by Latest stays readable until a later Latest
// adopts a different picture or until Close. The UI presents it in the same
// callback it adopts it, so the bitmap it draws points into memory the source
// still owns.
type FrameSource interface {
	Latest() (img *image.RGBA, ok bool)
	Close() error
}

// SyntheticFrameSource generates a smooth moving gradient at up to 60 Hz. It
// double-buffers two *image.RGBA surfaces so ui.Bitmap.Update never races with
// pixel writes. It renders on the caller's goroutine, so it is meant for tests
// and non-Windows builds; the -demo mode uses DemoFrameSource instead.
type SyntheticFrameSource struct {
	mu      sync.Mutex
	w, h    int
	buffers [2]*image.RGBA
	nextBuf int
	started time.Time
	lastAt  time.Time
	closed  bool
	scratch gradientScratch
}

// NewSyntheticFrameSource returns a moving-gradient FrameSource at w x h.
func NewSyntheticFrameSource(w, h int) *SyntheticFrameSource {
	if w <= 0 {
		w = 960
	}
	if h <= 0 {
		h = 540
	}
	now := time.Now()
	return &SyntheticFrameSource{
		w: w,
		h: h,
		buffers: [2]*image.RGBA{
			image.NewRGBA(image.Rect(0, 0, w, h)),
			image.NewRGBA(image.Rect(0, 0, w, h)),
		},
		started: now,
	}
}

// Latest renders the next synthetic gradient frame into the spare buffer and
// returns it, at most once per 15 ms. The other buffer, which is the picture
// the UI last took, is not touched until the call after this one, so the
// picture on screen stays readable.
func (s *SyntheticFrameSource) Latest() (*image.RGBA, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, false
	}
	now := time.Now()
	if !s.lastAt.IsZero() && now.Sub(s.lastAt) < 15*time.Millisecond {
		return nil, false
	}
	s.lastAt = now

	buf := s.buffers[s.nextBuf]
	s.nextBuf = 1 - s.nextBuf
	s.scratch.render(buf, now.Sub(s.started).Seconds())
	return buf, true
}

// Close implements FrameSource; there is nothing to release, but a build that
// runs after shutdown must not present another picture.
func (s *SyntheticFrameSource) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

// DemoFrameSource renders the synthetic test pattern on its own goroutine so
// the UI thread only ever takes a finished picture. Three buffers let the
// renderer stay one frame ahead of the UI: it never writes the picture being
// shown nor the one waiting to be shown, so presentation can reach the display
// rate even though rendering is the expensive part.
type DemoFrameSource struct {
	mu        sync.Mutex
	w, h      int
	buffers   [3]*image.RGBA
	latest    int  // newest finished picture, -1 before the first
	inHand    bool // the UI holds (and is showing) buffers[latest]
	displayed int  // buffer the UI last took, -1 before the first
	closed    bool // Close finished; no more pictures are handed out
	started   time.Time
	stop      chan struct{}
	done      chan struct{}
	once      sync.Once
	scratch   gradientScratch
}

// NewDemoFrameSource starts a background gradient renderer at w x h.
func NewDemoFrameSource(w, h int) *DemoFrameSource {
	if w <= 0 {
		w = 2560
	}
	if h <= 0 {
		h = 1178
	}
	d := &DemoFrameSource{
		w:         w,
		h:         h,
		latest:    -1,
		displayed: -1,
		inHand:    true,
		started:   time.Now(),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	for i := range d.buffers {
		d.buffers[i] = image.NewRGBA(image.Rect(0, 0, w, h))
	}
	go d.loop()
	return d
}

func (d *DemoFrameSource) loop() {
	defer close(d.done)
	ticker := time.NewTicker(16 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-ticker.C:
		}
		d.mu.Lock()
		write := -1
		for i := 0; i < len(d.buffers); i++ {
			if i != d.latest && i != d.displayed {
				write = i
				break
			}
		}
		d.mu.Unlock()
		if write < 0 {
			// Every buffer is either on screen or waiting to be taken.
			continue
		}

		d.scratch.render(d.buffers[write], time.Since(d.started).Seconds())

		d.mu.Lock()
		d.latest = write
		d.inHand = false
		d.mu.Unlock()
	}
}

// Latest takes the newest finished picture, or reports false when the UI
// already holds it or nothing has been rendered yet. The buffer it hands out
// is never written again before the UI takes a different one.
func (d *DemoFrameSource) Latest() (*image.RGBA, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.latest < 0 || d.inHand {
		return nil, false
	}
	d.inHand = true
	d.displayed = d.latest
	return d.buffers[d.latest], true
}

// Close stops the renderer and waits for any in-flight frame to finish, then
// stops handing out pictures.
func (d *DemoFrameSource) Close() error {
	d.once.Do(func() { close(d.stop) })
	<-d.done
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	return nil
}

// gradientScratch is the per-column and per-row lookup tables of the shared
// test pattern, kept between frames so 60 Hz presentation allocates nothing.
type gradientScratch struct {
	uIdx                []uint8
	rBase, gBase, bBase []int32
}

// render fills buf with the shared test pattern: a moving warm amber/slate
// gradient with a traveling highlight band. Per-column and per-row
// contributions are precomputed as integer LUTs so the pixel loop stays cheap
// even at 2560x1178 (the demo must not be the bottleneck when verifying 60 Hz
// presentation).
func (s *gradientScratch) render(buf *image.RGBA, t float64) {
	w, h, stride := buf.Rect.Dx(), buf.Rect.Dy(), buf.Stride
	waveX := math.Sin(t * 1.1)
	waveY := math.Cos(t * 0.8)
	sweep := math.Mod(t*0.28, 1.0)

	if cap(s.uIdx) < w {
		s.uIdx = make([]uint8, w)
	} else {
		s.uIdx = s.uIdx[:w]
	}
	uIdx := s.uIdx
	for x := 0; x < w; x++ {
		nx := float64(x) / float64(w)
		u := 0.5 + 0.5*math.Sin(2.4*nx+t*0.9+waveY*0.4)
		uIdx[x] = uint8(u*255 + 0.5)
	}
	var rU, gU, bU [256]int32
	for i := 0; i < 256; i++ {
		u := float64(i) / 255
		rU[i] = int32(112.5 * u)
		gU[i] = int32(73.6 * u)
		bU[i] = int32(-38.0 * u)
	}
	if cap(s.rBase) < h {
		s.rBase = make([]int32, h)
		s.gBase = make([]int32, h)
		s.bBase = make([]int32, h)
	} else {
		s.rBase = s.rBase[:h]
		s.gBase = s.gBase[:h]
		s.bBase = s.bBase[:h]
	}
	rBase, gBase, bBase := s.rBase, s.gBase, s.bBase
	for y := 0; y < h; y++ {
		ny := float64(y) / float64(h)
		v := 0.5 + 0.5*math.Cos(2.0*ny-t*0.7+waveX*0.4)
		dy := ny - sweep
		band := math.Exp(-dy*dy*90.0) * 34.0
		rBase[y] = int32(26 + 45*v + band)
		gBase[y] = int32(24 + 42*(1-v) + band*0.75)
		bBase[y] = int32(30 + 115*v*0.65 + band*0.4)
	}

	for y := 0; y < h; y++ {
		row := buf.Pix[y*stride : y*stride+w*4]
		rb, gb, bb := rBase[y], gBase[y], bBase[y]
		gridRow := y%64 == 0
		for x := 0; x < w; x++ {
			i := uIdx[x]
			r, g, b := rb+rU[i], gb+gU[i], bb+bU[i]
			if gridRow || x%64 == 0 {
				// Subtle grid lines every 64 pixels to verify sharp scaling.
				r, g, b = r+10, g+10, b+10
			}
			if r < 0 {
				r = 0
			} else if r > 255 {
				r = 255
			}
			if g < 0 {
				g = 0
			} else if g > 255 {
				g = 255
			}
			if b < 0 {
				b = 0
			} else if b > 255 {
				b = 255
			}
			o := x * 4
			row[o] = uint8(r)
			row[o+1] = uint8(g)
			row[o+2] = uint8(b)
			row[o+3] = 255
		}
	}
}
