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
