//go:build windows && amd64

package decode

import (
	"runtime"
	"testing"
	"unsafe"
)

func TestELDResamplingPreservesLongRunSampleRate(t *testing.T) {
	ff, err := loadFFmpeg()
	if err != nil {
		t.Fatal(err)
	}
	// The real sender advertises spf=480, and the default ELD config specifies
	// 44.1 kHz. Keep using the same resampler, as the live decoder does.
	const sourceRate = 44100
	const packetSamples = 480
	const packets = 2940 // exactly 32 seconds at 44.1 kHz
	input := make([]int16, packetSamples*2)
	for i := range input {
		input[i] = 1000
	}
	plane := (*byte)(unsafe.Pointer(&input[0]))
	native := avFrame{Samples: packetSamples, Format: sampleS16, SampleRate: sourceRate, ExtendedData: &plane}
	ff.layoutDefault(&native.ChannelLayout, 2)
	defer ff.layoutUninit(&native.ChannelLayout)
	d := &AudioDecoder{ff: ff, frame: AudioFrame{SampleRate: OutputRate}}
	defer d.Close()
	total := 0
	for i := range packets {
		frame, err := d.convertFrame(&native)
		runtime.KeepAlive(input)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if i > 0 && frame.Frames() != 522 && frame.Frames() != 523 {
			t.Fatalf("packet %d produced %d frames, want 522/523", i, frame.Frames())
		}
		total += frame.Frames()
	}
	want := packets * packetSamples * OutputRate / sourceRate
	// swresample retains its short filter delay. A per-packet reset, a wrong
	// source frequency, or rounding every packet to 522 would fail this bound.
	if total > want || want-total > 32 {
		t.Fatalf("32 s produced %d frames, want %d minus <=32 filter-delay frames", total, want)
	}
	t.Logf("32 s of ELD-sized input produced %d/%d output frames (%.4f Hz)", total, want, float64(total)/32)
}
