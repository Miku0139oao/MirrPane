//go:build windows

package audioout

import (
	"fmt"
	"math"
	"testing"
	"time"
	"unsafe"
)

func TestPeriodicBurstsPreserveDeviceCoverage(t *testing.T) {
	for _, profile := range []string{"low", "balanced", "stable"} {
		for _, packetFrames := range []int{480, 522} {
			t.Run(fmt.Sprintf("%s/%d", profile, packetFrames), func(t *testing.T) {
				dev := &fakeDevice{}
				p := newTestPlayer(profile, dev)
				var phase float64
				const packets = 6
				// Six 480-frame packets per 60 ms are exactly 48 kHz. Six
				// 522-frame packets need 65.25 ms; delivering them every 60 ms
				// would over-supply audio by 8.75%, independently of jitter.
				interval := time.Duration(packetFrames*packets) * time.Second / SampleRate
				nextBurst := time.Duration(0)
				for now := time.Duration(0); now <= 20*time.Second; now += 3 * time.Millisecond {
					dev.advanceTo(now)
					for nextBurst <= now {
						for range packets {
							p.Write(makeToneFrames(packetFrames, &phase))
						}
						nextBurst += interval
					}
					p.pumpStep()
				}
				st := p.Stats()
				if st.TrimCount != 0 || st.DriftCount != 0 || st.UnderrunCount != 0 || dev.gaps != 0 {
					t.Fatalf("periodic burst lost audio: stats=%+v, device gaps=%d (%v)", st, dev.gaps, dev.gapDuration)
				}
				if dev.submittedFrames < 20*SampleRate-bufFrames {
					t.Fatalf("device received only %d frames in 20 s", dev.submittedFrames)
				}
			})
		}
	}
}

func TestDelayedHeaderReturnDoesNotThrottlePlayback(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
		delay time.Duration
		ok    bool
	}{
		{"3_buffers_35ms", 3, 35 * time.Millisecond, false},
		{"4_buffers_35ms", 4, 35 * time.Millisecond, false},
		{"5_buffers_35ms_idle", 5, 35 * time.Millisecond, true},
		{"5_buffers_55ms_2k_load", 5, 55 * time.Millisecond, false},
		{"7_buffers_55ms_2k_load", 7, 55 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Model the real waveOut path, where WHDR_DONE can trail actual
			// playback by ~35 ms at idle and ~45-55 ms under live 2K 60 fps
			// D3D11VA/sws_scale load. A large ring alone cannot cover this
			// delay: the driver owns each header until returning it to the pump.
			dev := &fakeDevice{completionDelay: tc.delay}
			p := newTestPlayer("low", dev)
			p.bufs = p.bufs[:tc.count]
			var phase float64
			p.Write(makeToneFrames(bufFrames*8, &phase))
			nextPacket := 10 * time.Millisecond
			for now := time.Duration(0); now <= 10*time.Second; now += 3 * time.Millisecond {
				dev.advanceTo(now)
				for nextPacket <= now {
					p.Write(makeToneFrames(bufFrames, &phase))
					nextPacket += 10 * time.Millisecond
				}
				p.pumpStep()
			}
			st := p.Stats()
			if !tc.ok {
				// Negative controls: reproduce the bottleneck with undersized
				// device queues even though the ring already holds >= 200 ms.
				if dev.gaps == 0 || st.TrimCount == 0 {
					t.Fatalf("undersized device queue did not reproduce starvation and overflow: stats=%+v gaps=%d", st, dev.gaps)
				}
				return
			}
			if st.TrimCount != 0 || st.UnderrunCount != 0 || dev.gaps != 0 {
				t.Fatalf("header turnaround starved playback: stats=%+v gaps=%d (%v)", st, dev.gaps, dev.gapDuration)
			}
			if dev.submittedFrames < 10*SampleRate-int(startupCushion)*SampleRate/int(time.Second) {
				t.Fatalf("device rate was throttled: submitted %d frames in 10 s", dev.submittedFrames)
			}
		})
	}
}

func TestOverflowRetainsHighWaterAndIncomingPacket(t *testing.T) {
	for _, profile := range []string{"low", "balanced", "stable"} {
		t.Run(profile, func(t *testing.T) {
			p := newTestPlayer(profile, &fakeDevice{})
			p.Write(make([]int16, p.ringFramesCap()*Channels))
			p.Write(make([]int16, 522*Channels))
			want := (p.effectiveHighWater() + 522) * Channels
			if p.queued != want || p.Stats().TrimCount != 1 {
				t.Fatalf("overflow kept %d samples, want high-water plus packet (%d); stats=%+v", p.queued, want, p.Stats())
			}
			if (p.r+p.queued)%len(p.ring) != p.w {
				t.Fatal("overflow broke ring ordering")
			}
		})
	}
}

func TestHighWaterLeavesBurstHeadroom(t *testing.T) {
	for _, tc := range []struct{ capacity, target, want int }{
		{200, 20, 60}, {250, 30, 70}, {350, 40, 80}, {80, 20, 40},
	} {
		got := highWaterFramesFor(SampleRate*tc.capacity/1000, SampleRate*tc.target/1000)
		if got != SampleRate*tc.want/1000 {
			t.Errorf("high-water(%d ms, %d ms) = %.2f ms, want %d ms", tc.capacity, tc.target, float64(got)*1000/SampleRate, tc.want)
		}
	}
}

func TestOversizedWriteCountsOneTrimAndPreservesNewestTail(t *testing.T) {
	for _, prefix := range []bool{false, true} {
		t.Run(fmt.Sprint(prefix), func(t *testing.T) {
			p := newTestPlayer("low", &fakeDevice{})
			if prefix {
				p.Write(make([]int16, bufFrames*Channels))
			}
			input := make([]int16, len(p.ring)+600*Channels)
			for i := range input {
				input[i] = int16(i)
			}
			p.Write(input)
			if p.queued != len(p.ring) || p.Stats().TrimCount != 1 {
				t.Fatalf("oversized write accounting: queued=%d stats=%+v", p.queued, p.Stats())
			}
			dst := make([]byte, p.ringFramesCap()*BytesPerFr)
			if n := p.take(dst); n != p.ringFramesCap() {
				t.Fatalf("take = %d, want %d", n, p.ringFramesCap())
			}
			out := unsafe.Slice((*int16)(unsafe.Pointer(&dst[0])), len(p.ring))
			for i, want := range input[len(input)-len(p.ring):] {
				if out[i] != want {
					t.Fatalf("tail sample %d = %d, want %d", i, out[i], want)
				}
			}
		})
	}
}

func TestTakeDriftCarriesFractionAndOrderingAcrossWrappedBuffers(t *testing.T) {
	p := newTestPlayer("balanced", &fakeDevice{})
	// Start near the end so both interpolation neighbours and read positions
	// cross the physical ring boundary while the logical waveform stays linear.
	p.r, p.w = len(p.ring)-100, len(p.ring)-100
	const inputFrames = 6000
	input := make([]int16, inputFrames*Channels)
	for i := range inputFrames {
		input[i*Channels] = int16(i * 3)
		input[i*Channels+1] = int16(-i * 2)
	}
	p.Write(input)
	p.trimActive = true
	const rate = 1.005
	outputFrames := 0
	var previous int16
	for b, frames := range []int{480, 481, 479, 480} {
		dst := make([]byte, frames*BytesPerFr)
		if n := p.take(dst); n != frames {
			t.Fatalf("buffer %d returned %d frames, want %d", b, n, frames)
		}
		samples := unsafe.Slice((*int16)(unsafe.Pointer(&dst[0])), frames*Channels)
		for i := range frames {
			pos := float64(outputFrames+i) * rate
			for ch, slope := range []float64{3, -2} {
				want := int16(pos * slope)
				if math.Abs(float64(samples[i*Channels+ch])-float64(want)) > 1 {
					t.Fatalf("buffer %d frame %d ch %d = %d, want %d (continuous position %v)", b, i, ch, samples[i*Channels+ch], want, pos)
				}
			}
			if outputFrames+i > 0 && (samples[i*Channels]-previous < 2 || samples[i*Channels]-previous > 4) {
				t.Fatalf("boundary step or reversed samples at output frame %d", outputFrames+i)
			}
			previous = samples[i*Channels]
		}
		outputFrames += frames
		consumed := float64(outputFrames) * rate
		if got := inputFrames - p.queued/Channels; got != int(consumed) {
			t.Fatalf("consumed %d input frames, want %d", got, int(consumed))
		}
		if math.Abs(p.driftPhase-(consumed-math.Floor(consumed))) > 1e-8 {
			t.Fatalf("phase after buffer %d = %v, want %v", b, p.driftPhase, consumed-math.Floor(consumed))
		}
	}
	if p.Stats().TrimCount != 0 || p.Stats().DriftCount != 1 {
		t.Fatalf("gentle correction was confused with an overflow: %+v", p.Stats())
	}
}

func TestTakeStraightAndLookaheadFallbackPreserveSamples(t *testing.T) {
	for _, phase := range []float64{0, 0.75} {
		t.Run(fmt.Sprint(phase), func(t *testing.T) {
			p := newTestPlayer("low", &fakeDevice{})
			p.r, p.w = len(p.ring)-100, len(p.ring)-100
			input := make([]int16, bufFrames*Channels)
			for i := range input {
				input[i] = int16(i + 1)
			}
			p.Write(input)
			p.driftPhase = phase // nonzero phase requires an unavailable neighbour
			dst := make([]byte, bufFrames*BytesPerFr)
			if n := p.take(dst); n != bufFrames {
				t.Fatalf("take returned %d frames, want %d", n, bufFrames)
			}
			out := unsafe.Slice((*int16)(unsafe.Pointer(&dst[0])), len(input))
			for i, want := range input {
				if out[i] != want {
					t.Fatalf("sample %d = %d, want %d", i, out[i], want)
				}
			}
			if p.queued != 0 || p.r != p.w || p.driftPhase != 0 || p.take(dst) != 0 {
				t.Fatalf("straight copy lost sample accounting: queued=%d r=%d w=%d phase=%v", p.queued, p.r, p.w, p.driftPhase)
			}
		})
	}
}

func TestEndingDriftPreservesFractionalPosition(t *testing.T) {
	p := newTestPlayer("low", &fakeDevice{})
	input := make([]int16, bufFrames*2*Channels)
	for i := 0; i < len(input)/Channels; i++ {
		input[i*Channels], input[i*Channels+1] = int16(i*4), int16(-i*4)
	}
	p.Write(input)
	p.driftPhase = 0.4 // remainder left by the final rate-corrected buffer
	dst := make([]byte, bufFrames*BytesPerFr)
	if n := p.take(dst); n != bufFrames {
		t.Fatalf("take = %d, want %d", n, bufFrames)
	}
	out := unsafe.Slice((*int16)(unsafe.Pointer(&dst[0])), bufFrames*Channels)
	for i := range bufFrames {
		want := int16(float64(i*4) + 1.6)
		if out[i*Channels] != want || out[i*Channels+1] != -want {
			t.Fatalf("drift exit lost phase at frame %d: (%d, %d), want (%d, %d)", i, out[i*Channels], out[i*Channels+1], want, -want)
		}
	}
	if p.queued != bufFrames*Channels || math.Abs(p.driftPhase-0.4) > 1e-8 || p.Stats().DriftCount != 0 {
		t.Fatalf("normal-rate phase carry consumed the wrong samples: phase=%v queued=%d stats=%+v", p.driftPhase, p.queued, p.Stats())
	}
}
