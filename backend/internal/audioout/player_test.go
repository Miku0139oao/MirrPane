//go:build windows

package audioout

import (
	"testing"
	"unsafe"
)

func TestPlayerRingCapsBacklog(t *testing.T) {
	p := &Player{
		bufs: make([]buffer, bufCount),
		ring: make([]int16, ringFrames*Channels),
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
	}

	// Simulate a burst larger than the balanced ring arriving at once.
	chunk := make([]int16, 522*Channels)
	for i := 0; i < 40; i++ {
		p.Write(chunk)
	}

	if got := p.queued / Channels; got > ringFrames {
		t.Fatalf("queued = %d frames, want <= %d", got, ringFrames)
	}
	if p.queued%Channels != 0 {
		t.Fatalf("queued = %d samples, not aligned to %d channels", p.queued, Channels)
	}
}

// TestTakePreservesContinuity verifies that partial remainders between AAC-ELD
// frames (522 frames) and device buffers (480 frames) stay in the ring instead
// of being zero-padded into audible clicks.
func TestTakePreservesContinuity(t *testing.T) {
	p := &Player{
		bufs: make([]buffer, bufCount),
		ring: make([]int16, ringFrames*Channels),
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
	}

	// Write two consecutive 522-frame packets with a ramp signal.
	var seq int16 = 1
	writePacket := func() {
		pkt := make([]int16, 522*Channels)
		for i := range pkt {
			pkt[i] = seq
			seq++
		}
		p.Write(pkt)
	}

	dst := make([]byte, bufFrames*BytesPerFr)
	out := unsafe.Slice((*int16)(unsafe.Pointer(&dst[0])), bufFrames*Channels)

	writePacket()
	if n := p.take(dst); n != bufFrames {
		t.Fatalf("first take = %d, want %d", n, bufFrames)
	}
	// Remainder is 42 frames: must NOT be emitted as a zero-padded buffer.
	if n := p.take(dst); n != 0 {
		t.Fatalf("partial remainder emitted %d frames, want 0", n)
	}

	writePacket()
	if n := p.take(dst); n != bufFrames {
		t.Fatalf("second take = %d, want %d", n, bufFrames)
	}
	// First sample of second buffer must continue immediately after first buffer.
	wantFirst := int16(bufFrames*Channels + 1)
	if out[0] != wantFirst {
		t.Fatalf("discontinuity across packet boundary: got %d, want %d", out[0], wantFirst)
	}
}

func TestAudioProfileParams(t *testing.T) {
	cLow, rLow, tLow := profileParams("low")
	cBal, rBal, tBal := profileParams("balanced")
	cStb, rStb, tStb := profileParams("stable")
	if !(cLow <= cBal && cBal < cStb) || cLow < 5 {
		t.Fatalf("bufCount order invalid: low=%d bal=%d stable=%d", cLow, cBal, cStb)
	}
	if !(rLow < rBal && rBal < rStb) {
		t.Fatalf("ringFrames order invalid: low=%d bal=%d stable=%d", rLow, rBal, rStb)
	}
	if !(tLow < tBal && tBal < tStb) {
		t.Fatalf("trimTarget order invalid: low=%d bal=%d stable=%d", tLow, tBal, tStb)
	}
	for _, tc := range []struct {
		profile               string
		count, capacity, trim int
	}{
		{"low", 7, 250, 20},
		{"balanced", 8, 300, 30},
		{"stable", 10, 400, 40},
	} {
		count, capacity, trim := profileParams(tc.profile)
		if count != tc.count || capacity != SampleRate*tc.capacity/1000 || trim != SampleRate*tc.trim/1000 {
			t.Errorf("%s profile = (%d, %d, %d), want (%d, %d ms, %d ms)", tc.profile, count, capacity, trim, tc.count, tc.capacity, tc.trim)
		}
	}
}
