package audio

import (
	"bytes"
	"encoding/binary"
	"io"
	"log"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/miku0139/airplay-go/backend/internal/decode"
)

type testAudioDecoder struct {
	onDecode    func([]byte) (*decode.AudioFrame, error)
	onConfigure func([]byte) error
	onFlush     func()
	onClose     func()
}

func (d *testAudioDecoder) Decode(data []byte) (*decode.AudioFrame, error) {
	return d.onDecode(data)
}

func (d *testAudioDecoder) Configure(config []byte) error {
	return d.onConfigure(config)
}

func (d *testAudioDecoder) Flush() { d.onFlush() }
func (d *testAudioDecoder) Close() { d.onClose() }

func newTestAudioReceiver(t *testing.T, d audioDecoder) *Receiver {
	t.Helper()
	listen := func() *net.UDPConn {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	return &Receiver{
		log:     log.New(io.Discard, "", 0),
		data:    listen(),
		control: listen(),
		keys:    []Key{{Name: "test", Key: bytes.Repeat([]byte{0x11}, 16)}},
		iv:      bytes.Repeat([]byte{0x22}, 16),
		chosen:  -1,
		dec:     d,
	}
}

func testAudioPacket(seq uint16) []byte {
	pkt := make([]byte, HeaderLen+1)
	binary.BigEndian.PutUint16(pkt[2:4], seq)
	// A short payload has no encrypted prefix, but still passes the AAC check.
	pkt[HeaderLen] = 0x8c
	return pkt
}

func waitForAudioTest(t *testing.T, done <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", operation)
	}
}

func TestDecoderLifecycleWaitsForDecode(t *testing.T) {
	for _, action := range []string{"close", "reset", "configure"} {
		t.Run(action, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			started := make(chan struct{})
			called := make(chan struct{})
			finished := make(chan struct{})
			packetDone := make(chan struct{})
			decodeCalls := 0
			var r *Receiver
			lifecycle := func() {
				if r.decMu.TryLock() {
					r.decMu.Unlock()
					t.Error("decoder lifecycle called without the decoder lock")
				}
				// Decoder calls must also leave the receiver's state lock free.
				if r.mu.TryLock() {
					r.mu.Unlock()
				} else {
					t.Error("decoder lifecycle called with the receiver state lock held")
				}
				close(called)
			}
			d := &testAudioDecoder{
				onDecode: func([]byte) (*decode.AudioFrame, error) {
					if r.decMu.TryLock() {
						r.decMu.Unlock()
						t.Error("Decode called without the decoder lock")
					}
					if r.mu.TryLock() {
						r.mu.Unlock()
					} else {
						t.Error("Decode called with the receiver state lock held")
					}
					decodeCalls++
					close(entered)
					<-release
					return &decode.AudioFrame{Samples: []int16{100, 200}, Channels: 2, SampleRate: decode.OutputRate}, nil
				},
				onConfigure: func([]byte) error { lifecycle(); return nil },
				onFlush:     lifecycle,
				onClose:     lifecycle,
			}
			r = newTestAudioReceiver(t, d)
			go func() {
				r.handlePacket(testAudioPacket(1))
				close(packetDone)
			}()
			waitForAudioTest(t, entered, "Decode to start")
			go func() {
				close(started)
				switch action {
				case "close":
					r.Close()
				case "reset":
					r.ResetStream()
				case "configure":
					r.Configure(r.keys, r.iv, []byte{0x12, 0x10})
				}
				close(finished)
			}()
			waitForAudioTest(t, started, "lifecycle call to start")
			select {
			case <-called:
				t.Errorf("%s touched the decoder during Decode", action)
			case <-time.After(25 * time.Millisecond):
				// Decode stays blocked on release throughout this interval.
			}
			select {
			case <-finished:
				t.Errorf("%s returned before Decode finished", action)
			default:
			}
			if received, _, decoded := r.Stats(); received != 1 || decoded != 0 {
				t.Errorf("in-flight stats = (%d, %d), want (1, 0)", received, decoded)
			}
			close(release)
			waitForAudioTest(t, called, "decoder lifecycle operation")
			waitForAudioTest(t, finished, "lifecycle call to finish")
			waitForAudioTest(t, packetDone, "packet to finish")
			if action == "close" {
				// A late SETUP or packet must not resurrect a closed receiver.
				r.Configure(r.keys, r.iv)
				r.handlePacket(testAudioPacket(2))
				r.Close()
				if r.dec != nil || decodeCalls != 1 {
					t.Errorf("closed receiver retained a decoder or decoded late input: calls=%d", decodeCalls)
				}
			}
		})
	}
}

func TestFrameCallbackOwnsSamplesAndCanClose(t *testing.T) {
	frame := &decode.AudioFrame{Samples: []int16{100, 200}, Channels: 2, SampleRate: decode.OutputRate}
	d := &testAudioDecoder{
		onDecode: func([]byte) (*decode.AudioFrame, error) { return frame, nil },
		onFlush:  func() { frame.Samples[0] = -1 },
		onClose:  func() { frame.Channels = 0; frame.Samples = nil },
	}
	r := newTestAudioReceiver(t, d)
	done := make(chan struct{})
	called := false
	r.handlers.OnFrame = func(got *decode.AudioFrame) {
		called = true
		r.ResetStream()
		r.Close()
		if got.Channels != 2 || !slices.Equal(got.Samples, []int16{100, 200}) {
			t.Errorf("decoder lifecycle changed callback's PCM: %+v", got)
		}
	}
	go func() {
		r.handlePacket(testAudioPacket(1))
		close(done)
	}()
	waitForAudioTest(t, done, "reentrant frame callback")
	if !called {
		t.Error("frame callback was not called")
	}
}

func TestConfigureAndResetRetainDecoder(t *testing.T) {
	r, err := New(log.New(io.Discard, "", 0), Handlers{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.dec == nil {
		t.Fatal("audio decoder unavailable")
	}
	original := r.dec
	for range 3 {
		r.Configure([]Key{{Name: "test", Key: make([]byte, 16)}}, make([]byte, 16))
		r.ResetStream()
		if r.dec != original {
			t.Fatal("unchanged config rebuilt the decoder")
		}
	}
}
