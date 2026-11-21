package decode

import (
	"errors"
	"os"
	"testing"
)

// splitAnnexB cuts a byte-stream capture into access units at each 4-byte start
// code, which is how the test feeds the decoder one frame at a time.
func splitAnnexB(b []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+3 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1 {
			if start >= 0 {
				out = append(out, b[start:i])
			}
			start = i
			i += 3
		}
	}
	if start >= 0 && start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

func TestDecodeH264Stream(t *testing.T) {
	stream, err := os.ReadFile("testdata/sample.h264")
	if err != nil {
		t.Skipf("fixture missing: %v", err)
	}

	d, err := NewDecoder()
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	defer d.Close()

	// A byte-stream capture has SPS/PPS interleaved with slices, not one access
	// unit per start code, so the whole buffer is fed up front and the decoder
	// hands back every picture it has.
	img, err := d.Decode(stream)
	if errors.Is(err, ErrNoFrame) {
		t.Fatal("no picture decoded from a valid stream")
	}
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if img.Width != 320 || img.Height != 240 {
		t.Fatalf("decoded %dx%d, want 320x240", img.Width, img.Height)
	}
	// Which path ran is worth knowing when a machine looks slow: the picture must
	// be identical either way, so this is informative, not an assertion.
	t.Logf("decoded 320x240 with hardware=%v", d.Hardware())
	if len(img.Pixels) != img.Stride*img.Height {
		t.Fatalf("buffer %d bytes, want %d", len(img.Pixels), img.Stride*img.Height)
	}
	if img.Stride != 320*4 {
		t.Fatalf("stride = %d, want %d", img.Stride, 320*4)
	}

	// A testsrc2 frame is not uniform. If the conversion produced an empty or
	// all-zero buffer the picture would be black, so require real variance.
	var min, max byte = 255, 0
	for i := 0; i < len(img.Pixels); i += 4 {
		if img.Pixels[i] < min {
			min = img.Pixels[i]
		}
		if img.Pixels[i] > max {
			max = img.Pixels[i]
		}
	}
	if max-min < 8 {
		t.Fatalf("blue channel spans only %d..%d; the frame looks empty", min, max)
	}
}

func TestDecodeEmptyPacket(t *testing.T) {
	d, err := NewDecoder()
	if err != nil {
		t.Skipf("FFmpeg runtime unavailable: %v", err)
	}
	defer d.Close()

	if _, err := d.Decode(nil); !errors.Is(err, ErrNoFrame) {
		t.Fatalf("Decode(nil) = %v, want ErrNoFrame", err)
	}
	if _, err := d.Decode([]byte{0x00, 0x00, 0x00, 0x01, 0xff}); err == nil {
		t.Fatal("garbage was accepted")
	}
}

func TestVersionIsReported(t *testing.T) {
	if v := Version(); v == "" {
		t.Fatal("Version is empty")
	}
}

// The direct path renders into caller-supplied memory at an arbitrary row
// stride. It must not touch the decoder's own picture buffer, and it must not
// write into the padding between rows.
func TestReceiveAccessUnitAndCopyImageTo(t *testing.T) {
	stream, err := os.ReadFile("testdata/sample.h264")
	if err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	units := accessUnits(stream)
	if len(units) == 0 {
		t.Fatal("no access units in fixture")
	}

	d, err := NewDecoderWithOptions(Options{PreferHardware: false, ColorRange: "auto", ScaleFilter: "fast"})
	if err != nil {
		t.Fatalf("NewDecoderWithOptions: %v", err)
	}
	defer d.Close()

	const pad = 64
	shortRejected, converted := false, false
	for _, u := range units {
		if err := d.ReceiveAccessUnit(u, true); errors.Is(err, ErrNoFrame) {
			continue
		} else if err != nil {
			t.Fatalf("ReceiveAccessUnit: %v", err)
		}
		w, h := d.FrameSize()
		if w != 320 || h != 240 {
			t.Fatalf("frame size %dx%d, want 320x240", w, h)
		}
		if d.LastImage() != nil {
			t.Fatal("the direct path filled the decoder's own buffer")
		}

		// A short destination must be refused before any row is converted.
		if !shortRejected {
			shortRejected = true
			if _, err := d.CopyImageTo(make([]byte, w*4*h-1), w*4); err == nil {
				t.Fatal("a short destination was accepted")
			}
			continue
		}

		stride := w*4 + pad
		dst := make([]byte, stride*h)
		timings, err := d.CopyImageTo(dst, stride)
		if err != nil {
			t.Fatalf("CopyImageTo: %v", err)
		}
		if timings.TransferNanos < 0 || timings.ConvertNanos < 0 {
			t.Fatalf("negative timings: %+v", timings)
		}
		for y := 0; y < h; y++ {
			for _, b := range dst[y*stride+w*4 : (y+1)*stride] {
				if b != 0 {
					t.Fatalf("row %d padding was written", y)
				}
			}
		}
		// testsrc2 is not uniform; an empty conversion would be all black.
		var min, max byte = 255, 0
		for y := 0; y < h; y++ {
			row := dst[y*stride : y*stride+w*4]
			for x := 0; x < len(row); x += 4 {
				if row[x] < min {
					min = row[x]
				}
				if row[x] > max {
					max = row[x]
				}
			}
		}
		if max-min < 8 {
			t.Fatalf("blue channel spans only %d..%d; the frame looks empty", min, max)
		}
		converted = true
		break
	}
	if !shortRejected {
		t.Fatal("no picture was produced for the short-destination check")
	}
	if !converted {
		t.Fatal("no picture was produced for the stride check")
	}

	// One received picture converts once; a second attempt must be refused.
	if w, h := d.FrameSize(); w != 0 || h != 0 {
		t.Fatalf("FrameSize after conversion = %dx%d, want 0x0", w, h)
	}
	if _, err := d.CopyImageTo(make([]byte, 320*240*4), 320*4); err == nil {
		t.Fatal("converted a picture twice")
	}
}

// Skipping the download and conversion must not disturb decoding: P/B frames
// are references, so both paths must yield exactly the same pictures.
func TestGatedReceiveMatchesFullDecode(t *testing.T) {
	stream, err := os.ReadFile("testdata/sample.h264")
	if err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	units := accessUnits(stream)
	if len(units) == 0 {
		t.Fatal("no access units in fixture")
	}

	count := func(convert bool) int {
		d, err := NewDecoderWithOptions(Options{PreferHardware: false, ColorRange: "auto", ScaleFilter: "fast"})
		if err != nil {
			t.Fatalf("NewDecoderWithOptions: %v", err)
		}
		defer d.Close()
		pictures := 0
		for _, u := range units {
			err := d.ReceiveAccessUnit(u, convert)
			if errors.Is(err, ErrNoFrame) {
				continue
			}
			if err != nil {
				t.Fatalf("ReceiveAccessUnit(convert=%v): %v", convert, err)
			}
			pictures++
		}
		return pictures
	}

	full := count(true)
	gated := count(false)
	if full == 0 {
		t.Fatal("the fixture produced no pictures")
	}
	if gated != full {
		t.Fatalf("gated decode produced %d pictures, full decode %d", gated, full)
	}
}

func TestDecodeAccessUnit(t *testing.T) {
	stream, err := os.ReadFile("testdata/sample.h264")
	if err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	units := splitAnnexB(stream)
	if len(units) < 2 {
		t.Fatalf("expected multiple NAL units, got %d", len(units))
	}

	d, err := NewDecoder()
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	defer d.Close()

	// Feed SPS/PPS + first IDR slice as one access unit.
	var firstAU []byte
	got := 0
	for _, u := range units {
		nalType := u[4] & 0x1F
		firstAU = append(firstAU, u...)
		if nalType == 5 || nalType == 1 {
			img, err := d.DecodeAccessUnit(firstAU)
			firstAU = nil
			if errors.Is(err, ErrNoFrame) {
				continue
			}
			if err != nil {
				t.Fatalf("DecodeAccessUnit: %v", err)
			}
			if img.Width != 320 || img.Height != 240 {
				t.Fatalf("decoded %dx%d, want 320x240", img.Width, img.Height)
			}
			got++
		}
	}
	if got == 0 {
		t.Fatal("DecodeAccessUnit produced no pictures")
	}
}

func TestDecodeSoftwareForcedAndOptions(t *testing.T) {
	stream, err := os.ReadFile("testdata/sample.h264")
	if err != nil {
		t.Skipf("fixture missing: %v", err)
	}

	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"software-full-bicubic", Options{PreferHardware: false, ColorRange: "full", ScaleFilter: "bicubic"}},
		{"software-limited-bilinear", Options{PreferHardware: false, ColorRange: "limited", ScaleFilter: "bilinear"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := NewDecoderWithOptions(tc.opts)
			if err != nil {
				t.Fatalf("NewDecoderWithOptions: %v", err)
			}
			defer d.Close()

			img, err := d.Decode(stream)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if d.Hardware() {
				t.Fatal("Hardware() = true when PreferHardware=false")
			}
			if img.Width != 320 || img.Height != 240 {
				t.Fatalf("decoded %dx%d, want 320x240", img.Width, img.Height)
			}
		})
	}
}
