//go:build windows

package audioout

import (
	"os"
	"testing"
	"time"
	"unsafe"
)

// Opt-in hardware smoke test. Submit silence to the shared default device;
// neither the running receiver nor system volume/settings are changed.
func TestSilentDeviceMaintainsSampleRate(t *testing.T) {
	if os.Getenv("MIRRPANE_AUDIO_DEVICE_TEST") != "1" {
		t.Skip("set MIRRPANE_AUDIO_DEVICE_TEST=1 to exercise the real waveOut device")
	}
	for _, profile := range []string{"low", "balanced", "stable"} {
		t.Run(profile, func(t *testing.T) {
			p, err := OpenWithProfile(profile)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			const duration = 5 * time.Second
			packet := make([]int16, bufFrames*Channels)
			start := time.Now()
			nextPacket := time.Duration(0)
			inputFrames := 0
			var steadyStart time.Time
			var steadyFrames uint32
			for time.Since(start) < duration {
				now := time.Since(start)
				for nextPacket <= now {
					p.Write(packet)
					inputFrames += bufFrames
					nextPacket += 10 * time.Millisecond
				}
				if now >= time.Second && steadyStart.IsZero() {
					steadyFrames = deviceSamplePosition(t, p.handle)
					steadyStart = time.Now()
				}
				time.Sleep(3 * time.Millisecond)
			}
			played := deviceSamplePosition(t, p.handle)
			steadyRate := float64(played-steadyFrames) / time.Since(steadyStart).Seconds()
			stats := p.Stats()
			t.Logf("elapsed=%v inputFrames=%d playedFrames=%d steadyRate=%.2f Hz stats=%+v", time.Since(start), inputFrames, played, steadyRate, stats)
			// Allow startup, the driver's reporting delay and buffers still in
			// flight, but not the old sustained ~12 kframes/s throughput deficit.
			if int(played) < inputFrames-SampleRate*150/1000 || int(played) > inputFrames || steadyRate < SampleRate*0.99 || steadyRate > SampleRate*1.01 {
				t.Fatalf("device failed to keep up: played=%d input=%d steadyRate=%.2f Hz", played, inputFrames, steadyRate)
			}
			if stats.TrimCount != 0 || stats.UnderrunCount != 0 || stats.HighWaterMs >= 100 {
				t.Fatalf("steady 48 kHz source caused a backlog or starvation: %+v", stats)
			}
		})
	}
}

func deviceSamplePosition(t *testing.T, handle uintptr) uint32 {
	t.Helper()
	// MMTIME has a four-byte type and an eight-byte union. TIME_SAMPLES
	// measures device progress independently of our ring counters.
	position := struct {
		Kind, Frames uint32
		Padding      [4]byte
	}{Kind: 2}
	ret, _, _ := winmm.NewProc("waveOutGetPosition").Call(handle,
		uintptr(unsafe.Pointer(&position)), unsafe.Sizeof(position))
	if ret != 0 || position.Kind != 2 {
		t.Fatalf("waveOutGetPosition: result=%d format=%d", ret, position.Kind)
	}
	return position.Frames
}
