//go:build windows

package audioout

import (
	"errors"
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// pump keeps the device fed and supervises it. It is the only goroutine that
// touches the headers after they are queued, so no locking is needed around
// them beyond devMu against Flush/Close.
func (p *Player) pump() {
	defer func() {
		p.devMu.Lock()
		if p.hasDeviceLocked() {
			p.closeDeviceLocked()
		}
		p.devMu.Unlock()
		close(p.pumpDone)
	}()
	restoreThread := elevatePumpThread()
	defer restoreThread()

	p.mu.Lock()
	haveStopEvent := p.doneEvent != 0
	p.mu.Unlock()

	var ticker *time.Ticker
	if !haveStopEvent {
		ticker = time.NewTicker(pumpTick)
		defer ticker.Stop()
	}

	for {
		p.mu.Lock()
		doneEvent, wakeEvent := p.doneEvent, p.wakeEvent
		p.mu.Unlock()

		if doneEvent != 0 {
			handles := make([]windows.Handle, 0, 3)
			handles = append(handles, doneEvent)
			if wakeEvent != 0 {
				handles = append(handles, wakeEvent)
			}
			p.devMu.Lock()
			if p.devEvent != 0 {
				handles = append(handles, p.devEvent)
			}
			p.devMu.Unlock()
			ev, _ := windows.WaitForMultipleObjects(handles, false, uint32(pumpTick/time.Millisecond))
			if ev == windows.WAIT_OBJECT_0 {
				return
			}
			if ev == windows.WAIT_FAILED {
				time.Sleep(pumpTick)
			}
		} else {
			select {
			case <-p.done:
				return
			case <-p.wake:
			case <-ticker.C:
			}
		}
		p.pumpStep()
	}
}

// pumpStep reclaims finished device buffers, supervises the endpoint, and
// queues any ready audio.
func (p *Player) pumpStep() {
	p.devMu.Lock()
	defer p.devMu.Unlock()
	if p.isClosed() {
		return
	}
	now := p.clockNow()

	if !p.hasDeviceLocked() {
		p.playing = false
		p.primingUntil = time.Time{}
		p.maybeReopenLocked(now)
		return
	}

	// Reclaim finished buffers and count how many are still playing.
	inFlight := 0
	progressed := false
	for i := range p.bufs {
		b := &p.bufs[i]
		if b.inFlight {
			if b.hdr.Flags&whdrDone != 0 {
				b.inFlight = false
				progressed = true
			} else {
				inFlight++
			}
		}
	}
	if progressed {
		p.lastProgress = now
	}

	if p.supervisor {
		// A header that never comes back means the endpoint stopped consuming:
		// it was unplugged, disabled, or the driver wedged. Reset before the
		// ring can overflow forever with inaudible audio.
		if inFlight > 0 && !p.lastProgress.IsZero() && now.Sub(p.lastProgress) > stallTimeout {
			p.deviceErrors.Add(1)
			p.lastDevErr = errors.New("audioout: output device stalled")
			p.restartLocked(now, false)
			return
		}
		// Windows does not migrate an open waveOut handle when the default
		// endpoint changes; the old device keeps playing. Compare the mapper's
		// current identity with the one captured at open.
		if p.probeDev != nil && !now.Before(p.nextProbe) {
			p.nextProbe = now.Add(endpointProbeInterval)
			sig, err := p.probeDev()
			if errors.Is(err, errNoOutputDevice) || (err == nil && sig != "" && p.devSignature != "" && sig != p.devSignature) {
				p.deviceErrors.Add(1)
				p.lastDevErr = errors.New("audioout: default output endpoint changed")
				p.restartLocked(now, true)
				return
			}
			if err == nil {
				p.devSignature = sig
			}
		}
	}

	// When playback is idle (at startup or after a pause), wait for a
	// 20 ms of data AND a 20 ms time cushion before submitting the first
	// buffers. A whole burst may arrive at once: starting it immediately would
	// exhaust the last full device buffer before the next burst arrives.
	if inFlight == 0 && !p.hasAtLeast(bufFrames*2) {
		p.primingUntil = time.Time{}
		if p.playing {
			p.playing = false
			p.underrunCount.Add(1)
		}
		return
	}
	if inFlight == 0 && !p.playing {
		now := p.clockNow()
		if p.primingUntil.IsZero() {
			p.primingUntil = now.Add(startupCushion)
		}
		if now.Before(p.primingUntil) {
			return
		}
		p.primingUntil = time.Time{}
	}

	wasIdle := inFlight == 0
	submitted := 0
	for i := range p.bufs {
		b := &p.bufs[i]
		if b.inFlight {
			continue
		}
		n := p.take(b.data)
		if n == 0 {
			break
		}
		b.hdr.BufferLength = uint32(n * BytesPerFr)
		b.hdr.BytesRecorded = uint32(n * BytesPerFr)
		// A finished buffer is rewritten in place: the header stays prepared
		// for the life of the player, only the done flag is cleared.
		b.hdr.Flags &^= whdrDone

		var err error
		if p.submitBuf != nil {
			err = p.submitBuf(b, n)
		} else {
			ret, _, _ := procWaveOutWrite.Call(p.handle,
				uintptr(unsafe.Pointer(&b.hdr)), unsafe.Sizeof(wavehdr{}))
			if ret != 0 {
				err = fmt.Errorf("audioout: waveOutWrite: %d", ret)
			}
		}
		if err != nil {
			// Only a header the driver accepted may be marked in flight. A
			// rejected one never completes, and treating it as playing would
			// exhaust the device queue and silence the receiver until restart.
			p.deviceErrors.Add(1)
			p.lastDevErr = err
			p.restartLocked(now, false)
			return
		}
		b.inFlight = true
		submitted++
		p.playing = true
	}
	if wasIdle && submitted > 0 {
		p.lastProgress = now
	}
}

// take copies one full buffer of audio, or returns 0 when fewer than one full
// buffer is queued. Leaving partial remainders in the ring (instead of padding
// them with zero silence) keeps the waveform continuous across AAC packet
// boundaries (~522 frames vs 480-frame device buffers).
//
// When backlog correction is active (after sustained highWater occupancy), it
// gently consumes up to 0.5% extra frames via linear interpolation until
// post-take occupancy returns to trimTarget.
func (p *Player) take(dst []byte) int {
	want := len(dst) / BytesPerFr
	if want == 0 {
		return 0
	}

	p.mu.Lock()
	if p.closed || p.queued/Channels < want {
		p.mu.Unlock()
		return 0
	}

	queuedFrames := p.queued / Channels
	target := p.effectiveTrimTarget()
	if queuedFrames-want > p.effectiveHighWater() {
		now := p.clockNow()
		if p.aboveHighWaterSince.IsZero() {
			p.aboveHighWaterSince = now
		} else if now.Sub(p.aboveHighWaterSince) >= backlogWindow {
			p.trimActive = true
		}
	} else {
		p.aboveHighWaterSince = time.Time{}
	}
	rate := 1.0
	if p.trimActive {
		postTake := queuedFrames - want
		if postTake <= target {
			p.trimActive = false
			p.trimCounted = false
		} else {
			extra := float64(want) * 0.005
			excess := float64(postTake-target) - p.driftPhase
			if extra > excess {
				extra = excess
			}
			if extra > 0 {
				rate += extra / float64(want)
			}
		}
	}

	out := unsafe.Slice((*int16)(unsafe.Pointer(&dst[0])), want*Channels)
	total := p.driftPhase + float64(want)*rate
	consumed := int(total)
	interpolate := rate > 1 || p.driftPhase > 0
	last := p.driftPhase + float64(want-1)*rate
	if interpolate && int(last)+1 >= queuedFrames {
		// Without the interpolation neighbour, preserve all available full
		// frames by copying straight. Never read an unwritten ring slot.
		interpolate = false
	}
	if !interpolate {
		samples := want * Channels
		for i := 0; i < samples; i++ {
			out[i] = p.ring[p.r]
			p.r = (p.r + 1) % len(p.ring)
		}
		p.queued -= samples
		p.driftPhase = 0
	} else {
		ringLen := len(p.ring)
		for i := 0; i < want; i++ {
			pos := p.driftPhase + float64(i)*rate
			j := int(pos)
			f := pos - float64(j)
			for c := 0; c < Channels; c++ {
				s0 := float64(p.ring[(p.r+j*Channels+c)%ringLen])
				s1 := float64(p.ring[(p.r+(j+1)*Channels+c)%ringLen])
				out[i*Channels+c] = int16(s0 + (s1-s0)*f)
			}
		}
		samples := consumed * Channels
		p.r = (p.r + samples) % ringLen
		p.queued -= samples
		p.driftPhase = total - float64(consumed)
		if rate > 1 && !p.trimCounted {
			p.driftCount.Add(1)
			p.trimCounted = true
		}
	}
	if p.queued/Channels <= target {
		p.trimActive = false
		p.trimCounted = false
	}
	p.mu.Unlock()
	return want
}

// hasAtLeast reports whether at least n frames are queued.
func (p *Player) hasAtLeast(n int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.queued >= n*Channels
}
