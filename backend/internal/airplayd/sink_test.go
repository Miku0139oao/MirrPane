//go:build windows && amd64

package airplayd

import (
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/Miku0139oao/MirrPane/backend/internal/decode"
	"github.com/Miku0139oao/MirrPane/backend/internal/framefeed"
	"github.com/Miku0139oao/MirrPane/backend/internal/frames"
	"github.com/Miku0139oao/MirrPane/backend/internal/mirror"
	"golang.org/x/sys/windows"
)

func TestSessionSnapshotsAreFreshAndUnique(t *testing.T) {
	name := fmt.Sprintf(`Local\MirrPaneSnapshot-%d`, os.Getpid())
	pub, err := frames.NewPublisher(name, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	publish := func(color byte) {
		t.Helper()
		if err := pub.Publish(2, 1, 8, []byte{color, 0, 0, 255, color, 0, 0, 255}); err != nil {
			t.Fatal(err)
		}
	}
	publish(27)
	sink := newFrameSink(nil, pub, nil)
	defer sink.Close()
	dir := t.TempDir()
	if _, err := sink.SnapshotPNG(dir); err == nil {
		t.Fatal("new receiver offered a previous picture")
	}
	publish(90)
	const count = 6
	var wg sync.WaitGroup
	paths := make(chan string, count)
	for range count {
		wg.Go(func() {
			path, err := sink.SnapshotPNG(dir)
			if err != nil {
				t.Error(err)
				return
			}
			paths <- path
		})
	}
	wg.Wait()
	close(paths)
	seen := map[string]bool{}
	for path := range paths {
		if seen[path] {
			t.Fatalf("duplicate filename %q", path)
		}
		seen[path] = true
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if r, _, _, _ := img.At(1, 0).RGBA(); r>>8 != 90 {
			t.Fatalf("unexpected snapshot pixel %d", r>>8)
		}
	}
	if len(seen) != count {
		t.Fatalf("saved %d of %d shots", len(seen), count)
	}
	sink.ResetStream()
	if _, err := sink.SnapshotPNG(dir); err == nil {
		t.Fatal("snapshot used a disconnected sender's picture")
	}
	publish(180)
	pic, err := sink.snapshotPicture()
	if err != nil || pic.Pix[0] != 180 {
		t.Fatalf("new session picture: %v", err)
	}
}

// The fixture is a single intra-coded 2560x1178 picture, red on the left
// and blue on the right. Exercise the real decoder, sink and native reader
// at the visible (cropped) size sent by a landscape iPhone.
func TestLandscape2KNativeReceiverPath(t *testing.T) {
	const w, h = 2560, 1178
	stream, err := os.ReadFile(filepath.Join("..", "..", "internal", "decode", "testdata", "landscape-2560x1178.h264"))
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf(`Local\MirrPaneLandscape-%d`, os.Getpid())
	pub, err := frames.NewPublisher(name, w, h)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	reader, err := framefeed.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	dec, err := decode.NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	sink := newFrameSink(dec, pub, nil)
	defer sink.Close()
	var seq uint64
	for i := range 12 {
		if !sink.Handle(&mirror.Frame{Data: stream, Valid: true}) {
			t.Fatalf("frame %d was not published", i)
		}
		f, ok := reader.Latest()
		if !ok || f.Width != w || f.Height != h || f.Seq <= seq {
			t.Fatalf("frame %d: got %dx%d seq=%d after %d", i, f.Width, f.Height, f.Seq, seq)
		}
		seq = f.Seq
		left := f.Pixels[(h-1)*f.Stride+4:]
		right := f.Pixels[(h-1)*f.Stride+4*(w-2):]
		if left[0] < 220 || left[2] > 30 || right[0] > 30 || right[2] < 220 {
			t.Fatalf("frame %d: last visible row has unexpected colors: left=%v right=%v", i, left[:4], right[:4])
		}
	}
	if pub.ConsumerAllowsGPUShare() || pub.ConsumerStats().Adopted != 12 {
		t.Fatalf("expected 12 native RGBA adoptions, got %+v", pub.ConsumerStats())
	}
	t.Logf("12 landscape %dx%d pictures decoded by %s and adopted by native reader", w, h, sink.Mode())
}

func splitTestAccessUnits(stream []byte) [][]byte {
	var nals [][]byte
	findSC := func(b []byte, from int) (int, int) {
		for i := from; i+2 < len(b); i++ {
			if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
				start := i
				if start > from && b[start-1] == 0 {
					start--
				}
				return start, i + 3
			}
		}
		return -1, -1
	}
	start, payload := findSC(stream, 0)
	for start >= 0 {
		next, nextPayload := findSC(stream, payload)
		end := next
		if end < 0 {
			end = len(stream)
		}
		nals = append(nals, stream[start:end])
		start, payload = next, nextPayload
	}
	var units [][]byte
	var cur []byte
	for _, u := range nals {
		_, p := findSC(u, 0)
		cur = append(cur, u...)
		if t := u[p] & 0x1f; t >= 1 && t <= 5 {
			units = append(units, cur)
			cur = nil
		}
	}
	return units
}

func TestFrameSinkGPUZeroCopySnapshotAndConsumerFallback(t *testing.T) {
	stream, err := os.ReadFile(filepath.Join("..", "..", "internal", "decode", "testdata", "sample.h264"))
	if err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
	units := splitTestAccessUnits(stream)
	if len(units) < 4 {
		t.Fatalf("expected >=4 access units, got %d", len(units))
	}

	shmName := fmt.Sprintf(`Local\MirrPaneSinkGPUTest_%d`, os.Getpid())
	pub, err := frames.NewPublisher(shmName, 1920, 1080)
	if err != nil {
		t.Skipf("shared memory unavailable: %v", err)
	}
	t.Cleanup(pub.Close)

	dec, err := decode.NewDecoder()
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}

	sink := newFrameSink(dec, pub, nil)
	defer sink.Close()

	// Advertise a GPU-capable consumer through consumer_pid and consumer_flags.
	namePtr, err := windows.UTF16PtrFromString(shmName)
	if err != nil {
		t.Fatalf("UTF16PtrFromString: %v", err)
	}
	hMap, err := windows.CreateFileMapping(windows.InvalidHandle, nil, windows.PAGE_READWRITE, 0, 4096, namePtr)
	if err != nil && err != windows.ERROR_ALREADY_EXISTS {
		t.Fatalf("open test mapping: %v", err)
	}
	defer windows.CloseHandle(hMap)
	view, err := windows.MapViewOfFile(hMap, windows.FILE_MAP_READ|windows.FILE_MAP_WRITE, 0, 0, 4096)
	if err != nil {
		t.Fatalf("MapViewOfFile: %v", err)
	}
	defer windows.UnmapViewOfFile(view)

	consumerPID := (*uint32)(unsafe.Add(unsafe.Pointer(nil), view+0x080))
	consumerFlags := (*uint32)(unsafe.Add(unsafe.Pointer(nil), view+0x084))
	atomic.StoreUint32(consumerPID, uint32(os.Getpid()))
	atomic.StoreUint32(consumerFlags, frames.ConsumerFlagAttached|frames.ConsumerFlagGPUShare)

	// Feed access units until the first picture is published.
	used := 0
	publishedFirst := false
	for i, u := range units {
		used = i + 1
		if sink.Handle(&mirror.Frame{Valid: true, Data: u}) {
			publishedFirst = true
			break
		}
	}
	if !publishedFirst {
		t.Fatal("sink produced no picture")
	}

	_, format, ok := pub.LatestSlot()
	stats := sink.Stats()
	if !ok || stats.GPUZeroCopy != (format == frames.FormatDXGIBGRA) {
		t.Fatalf("first publication format=%d ok=%v disagrees with gpuZeroCopy=%v", format, ok, stats.GPUZeroCopy)
	}
	if stats.GPUZeroCopy {
		if stats.DownloadAvgMs != 0 || stats.ConvertAvgMs <= 0 {
			t.Fatalf("GPU timings: download=%f convert=%f", stats.DownloadAvgMs, stats.ConvertAvgMs)
		}
		t.Logf("GPU convert+sync: %.3f ms on first publication", stats.ConvertAvgMs)
	} else {
		t.Logf("GPU sharing unavailable, exercising CPU snapshot (%s)", sink.Mode())
	}

	assertSinkSnapshot(t, sink)

	// Simulate cross-adapter mismatch reported by FrameReader via ConsumerFlagNoGPUShare.
	atomic.StoreUint32(consumerFlags, frames.ConsumerFlagAttached|frames.ConsumerFlagGPUShare|frames.ConsumerFlagNoGPUShare)
	publishedFallback := false
	for _, u := range units[used:] {
		if sink.Handle(&mirror.Frame{Valid: true, Data: u}) {
			publishedFallback = true
			break
		}
	}
	if !publishedFallback {
		t.Fatal("sink produced no picture after ConsumerFlagNoGPUShare")
	}
	_, format, ok = pub.LatestSlot()
	if !ok || format != frames.FormatRGBA || sink.Stats().GPUZeroCopy {
		t.Fatalf("expected immediate CPU FormatRGBA (%d) fallback, got ok=%v format=%d", frames.FormatRGBA, ok, format)
	}

	assertSinkSnapshot(t, sink)
}

func assertSinkSnapshot(t *testing.T, sink *frameSink) {
	t.Helper()
	path, err := sink.SnapshotPNG(t.TempDir())
	if err != nil {
		t.Fatalf("SnapshotPNG: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decode snapshot PNG: %v", err)
	}
	if img.Bounds().Dx() != 320 || img.Bounds().Dy() != 240 {
		t.Fatalf("snapshot size = %v, want 320x240", img.Bounds())
	}
	pixel := img.At(0, 0)
	for y := 0; y < img.Bounds().Dy(); y += 8 {
		for x := 0; x < img.Bounds().Dx(); x += 8 {
			if img.At(x, y) != pixel {
				return
			}
		}
	}
	t.Fatal("snapshot contains no visible variation")
}
