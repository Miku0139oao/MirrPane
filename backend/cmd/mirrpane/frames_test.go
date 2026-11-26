package main

import (
	"image"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

func solidRGBA(w, h int, v byte) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = v
	}
	return img
}

// newVideoTestApp is the smallest app the presentation methods need.
func newVideoTestApp(quality string) *mirrpaneApp {
	prefs := DefaultUIPrefs()
	prefs.FilterQuality = quality
	return &mirrpaneApp{
		bitmap: ui.NewBitmap(opaqueBlank()),
		prefs:  prefs,
	}
}

func TestVideoSamplingBothDirections(t *testing.T) {
	a := newVideoTestApp("medium")
	first := solidRGBA(64, 32, 0x40)
	a.presentVideoFrame(first)
	if a.videoPicture != first {
		t.Fatal("presentVideoFrame did not remember the picture")
	}
	if w, h := a.bitmap.Size(); w != 64 || h != 32 {
		t.Fatalf("bitmap size = %dx%d, want 64x32", w, h)
	}
	if a.videoSharp {
		t.Fatal("a smooth preference made a nearest-neighbour bitmap")
	}

	// Switching to sharp rebuilds from the held picture, not from a blank.
	a.prefs.FilterQuality = "none"
	if !a.applySamplingMode() {
		t.Fatal("switching to sharp did not replace the bitmap")
	}
	if !a.videoSharp {
		t.Fatal("the sharp bitmap samples smoothly")
	}
	if w, h := a.bitmap.Size(); w != 64 || h != 32 {
		t.Fatalf("sharp bitmap size = %dx%d, want 64x32", w, h)
	}

	// No change, no rebuild.
	sharpBitmap := a.bitmap
	if a.applySamplingMode() {
		t.Fatal("the sampler rebuilt without a change")
	}
	if a.bitmap != sharpBitmap {
		t.Fatal("the sampler replaced the bitmap without a change")
	}

	// And back to smooth: the direction the old one-way Pixelated could not do.
	a.prefs.FilterQuality = "high"
	if !a.applySamplingMode() {
		t.Fatal("switching back to smooth did not replace the bitmap")
	}
	if a.videoSharp {
		t.Fatal("the smooth bitmap still samples nearest-neighbour")
	}
	if w, h := a.bitmap.Size(); w != 64 || h != 32 {
		t.Fatalf("smooth bitmap size = %dx%d, want 64x32", w, h)
	}

	// A new picture while sharp: presentVideoFrame moves the rebuilt bitmap to
	// it without a second upload, and keeps sampling sharp.
	a.prefs.FilterQuality = "none"
	a.applySamplingMode()
	second := solidRGBA(80, 40, 0x80)
	a.presentVideoFrame(second)
	if a.videoPicture != second {
		t.Fatal("presentVideoFrame did not adopt the new picture")
	}
	if w, h := a.bitmap.Size(); w != 80 || h != 40 {
		t.Fatalf("bitmap size after a new picture = %dx%d, want 80x40", w, h)
	}
	if !a.videoSharp {
		t.Fatal("presenting a picture dropped nearest-neighbour sampling")
	}
}

func TestVideoSamplingBeforeFirstPicture(t *testing.T) {
	a := newVideoTestApp("none")
	// The blank opening picture cannot be un-pixelated, and needs nothing.
	a.applySamplingMode()
	if !a.videoSharp {
		t.Fatal("sharp mode was not recorded before the first picture")
	}
	img := solidRGBA(32, 16, 0x20)
	bitmap := a.bitmap
	a.presentVideoFrame(img)
	if a.bitmap != bitmap {
		t.Fatal("presenting the first picture replaced the already sharp bitmap")
	}
	if !a.videoSharp {
		t.Fatal("the first picture dropped nearest-neighbour sampling")
	}
	if w, h := a.bitmap.Size(); w != 32 || h != 16 {
		t.Fatalf("bitmap size = %dx%d, want 32x16", w, h)
	}
}

func TestClearVideoPictureShowsOpaqueBlank(t *testing.T) {
	a := newVideoTestApp("none")
	a.presentVideoFrame(solidRGBA(48, 24, 0x11))
	a.clearVideoPicture()
	if a.videoPicture != nil {
		t.Fatal("clearVideoPicture kept the picture")
	}
	if w, h := a.bitmap.Size(); w != 1 || h != 1 {
		t.Fatalf("blank bitmap size = %dx%d, want 1x1", w, h)
	}
	// Nothing of the old frame may come back through a later switch.
	a.prefs.FilterQuality = "medium"
	if a.applySamplingMode() {
		t.Fatal("a sampling switch revived a cleared picture")
	}
	if _, ok := a.videoPicture.(*image.RGBA); ok {
		t.Fatal("videoPicture is set after clearVideoPicture")
	}
}

// TestFrameSourcesStopAtClose checks a build that still runs after shutdown
// cannot take another picture: presenting after Close would read buffers the
// app has already let go.
func TestFrameSourcesStopAtClose(t *testing.T) {
	syn := NewSyntheticFrameSource(8, 4)
	if _, ok := syn.Latest(); !ok {
		t.Fatal("the synthetic source produced no picture before Close")
	}
	if err := syn.Close(); err != nil {
		t.Fatalf("synthetic Close: %v", err)
	}
	if _, ok := syn.Latest(); ok {
		t.Fatal("the synthetic source handed out a picture after Close")
	}

	demo := NewDemoFrameSource(32, 18)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := demo.Latest(); ok {
			break
		}
		if time.Now().After(deadline) {
			_ = demo.Close()
			t.Fatal("the demo source produced no picture")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := demo.Close(); err != nil {
		t.Fatalf("demo Close: %v", err)
	}
	if _, ok := demo.Latest(); ok {
		t.Fatal("the demo source handed out a picture after Close")
	}
	// Close is idempotent, including from the deferred call in shutdown paths.
	if err := demo.Close(); err != nil {
		t.Fatalf("second demo Close: %v", err)
	}
}

func TestGradientScratchReusesBuffers(t *testing.T) {
	buf := image.NewRGBA(image.Rect(0, 0, 320, 180))
	var s gradientScratch
	s.render(buf, 0)
	u, r := &s.uIdx[0], &s.rBase[0]
	s.render(buf, 1)
	if &s.uIdx[0] != u || &s.rBase[0] != r {
		t.Fatal("the gradient scratch reallocated its lookup tables")
	}
	s.render(buf, 2) // warm every slice before counting
	if allocs := testing.AllocsPerRun(10, func() { s.render(buf, 3) }); allocs != 0 {
		t.Fatalf("rendering a frame allocated %v times, want 0", allocs)
	}
}

// oneShotSource hands out one picture, then nothing: the live screen only
// needs the first build to adopt it.
type oneShotSource struct{ img *image.RGBA }

func (s *oneShotSource) Latest() (*image.RGBA, bool) {
	frame := s.img
	s.img = nil
	return frame, frame != nil
}

func (*oneShotSource) Close() error { return nil }

// stripedPicture is 4×4 of one-pixel black and white stripes: enlarged, a
// nearest-neighbour bitmap shows stripes of solid black and white, a smooth
// one blends between them.
func stripedPicture() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			v := uint8(0)
			if x%2 == 1 {
				v = 255
			}
			i := y*img.Stride + x*4
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = v, v, v, 255
		}
	}
	return img
}

// TestVideoSurfaceSamplesSharpAndSmoothWithoutRestart draws the live screen
// through mygo's renderer, switches the filter quality in both directions
// while the same session runs, and checks the picture really is resampled the
// way each setting asks (the old code marked the bitmap pixelated forever once
// "none" had been used).
func TestVideoSurfaceSamplesSharpAndSmoothWithoutRestart(t *testing.T) {
	a := newMirrPaneApp(t.TempDir(), &oneShotSource{stripedPicture()}, true)
	defer a.shutdown()
	a.applySnapshot(Snapshot{Phase: PhaseLive, State: &ReceiverState{Sender: "Test"}})

	view := ui.NewTester(a.view, 900, 540)
	// 4×4 scaled into the 900×540 window: stripes span the screen height.
	grays := func() int {
		img := view.Image()
		n := 0
		for x := 200; x < 700; x++ {
			if v := img.RGBAAt(x, 340).R; v > 10 && v < 245 {
				n++
			}
		}
		return n
	}

	if got := grays(); got < 8 {
		t.Fatalf("smooth sampling shows %d blended pixels along a stripe edge, want many", got)
	}

	// Switch to nearest-neighbour without touching the frame source.
	a.prefs.FilterQuality = "none"
	view.Frame()
	if got := grays(); got != 0 {
		t.Fatalf("sharp sampling shows %d blended pixels, want none", got)
	}
	sharp := a.bitmap
	if w, h := sharp.Size(); w != 4 || h != 4 {
		t.Fatalf("sharp bitmap is %dx%d, want 4x4", w, h)
	}

	// And back to smooth, still the same live session and same picture.
	a.prefs.FilterQuality = "medium"
	view.Frame()
	if got := grays(); got < 8 {
		t.Fatalf("switching back to smooth shows %d blended pixels, want many", got)
	}
	if w, h := a.bitmap.Size(); w != 4 || h != 4 {
		t.Fatalf("smooth bitmap is %dx%d, want 4x4", w, h)
	}

	// A quality change with no new frame at all: the held picture is reused.
	a.prefs.FilterQuality = "none"
	view.Frame()
	if got := grays(); got != 0 {
		t.Fatalf("sharp sampling after an idle switch shows %d blended pixels", got)
	}
}
