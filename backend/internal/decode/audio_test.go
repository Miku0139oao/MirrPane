package decode

import (
	"encoding/binary"
	"errors"
	"os"
	"testing"
)

// AacLcConfig is the AudioSpecificConfig for AAC-LC 44100 Hz stereo (AOT 2,
// frequency index 4, channel configuration 2). It is not what AirPlay sends; it
// lets the test drive the same decoder with a stream FFmpeg can encode.
var AacLcConfig = []byte{0x12, 0x10}

// stripADTS splits an ADTS stream into raw access units, which is the form the
// decoder expects. Each header is 7 bytes when no CRC follows.
func stripADTS(t *testing.T, b []byte) [][]byte {
	t.Helper()
	var units [][]byte
	for off := 0; off+7 <= len(b); {
		if b[off] != 0xFF || b[off+1]&0xF0 != 0xF0 {
			t.Fatalf("no ADTS sync at %d", off)
		}
		length := int(b[off+3]&0x03)<<11 | int(b[off+4])<<3 | int(b[off+5])>>5
		if length < 7 || off+length > len(b) {
			t.Fatalf("bad ADTS frame length %d at %d", length, off)
		}
		header := 7
		if b[off+1]&0x01 == 0 {
			header += 2 // CRC present
		}
		units = append(units, b[off+header:off+length])
		off += length
	}
	return units
}

func TestDecodeAACStream(t *testing.T) {
	raw, err := os.ReadFile("testdata/tone.aac")
	if err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	units := stripADTS(t, raw)
	if len(units) < 10 {
		t.Fatalf("only %d access units", len(units))
	}

	d, err := NewAudioDecoder(AacLcConfig)
	if err != nil {
		t.Fatalf("NewAudioDecoder: %v", err)
	}
	defer d.Close()

	var total int
	var sawAudio bool
	for _, unit := range units {
		frame, err := d.Decode(unit)
		if errors.Is(err, ErrNoFrame) {
			continue
		}
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if frame.Channels != 2 {
			t.Fatalf("channels = %d, want 2", frame.Channels)
		}
		if frame.SampleRate != OutputRate {
			t.Fatalf("sample rate = %d, want %d", frame.SampleRate, OutputRate)
		}
		if frame.Frames() == 0 {
			t.Fatal("empty audio frame")
		}
		// A 440 Hz sine must not be silence.
		var peak int16
		for _, s := range frame.Samples {
			if s > peak {
				peak = s
			} else if -s > peak {
				peak = -s
			}
		}
		if peak > 1000 {
			sawAudio = true
		}
		total += frame.Frames()
	}

	if !sawAudio {
		t.Fatal("decoded only silence")
	}
	// One second of audio at 48 kHz. The resampler keeps a delay line, so the
	// count runs slightly over plus whatever the last frame flushes.
	if total < 45000 || total > 51000 {
		t.Fatalf("decoded %d frames, want about 48000", total)
	}
}

func TestAudioDecoderRejectsEmpty(t *testing.T) {
	d, err := NewAudioDecoder(nil)
	if err != nil {
		t.Fatalf("NewAudioDecoder: %v", err)
	}
	defer d.Close()
	if _, err := d.Decode(nil); !errors.Is(err, ErrNoFrame) {
		t.Fatalf("Decode(nil) = %v, want ErrNoFrame", err)
	}
}

// EldConfig must match the AudioSpecificConfig UxPlay hands to its decoder.
func TestEldConfigShape(t *testing.T) {
	if len(EldConfig) != 4 {
		t.Fatalf("ELD config is %d bytes, want 4", len(EldConfig))
	}
	// Decode the whole AudioSpecificConfig. Bit 0 is the MSB of the first byte.
	//   bits 0..4   audioObjectType (31 = escape)
	//   bits 5..10  object type extension, 32 + 7 = 39 (ER AAC ELD)
	//   bits 11..14 samplingFrequencyIndex (4 = 44100 Hz)
	//   bits 15..18 channelConfiguration (2 = stereo)
	bits := binary.BigEndian.Uint32(EldConfig)
	if aot := (bits >> 27) & 0x1f; aot != 31 {
		t.Fatalf("audio object type prefix = %d, want the 31 escape", aot)
	}
	if ext := (bits >> 21) & 0x3f; 32+ext != 39 {
		t.Fatalf("audio object type = %d, want 39 (ER AAC ELD)", 32+ext)
	}
	if freq := (bits >> 17) & 0x0f; freq != 4 {
		t.Fatalf("sample rate index = %d, want 4 (44100 Hz)", freq)
	}
	if ch := (bits >> 13) & 0x0f; ch != 2 {
		t.Fatalf("channel configuration = %d, want 2", ch)
	}
}
