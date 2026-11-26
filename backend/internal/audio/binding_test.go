package audio

import (
	"bytes"
	"io"
	"log"
	"testing"
	"time"

	"github.com/miku0139/airplay-go/backend/internal/decode"
)

func TestAudioOwnershipRequiresDecodedFrames(t *testing.T) {
	decoded := false
	r := &Receiver{
		log: log.New(io.Discard, "", 0),
		dec: &testAudioDecoder{
			onConfigure: func([]byte) error { return nil },
			onFlush:     func() {},
			onDecode: func([]byte) (*decode.AudioFrame, error) {
				if !decoded {
					return nil, decode.ErrNoFrame
				}
				return &decode.AudioFrame{Samples: []int16{1, 2}, Channels: 2}, nil
			},
		},
	}
	r.Configure([]Key{{Name: "test", Key: bytes.Repeat([]byte{1}, 16)}}, bytes.Repeat([]byte{2}, 16))
	if !r.Busy() {
		t.Fatal("fresh audio setup has no start-up reservation")
	}
	r.configuredAt = time.Now().Add(-5 * time.Second)
	if r.Busy() {
		t.Fatal("unused setup kept audio reserved indefinitely")
	}
	if _, err := r.decodePacket(1, 0, []byte{0x8c}); err != decode.ErrNoFrame {
		t.Fatalf("incomplete audio decode = %v", err)
	}
	if r.Busy() || !r.lastFrame.IsZero() {
		t.Fatal("packet without decoded audio refreshed liveness")
	}
	decoded = true
	if frame, err := r.decodePacket(2, 0, []byte{0x8c}); err != nil || frame == nil {
		t.Fatalf("decoded audio = %v, %v", frame, err)
	}
	if !r.Busy() || r.lastFrame.IsZero() {
		t.Fatal("decoded audio did not protect the audio-only sender")
	}
	r.ResetStream()
	if r.Busy() || len(r.keys) == 0 || len(r.iv) != 16 {
		t.Fatal("audio-only reset did not retain keys while releasing liveness")
	}
	r.ResetKey()
	if r.Busy() || len(r.keys) != 0 || len(r.iv) != 0 {
		t.Fatal("full cleanup retained audio keys or ownership")
	}
	if f, err := r.decodePacket(3, 0, []byte{0x8c}); f != nil || err != nil {
		t.Fatal("audio could still be decrypted after full cleanup")
	}
}
