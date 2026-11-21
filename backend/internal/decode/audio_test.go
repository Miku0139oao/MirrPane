package decode

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"testing"
	"unsafe"
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

func TestAudioDecoderConfigureReusesMatchingConfig(t *testing.T) {
	config := append([]byte(nil), AacLcConfig...)
	d, err := NewAudioDecoder(config)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx, frame, packet := d.ctx, d.nativeFrame, d.packet
	// The decoder must remember the config's bytes, not the caller's slice.
	config[0] ^= 0xff
	if err := d.Configure(append([]byte(nil), AacLcConfig...)); err != nil {
		t.Fatal(err)
	}
	if d.ctx != ctx || d.nativeFrame != frame || d.packet != packet {
		t.Fatal("identical config replaced native decoder objects")
	}
	if err := d.Configure(make([]byte, 65537)); err == nil {
		t.Fatal("oversized replacement config accepted")
	}
	if d.ctx != ctx || !slices.Equal(d.config, AacLcConfig) {
		t.Fatal("failed replacement changed the active decoder")
	}
	if err := d.Configure(EldConfig); err != nil {
		t.Fatal(err)
	}
	if d.ctx == ctx || !slices.Equal(d.config, EldConfig) {
		t.Fatal("changed config did not replace the native decoder")
	}
	ctx = d.ctx
	// nil and explicit ELD are the same AudioSpecificConfig.
	for range 3 {
		if err := d.Configure(nil); err != nil {
			t.Fatal(err)
		}
		if d.ctx != ctx {
			t.Fatal("default config repeatedly rebuilt the decoder")
		}
	}
}

func TestAudioDecoderFlushRestartsStream(t *testing.T) {
	raw, err := os.ReadFile("testdata/tone.aac")
	if err != nil {
		t.Fatal(err)
	}
	units := stripADTS(t, raw)
	d, err := NewAudioDecoder(AacLcConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	decodeStream := func() []int16 {
		var pcm []int16
		for _, unit := range units {
			frame, err := d.Decode(unit)
			if errors.Is(err, ErrNoFrame) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			pcm = append(pcm, frame.Samples...)
		}
		return pcm
	}
	want := decodeStream()
	if len(want) == 0 || d.resampler == 0 {
		t.Fatal("fixture did not exercise codec and resampler history")
	}
	ctx, nativeFrame, packet := d.ctx, d.nativeFrame, d.packet
	d.Flush()
	if d.ctx != ctx || d.nativeFrame != nativeFrame || d.packet != packet {
		t.Fatal("flush replaced native decoder objects")
	}
	if d.resampler != 0 || len(d.frame.Samples) != 0 || d.nativeFrame.Data[0] != nil || d.packet.Data != nil {
		t.Fatal("flush retained audio from the previous stream")
	}
	got := decodeStream()
	if len(got) != len(want) {
		t.Fatalf("flushed stream has %d samples, want %d", len(got), len(want))
	}
	// AAC's perceptual noise substitution state need not restart bit-for-bit
	// after a flush. Require the same signal within 1% RMS error instead.
	var signalEnergy, errorEnergy float64
	for i, sample := range got {
		signal := float64(want[i])
		diff := float64(sample) - signal
		signalEnergy += signal * signal
		errorEnergy += diff * diff
	}
	if signalEnergy == 0 || errorEnergy > signalEnergy*0.0001 {
		t.Fatalf("flushed stream retained history: error energy %g, signal energy %g", errorEnergy, signalEnergy)
	}
	d.Close()
	d.Flush() // A teardown after shutdown must be harmless.
}

func TestAudioDownmixToStereo(t *testing.T) {
	ff, err := loadFFmpeg()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		channels, rate int32
	}{
		{1, 44100},
		{2, 48000},
		{6, 48000},
		{6, 8000},
		{8, 44100},
	} {
		t.Run(fmt.Sprintf("%dch-%dHz", tc.channels, tc.rate), func(t *testing.T) {
			const samples = 1024
			input := make([]int16, samples*int(tc.channels))
			for i := range samples {
				if tc.channels > 2 {
					// A center-only signal must reach both stereo channels;
					// dropping all but the first two channels would be silent.
					input[i*int(tc.channels)+2] = 1000
				} else {
					for ch := range int(tc.channels) {
						input[i*int(tc.channels)+ch] = int16(1000 * (ch + 1))
					}
				}
			}
			plane := (*byte)(unsafe.Pointer(&input[0]))
			native := avFrame{Samples: samples, Format: sampleS16, SampleRate: tc.rate, ExtendedData: &plane}
			ff.layoutDefault(&native.ChannelLayout, tc.channels)
			defer ff.layoutUninit(&native.ChannelLayout)
			d := &AudioDecoder{ff: ff, frame: AudioFrame{SampleRate: OutputRate}}
			defer d.Close()
			frame, err := d.convertFrame(&native)
			runtime.KeepAlive(input)
			if err != nil {
				t.Fatal(err)
			}
			if frame.Channels != 2 || frame.SampleRate != 48000 || len(frame.Samples)%2 != 0 {
				t.Fatalf("output format = %d channels, %d Hz, %d samples", frame.Channels, frame.SampleRate, len(frame.Samples))
			}
			expected := (samples*OutputRate + int(tc.rate) - 1) / int(tc.rate)
			if frame.Frames() < expected-256 || frame.Frames() > expected {
				t.Fatalf("output frames = %d, want about %d", frame.Frames(), expected)
			}
			// Inspect the middle of the interleaved S16 output, past any delay.
			mid := frame.Frames() / 2 * 2
			if frame.Samples[mid] <= 0 || frame.Samples[mid+1] <= 0 {
				t.Fatalf("signal missing from stereo output: %v", frame.Samples[mid:mid+2])
			}
			if tc.channels == 2 && (frame.Samples[mid] != 1000 || frame.Samples[mid+1] != 2000) {
				t.Fatalf("S16 stereo is not interleaved: %v", frame.Samples[mid:mid+2])
			}
		})
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
