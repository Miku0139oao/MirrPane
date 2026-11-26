//go:build windows

package audioout

import (
	"math"
	"testing"
	"time"
	"unsafe"
)

// fakeDevice models the Windows waveOut buffer queue on a deterministic clock.
type fakeDevice struct {
	now             time.Duration
	queue           []queuedBuf
	completionDelay time.Duration // driver may return a header after it played
	playbackEnd     time.Duration
	gaps            int
	gapDuration     time.Duration
	submittedFrames int
}

type queuedBuf struct {
	buf     *buffer
	endTime time.Duration
}

func (d *fakeDevice) submit(b *buffer, frames int) error {
	dur := time.Duration(frames) * time.Second / SampleRate
	start := d.now
	if len(d.queue) > 0 {
		if tail := d.queue[len(d.queue)-1].endTime; tail > start {
			start = tail
		}
	}
	if d.submittedFrames > 0 && start > d.playbackEnd {
		d.gaps++
		d.gapDuration += start - d.playbackEnd
	}
	d.submittedFrames += frames
	d.playbackEnd = start + dur
	d.queue = append(d.queue, queuedBuf{
		buf:     b,
		endTime: start + dur,
	})
	return nil
}

func (d *fakeDevice) advanceTo(t time.Duration) {
	d.now = t
	doneCount := 0
	for doneCount < len(d.queue) && d.queue[doneCount].endTime+d.completionDelay <= t {
		d.queue[doneCount].buf.hdr.Flags |= whdrDone
		doneCount++
	}
	if doneCount > 0 {
		d.queue = d.queue[doneCount:]
	}
}

func newTestPlayer(profile string, dev *fakeDevice) *Player {
	count, ring, trim := profileParams(profile)
	p := &Player{
		bufs:       make([]buffer, count),
		ring:       make([]int16, ring*Channels),
		trimTarget: trim,
		highWater:  highWaterFramesFor(ring, trim),
		wake:       make(chan struct{}, 1),
		done:       make(chan struct{}),
		pumpDone:   make(chan struct{}),
		submitBuf:  dev.submit,
		now:        func() time.Time { return time.Unix(0, int64(dev.now)) },
	}
	for i := range p.bufs {
		p.bufs[i].data = make([]byte, bufFrames*BytesPerFr)
		p.bufs[i].hdr.Data = uintptr(unsafe.Pointer(&p.bufs[i].data[0]))
		p.bufs[i].hdr.BufferLength = uint32(len(p.bufs[i].data))
	}
	return p
}

func makeToneFrames(frames int, phase *float64) []int16 {
	out := make([]int16, frames*Channels)
	const freq = 440.0
	step := 2 * math.Pi * freq / SampleRate
	for i := 0; i < frames; i++ {
		v := int16(12000 * math.Sin(*phase))
		out[i*Channels] = v
		out[i*Channels+1] = v
		*phase += step
	}
	return out
}

func TestSubCapacityBurstRecoversToTargetWithoutUnderrun(t *testing.T) {
	cases := []struct {
		name    string
		profile string
		burstMs int
		maxWait time.Duration
	}{
		{name: "low_105ms_burst", profile: "low", burstMs: 105, maxWait: 25 * time.Second},
		{name: "balanced_135ms_burst", profile: "balanced", burstMs: 135, maxWait: 25 * time.Second},
		{name: "stable_200ms_burst", profile: "stable", burstMs: 200, maxWait: 35 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dev := &fakeDevice{}
			p := newTestPlayer(tc.profile, dev)
			var phase float64

			// Prime steady playback with a 30 ms start, then pump.
			p.Write(makeToneFrames(SampleRate*30/1000, &phase))
			p.pumpStep()

			// Inject a single sub-capacity burst (above high-water, below ring capacity).
			burstFrames := tc.burstMs * SampleRate / 1000
			p.Write(makeToneFrames(burstFrames, &phase))
			p.pumpStep()

			afterBurst := p.Stats()
			if afterBurst.HighWaterMs < float64(tc.burstMs)-10 {
				t.Fatalf("HighWaterMs = %.2f, want >= %d", afterBurst.HighWaterMs, tc.burstMs-10)
			}

			// Simulate steady 10 ms producer cadence and 3 ms pump ticker.
			const tick = 3 * time.Millisecond
			const pktInterval = 10 * time.Millisecond
			nextPkt := pktInterval
			var settledAt time.Duration
			settled := false

			for now := tick; now <= tc.maxWait; now += tick {
				dev.advanceTo(now)
				for nextPkt <= now {
					p.Write(makeToneFrames(bufFrames, &phase))
					nextPkt += pktInterval
				}
				p.pumpStep()

				st := p.Stats()
				if st.UnderrunCount != 0 {
					t.Fatalf("t=%v: correction caused underrun (count=%d, occ=%.2fms)", now, st.UnderrunCount, st.OccupancyMs)
				}
				if !settled && st.OccupancyMs <= st.TargetMs+10.5 && st.DriftCount > 0 {
					// Confirm post-take occupancy (when ring just fed a buffer) reached target.
					if st.OccupancyMs <= st.TargetMs+0.5 {
						settled = true
						settledAt = now
					}
				}
			}

			if !settled {
				st := p.Stats()
				t.Fatalf("occupancy did not return to target %.2fms within %v (got %.2fms, trimCount=%d)",
					st.TargetMs, tc.maxWait, st.OccupancyMs, st.TrimCount)
			}

			// Run 2 more seconds at steady state and ensure occupancy remains near target without extra trims/underruns.
			driftsAtSettle := p.Stats().DriftCount
			for now := tc.maxWait + tick; now <= tc.maxWait+2*time.Second; now += tick {
				dev.advanceTo(now)
				for nextPkt <= now {
					p.Write(makeToneFrames(bufFrames, &phase))
					nextPkt += pktInterval
				}
				p.pumpStep()
			}

			final := p.Stats()
			if final.UnderrunCount != 0 {
				t.Fatalf("post-recovery UnderrunCount = %d, want 0", final.UnderrunCount)
			}
			if final.TrimCount != 0 {
				t.Fatalf("sub-capacity burst discarded audio: %+v", final)
			}
			if final.DriftCount != driftsAtSettle {
				t.Fatalf("unexpected extra correction after settling at %v: got %d, want %d", settledAt, final.DriftCount, driftsAtSettle)
			}
			if final.OccupancyMs > final.TargetMs+11.0 {
				t.Fatalf("post-recovery occupancy drifted up to %.2fms (target %.2fms)", final.OccupancyMs, final.TargetMs)
			}
		})
	}
}

func TestSteadyStatePreservesOccupancy(t *testing.T) {
	dev := &fakeDevice{}
	p := newTestPlayer("balanced", dev)
	var phase float64

	// Pre-fill right to the balanced target (30 ms) so steady state sits at target.
	p.Write(makeToneFrames(bufFrames*3, &phase))
	p.pumpStep()

	const tick = 3 * time.Millisecond
	const pktFrames = 522 // realistic AAC-ELD packet size (~10.875 ms)
	pktInterval := time.Duration(pktFrames) * time.Second / SampleRate
	nextPkt := pktInterval

	minOcc := math.MaxFloat64
	maxOcc := 0.0

	for now := tick; now <= 10*time.Second; now += tick {
		dev.advanceTo(now)
		for nextPkt <= now {
			p.Write(makeToneFrames(pktFrames, &phase))
			nextPkt += pktInterval
		}
		p.pumpStep()

		st := p.Stats()
		if !p.playing {
			continue // measure steady occupancy after the initial time cushion
		}
		if st.OccupancyMs < minOcc {
			minOcc = st.OccupancyMs
		}
		if st.OccupancyMs > maxOcc {
			maxOcc = st.OccupancyMs
		}
	}

	st := p.Stats()
	if st.UnderrunCount != 0 {
		t.Fatalf("steady state UnderrunCount = %d, want 0", st.UnderrunCount)
	}
	if st.TrimCount != 0 {
		t.Fatalf("steady state TrimCount = %d, want 0 (maxOcc=%.2fms, highWaterMark=%.2fms)", st.TrimCount, maxOcc, st.HighWaterMarkMs)
	}
	if maxOcc >= st.HighWaterMarkMs {
		t.Fatalf("steady state maxOcc = %.2fms reached highWaterMark %.2fms", maxOcc, st.HighWaterMarkMs)
	}
	if maxOcc-minOcc > 25.0 {
		t.Fatalf("steady state occupancy varied excessively: min=%.2fms max=%.2fms", minOcc, maxOcc)
	}
}

func TestUnderrunTelemetryCountsStarvationOncePerGap(t *testing.T) {
	dev := &fakeDevice{}
	p := newTestPlayer("balanced", dev)
	var phase float64

	// Startup below 20 ms cushion must NOT count as underrun.
	p.Write(makeToneFrames(bufFrames, &phase))
	p.pumpStep()
	if st := p.Stats(); st.UnderrunCount != 0 {
		t.Fatalf("idle startup UnderrunCount = %d, want 0", st.UnderrunCount)
	}

	// Cross 20 ms cushion so playback starts.
	p.Write(makeToneFrames(bufFrames, &phase))
	p.pumpStep()
	dev.advanceTo(startupCushion)
	p.pumpStep()
	if !p.playing {
		t.Fatal("expected player to enter playing state")
	}

	// Advance 100 ms with no further writes so all in-flight buffers drain.
	for now := startupCushion + 3*time.Millisecond; now <= 100*time.Millisecond; now += 3 * time.Millisecond {
		dev.advanceTo(now)
		p.pumpStep()
	}

	if st := p.Stats(); st.UnderrunCount != 1 {
		t.Fatalf("UnderrunCount after single gap = %d, want 1", st.UnderrunCount)
	}

	// Resume playback, then starve a second time -> count must become 2.
	p.Write(makeToneFrames(bufFrames*2, &phase))
	p.pumpStep()
	for now := 103 * time.Millisecond; now <= 200*time.Millisecond; now += 3 * time.Millisecond {
		dev.advanceTo(now)
		p.pumpStep()
	}
	if st := p.Stats(); st.UnderrunCount != 2 {
		t.Fatalf("UnderrunCount after second gap = %d, want 2", st.UnderrunCount)
	}
}

func TestOverflowCrossfadeAvoidsHardJump(t *testing.T) {
	dev := &fakeDevice{}
	p := newTestPlayer("balanced", dev)

	// Fill ring with +10000 followed by -10000, then trigger overflow so the
	// discarded head (+10000) splices into the kept tail (-10000).
	// A hard jump from +10000 to -10000 would have a delta of 20000 in one sample;
	// a 2 ms (96-frame) crossfade keeps every adjacent sample delta small.
	posChunk := make([]int16, p.ringFramesCap()/2*Channels)
	for i := range posChunk {
		posChunk[i] = 10000
	}
	p.Write(posChunk)

	negChunk := make([]int16, (p.ringFramesCap()-len(posChunk)/Channels)*Channels)
	for i := range negChunk {
		negChunk[i] = -10000
	}
	p.Write(negChunk)

	overflowChunk := make([]int16, 500*Channels)
	for i := range overflowChunk {
		overflowChunk[i] = -10000
	}
	p.Write(overflowChunk)

	st := p.Stats()
	if st.TrimCount != 1 {
		t.Fatalf("overflow TrimCount = %d, want 1", st.TrimCount)
	}

	dst := make([]byte, bufFrames*BytesPerFr)
	n := p.take(dst)
	if n != bufFrames {
		t.Fatalf("take = %d, want %d", n, bufFrames)
	}
	samples := unsafe.Slice((*int16)(unsafe.Pointer(&dst[0])), bufFrames*Channels)

	// First frame should start smoothly at +10000.
	if samples[0] != 10000 {
		t.Fatalf("first sample after crossfade splice = %d, want 10000", samples[0])
	}

	// Verify there is no single-sample click > 500 across the crossfade window.
	maxStep := 0
	for i := 1; i < crossfadeFrames; i++ {
		diff := int(samples[i*Channels]) - int(samples[(i-1)*Channels])
		if diff < 0 {
			diff = -diff
		}
		if diff > maxStep {
			maxStep = diff
		}
	}
	// Blending +10000 to -10000 over 96 frames steps by ~208 per frame.
	if maxStep > 300 {
		t.Fatalf("max sample-to-sample step during crossfade = %d, want <= 300 (hard jump would be 20000)", maxStep)
	}
}

func TestDriftInterpolationIsContinuous(t *testing.T) {
	dev := &fakeDevice{}
	p := newTestPlayer("balanced", dev)
	var phase float64

	// Fill 80 ms (> 70 ms highWater) of a smooth 440 Hz tone.
	p.Write(makeToneFrames(SampleRate*80/1000, &phase))
	p.trimActive = true // exercise the resampler independently of the time gate

	dst := make([]byte, bufFrames*BytesPerFr)
	var prev int16
	havePrev := false
	maxStep := 0

	// Drain 3 consecutive buffers under active drift.
	for b := 0; b < 3; b++ {
		n := p.take(dst)
		if n != bufFrames {
			t.Fatalf("buf %d: take = %d, want %d", b, n, bufFrames)
		}
		samples := unsafe.Slice((*int16)(unsafe.Pointer(&dst[0])), bufFrames*Channels)
		for i := 0; i < bufFrames; i++ {
			cur := samples[i*Channels]
			if havePrev {
				diff := int(cur) - int(prev)
				if diff < 0 {
					diff = -diff
				}
				if diff > maxStep {
					maxStep = diff
				}
			}
			prev = cur
			havePrev = true
		}
	}

	// 440 Hz at amplitude 12000 steps by at most ~692 per frame; the fractional
	// phase carries across buffers, so drifted buffers may add at most ~1%.
	if maxStep > 750 {
		t.Fatalf("max sample step across drifted buffers = %d, want <= 750", maxStep)
	}
}
