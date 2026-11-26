//go:build windows

package main

import (
	"fmt"
	"image"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/miku0139/airplay-go/backend/internal/frames"
	"golang.org/x/sys/windows"
)

// feedTestMapping names a private shared-memory section: tests must never
// collide with a running MirrPane, whose section holds a live picture.
func feedTestMapping(t *testing.T, suffix string) string {
	t.Helper()
	return fmt.Sprintf(`Local\MirrPaneFeedTest_%d_%s_%d`,
		windows.GetCurrentProcessId(), suffix, time.Now().UnixNano())
}

func fillRGBA(w, h int, v byte) []byte {
	pix := make([]byte, w*h*4)
	for i := 0; i < len(pix); i += 4 {
		pix[i], pix[i+1], pix[i+2], pix[i+3] = v, v, v, 255
	}
	return pix
}

// TestFeedFrameSource2KDataPath drives a real 2K publisher through the frame
// source and into a ui.Bitmap, the path the window draws from: newest picture,
// zero-copy pixels, no re-presentation of an unchanged frame, and the picture
// on screen left alone by later publishes.
func TestFeedFrameSource2KDataPath(t *testing.T) {
	name := feedTestMapping(t, "2k")
	pub, err := frames.NewPublisher(name, 2560, 1440)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	defer pub.Close()

	src := newPlatformFrameSource(name).(*feedFrameSource)
	defer src.Close()

	if _, ok := src.Latest(); ok {
		t.Fatal("Latest returned a picture before any publish")
	}

	const w, h = 2560, 1440
	if err := pub.Publish(w, h, w*4, fillRGBA(w, h, 0x30)); err != nil {
		t.Fatalf("Publish 1: %v", err)
	}
	frame, ok := src.Latest()
	if !ok {
		t.Fatal("Latest returned false after the first publish")
	}
	if frame.Rect != image.Rect(0, 0, w, h) || frame.Stride != w*4 {
		t.Fatalf("frame = %v stride %d, want 2560x1440 stride %d", frame.Rect, frame.Stride, w*4)
	}
	if frame.Pix[0] != 0x30 || frame.Pix[len(frame.Pix)-4] != 0x30 {
		t.Fatalf("frame pixels = %#x..%#x, want 0x30", frame.Pix[0], frame.Pix[len(frame.Pix)-4])
	}

	// mygo takes a mapped RGBA of rows 4*width long as it is, without a copy;
	// the same slot the reader holds backs the bitmap.
	bitmap := ui.NewBitmap(opaqueBlank())
	bitmap.Update(frame)
	if bw, bh := bitmap.Size(); bw != w || bh != h {
		t.Fatalf("bitmap size = %dx%d, want %dx%d", bw, bh, w, h)
	}

	// No new picture: no presentation, no re-upload, and the pixels stay.
	if _, ok := src.Latest(); ok {
		t.Fatal("Latest re-presented an unchanged picture")
	}
	if frame.Pix[0] != 0x30 {
		t.Fatal("the picture on screen changed without a new publish")
	}

	// A newer picture is written into another slot; the held one is untouched,
	// so a paint between the two Latest calls still uploads the old picture.
	if err := pub.Publish(w, h, w*4, fillRGBA(w, h, 0x70)); err != nil {
		t.Fatalf("Publish 2: %v", err)
	}
	if frame.Pix[0] != 0x30 {
		t.Fatal("a new publish overwrote the picture on screen")
	}
	next, ok := src.Latest()
	if !ok {
		t.Fatal("Latest did not hand out the newer picture")
	}
	if next.Pix[0] != 0x70 {
		t.Fatalf("new picture = %#x, want 0x70", next.Pix[0])
	}

	// Steady state allocates nothing: the presentation image is reused.
	if allocs := testing.AllocsPerRun(50, func() { _, _ = src.Latest() }); allocs != 0 {
		t.Fatalf("Latest allocated %v times with no new picture, want 0", allocs)
	}
}

// TestFeedFrameSourceDrawsLive2KWindow is the whole local path: a 2K publisher
// writes a picture into its own shared-memory section, the app's frame source
// adopts it, and mygo's renderer rasterises the live window with it. Nothing
// here touches a running receiver's section.
func TestFeedFrameSourceDrawsLive2KWindow(t *testing.T) {
	name := feedTestMapping(t, "window")
	const w, h = 2560, 1440

	pub, err := frames.NewPublisher(name, w, h)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	defer pub.Close()
	src := newPlatformFrameSource(name)
	app := newMirrPaneApp(t.TempDir(), src, true)
	defer app.shutdown()

	want := [3]byte{200, 40, 80}
	pix := make([]byte, w*h*4)
	for i := 0; i < len(pix); i += 4 {
		pix[i], pix[i+1], pix[i+2], pix[i+3] = want[0], want[1], want[2], 255
	}
	if err := pub.Publish(w, h, w*4, pix); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	app.applySnapshot(Snapshot{Phase: PhaseLive, State: &ReceiverState{Sender: "2K"}})

	view := ui.NewTester(app.view, 900, 540)
	if bw, bh := app.bitmap.Size(); bw != w || bh != h {
		t.Fatalf("presented %dx%d, want %dx%d", bw, bh, w, h)
	}
	if got := view.Image().RGBAAt(450, 340); got.R != want[0] || got.G != want[1] || got.B != want[2] {
		t.Fatalf("live window center = %v, want %v", got, want)
	}

	// The next picture reaches the window without a restart or a new bitmap.
	next := [3]byte{30, 160, 220}
	for i := 0; i < len(pix); i += 4 {
		pix[i], pix[i+1], pix[i+2], pix[i+3] = next[0], next[1], next[2], 255
	}
	if err := pub.Publish(w, h, w*4, pix); err != nil {
		t.Fatalf("Publish 2: %v", err)
	}
	view.Frame()
	if got := view.Image().RGBAAt(450, 340); got.R != next[0] || got.G != next[1] || got.B != next[2] {
		t.Fatalf("live window center after the next frame = %v, want %v", got, next)
	}
	if bw, bh := app.bitmap.Size(); bw != w || bh != h {
		t.Fatalf("bitmap after the next frame = %dx%d, want %dx%d", bw, bh, w, h)
	}
}

// TestFeedFrameSourceKeepsPictureAcrossRestart restarts the publisher on the
// live section (as the sidecar does) and checks the picture the UI shows is
// neither re-presented nor damaged, and that the next picture is adopted.
func TestFeedFrameSourceKeepsPictureAcrossRestart(t *testing.T) {
	name := feedTestMapping(t, "restart")
	const w, h = 2560, 1440

	p1, err := frames.NewPublisher(name, w, h)
	if err != nil {
		t.Fatalf("NewPublisher 1: %v", err)
	}
	src := newPlatformFrameSource(name).(*feedFrameSource)
	defer src.Close()

	if err := p1.Publish(w, h, w*4, fillRGBA(w, h, 0xAA)); err != nil {
		p1.Close()
		t.Fatalf("p1 Publish: %v", err)
	}
	frame, ok := src.Latest()
	if !ok || frame.Pix[0] != 0xAA {
		p1.Close()
		t.Fatalf("first Latest ok=%v", ok)
	}

	p1.Close()
	p2, err := frames.NewPublisher(name, w, h)
	if err != nil {
		t.Fatalf("NewPublisher 2 (reuse): %v", err)
	}
	defer p2.Close()
	adopted := p2.ConsumerStats().Adopted

	// The restart must not re-present or invalidate the picture on screen.
	if _, ok := src.Latest(); ok {
		t.Fatal("Latest presented a picture after a sidecar restart with no new frame")
	}
	if frame.Pix[0] != 0xAA {
		t.Fatal("the picture on screen changed across the restart")
	}
	if got := p2.ConsumerStats().Adopted; got != adopted {
		t.Fatalf("adoption count moved across the restart: %d -> %d", adopted, got)
	}

	// The restarted producer's next picture is adopted normally.
	if err := p2.Publish(w, h, w*4, fillRGBA(w, h, 0xBB)); err != nil {
		t.Fatalf("p2 Publish: %v", err)
	}
	next, ok := src.Latest()
	if !ok || next.Pix[0] != 0xBB {
		t.Fatalf("Latest after restart ok=%v", ok)
	}
	if got := p2.ConsumerStats().Adopted; got != adopted+1 {
		t.Fatalf("adoption count after restart publish = %d, want %d", got, adopted+1)
	}
}
