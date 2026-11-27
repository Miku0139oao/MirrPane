//go:build windows

package audioout

import (
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// DevicePresent reports whether an output device is currently open.
func (p *Player) DevicePresent() bool {
	p.devMu.Lock()
	defer p.devMu.Unlock()
	return p.hasDeviceLocked()
}

// DeviceError returns the last endpoint fault (rejected write, stalled device,
// or changed default endpoint), or nil after the latest successful open.
func (p *Player) DeviceError() error {
	p.devMu.Lock()
	defer p.devMu.Unlock()
	return p.lastDevErr
}

// Generation counts Flush calls. The parent can use it to correlate a new media
// session with the queue that was dropped for it.

// hasDeviceLocked reports whether a device is available to submit to. Caller
// must hold devMu. An injected submit path only counts for the pre-supervisor
// tests that model a device without a handle.
func (p *Player) hasDeviceLocked() bool {
	return p.handle != 0 || (p.submitBuf != nil && !p.supervisor)
}

// openDeviceLocked opens WAVE_MAPPER and prepares every header. Caller holds
// devMu and guarantees no pump step is in progress.
func (p *Player) openDeviceLocked() error {
	if p.openDev != nil {
		if err := p.openDev(); err != nil {
			return err
		}
		if p.devSignature == "" && p.probeDev != nil {
			if sig, err := p.probeDev(); err == nil {
				p.devSignature = sig
			}
		}
		return nil
	}
	if p.availDev != nil && !p.availDev() {
		return errNoOutputDevice
	}

	format := waveformatex{
		FormatTag:      waveFormatPCM,
		Channels:       Channels,
		SamplesPerSec:  SampleRate,
		AvgBytesPerSec: SampleRate * BytesPerFr,
		BlockAlign:     BytesPerFr,
		BitsPerSample:  16,
	}
	event, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return fmt.Errorf("audioout: CreateEvent(dev): %w", err)
	}
	var handle uintptr
	ret, _, _ := procWaveOutOpen.Call(
		uintptr(unsafe.Pointer(&handle)),
		waveMapper,
		uintptr(unsafe.Pointer(&format)),
		uintptr(event), 0, callbackEvent)
	if ret != 0 {
		_ = windows.CloseHandle(event)
		return fmt.Errorf("audioout: waveOutOpen: %d", ret)
	}
	for i := range p.bufs {
		b := &p.bufs[i]
		b.prepared = false
		b.inFlight = false
		b.hdr.Flags = 0
		b.hdr.BufferLength = uint32(len(b.data))
		b.hdr.BytesRecorded = 0
		ret, _, _ := procWaveOutPrepare.Call(handle,
			uintptr(unsafe.Pointer(&b.hdr)), unsafe.Sizeof(wavehdr{}))
		if ret != 0 {
			procWaveOutClose.Call(handle)
			_ = windows.CloseHandle(event)
			return fmt.Errorf("audioout: waveOutPrepareHeader: %d", ret)
		}
		b.prepared = true
	}
	p.handle = handle
	p.devEvent = event
	p.devSignature, _ = p.probeSignatureLocked()
	return nil
}

func (p *Player) probeSignatureLocked() (string, error) {
	if p.probeDev == nil {
		return "", nil
	}
	return p.probeDev()
}

// closeDeviceLocked stops and releases the current device. Caller holds devMu.
func (p *Player) closeDeviceLocked() {
	if p.openDev != nil {
		if p.closeDev != nil {
			p.closeDev()
		}
		for i := range p.bufs {
			p.bufs[i].inFlight = false
			p.bufs[i].prepared = false
			p.bufs[i].hdr.Flags = 0
		}
		p.handle = 0
		p.devEvent = 0
		p.devSignature = ""
		return
	}
	if p.handle != 0 {
		procWaveOutReset.Call(p.handle)
		for i := range p.bufs {
			if p.bufs[i].prepared {
				procWaveOutUnprepare.Call(p.handle,
					uintptr(unsafe.Pointer(&p.bufs[i].hdr)), unsafe.Sizeof(wavehdr{}))
				p.bufs[i].prepared = false
			}
			p.bufs[i].inFlight = false
		}
		procWaveOutClose.Call(p.handle)
		p.handle = 0
	}
	if p.devEvent != 0 {
		_ = windows.CloseHandle(p.devEvent)
		p.devEvent = 0
	}
	p.devSignature = ""
}

// resetDeviceLocked stops playback and reclaims every queued header. Caller
// holds devMu; after this returns no stale completion can be attributed to old
// audio because the in-flight flags are gone and each header's done bit is
// cleared again right before its next write.
func (p *Player) resetDeviceLocked() {
	if p.resetDev != nil {
		p.resetDev()
	} else if p.handle != 0 {
		procWaveOutReset.Call(p.handle)
	}
	for i := range p.bufs {
		p.bufs[i].inFlight = false
		p.bufs[i].hdr.Flags &^= whdrDone
	}
	if p.devEvent != 0 {
		_ = windows.ResetEvent(p.devEvent)
	}
}

// bumpReopenDelay doubles the retry interval up to the cap.
func (p *Player) bumpReopenDelay() {
	if p.reopenDelay < reopenMinDelay {
		p.reopenDelay = reopenMinDelay
		return
	}
	p.reopenDelay *= 2
	if p.reopenDelay > reopenMaxDelay {
		p.reopenDelay = reopenMaxDelay
	}
}

// restartLocked drops the failed device and schedules a reopen. Caller holds
// devMu. When immediate is true the default endpoint just changed, so the retry
// happens on the next pump step instead of after a backoff.
func (p *Player) restartLocked(now time.Time, immediate bool) {
	p.closeDeviceLocked()
	p.dropQueued()
	p.playing = false
	p.primingUntil = time.Time{}
	p.lastProgress = time.Time{}
	if immediate {
		p.reopenDelay = 0
		p.reopenAt = now
		return
	}
	p.bumpReopenDelay()
	p.reopenAt = now.Add(p.reopenDelay)
}

// maybeReopenLocked opens a device again once the backoff expires. Caller holds
// devMu and no device is currently open.
func (p *Player) maybeReopenLocked(now time.Time) {
	if !p.supervisor || p.hasDeviceLocked() {
		return
	}
	if now.Before(p.reopenAt) {
		return
	}
	if p.availDev != nil && !p.availDev() {
		// No endpoint at all: checking again is cheap, opening is not. Keep the
		// retry cadence capped so a later plugin is still picked up.
		p.reopenAt = now.Add(reopenMaxDelay)
		return
	}
	if err := p.openDeviceLocked(); err != nil {
		p.lastDevErr = err
		p.bumpReopenDelay()
		p.reopenAt = now.Add(p.reopenDelay)
		return
	}
	p.lastDevErr = nil
	p.reopenDelay = 0
	if p.openedBefore {
		p.deviceReopens.Add(1)
	}
	p.openedBefore = true
	p.playing = false
	p.primingUntil = time.Time{}
	p.lastProgress = time.Time{}
	p.nextProbe = now.Add(endpointProbeInterval)
	// Audio queued while the device was gone is stale by the time it returns;
	// start the new endpoint with fresh packets instead of replaying it.
	p.dropQueued()
}
