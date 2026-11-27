package audio

import (
	"testing"

	"github.com/Miku0139oao/MirrPane/backend/internal/decode"
)

func TestRepeatedPacketsDecodeOnceAcrossSequenceWrap(t *testing.T) {
	const packets = 1000
	decodeCalls, callbackFrames := 0, 0
	r := newTestAudioReceiver(t, &testAudioDecoder{
		onDecode: func([]byte) (*decode.AudioFrame, error) {
			decodeCalls++
			return &decode.AudioFrame{Samples: make([]int16, 522*2), Channels: 2, SampleRate: 48000}, nil
		},
	})
	r.handlers.OnFrame = func(frame *decode.AudioFrame) { callbackFrames += frame.Frames() }
	for i := range packets {
		seq := uint16(65500 + i)
		for range 3 {
			r.handlePacket(testAudioPacket(seq))
		}
		// A late copy of the previous packet must not become new audio either.
		if i > 0 {
			r.handlePacket(testAudioPacket(seq - 1))
		}
	}
	if decodeCalls != packets || callbackFrames != packets*522 {
		t.Fatalf("duplicates over-supplied PCM: decoded=%d callbackFrames=%d", decodeCalls, callbackFrames)
	}
	got, dropped, decoded := r.Stats()
	if got != packets*4-1 || dropped != packets*3-1 || decoded != packets {
		t.Fatalf("sequence accounting = (%d, %d, %d), want (%d, %d, %d)", got, dropped, decoded, packets*4-1, packets*3-1, packets)
	}
}
