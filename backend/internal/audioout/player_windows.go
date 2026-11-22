//go:build windows

// Package audioout plays decoded audio on the default output device.
//
// It talks to winmm's waveOut API through syscalls rather than pulling in a
// cgo audio library: the product ships as a folder that runs on a clean machine,
// so every extra DLL or runtime is a cost. waveOut is part of Windows itself.
package audioout

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The output format is fixed: the AAC decoder resamples everything to 48 kHz
// stereo signed 16-bit, which every Windows audio device accepts in shared mode.
const (
	SampleRate = 48000
	Channels   = 2
	BytesPerFr = Channels * 2
)

const (
	// bufFrames is one device buffer: 10 ms, matching Windows's shared-mode
	// audio engine period.
	bufFrames = SampleRate / 100
	// bufCount is how many buffers stay queued: 50 ms max in flight. waveOut
	// header-return latency needs more headroom than the playback duration alone.
	bufCount = 5
	// ringFrames is the balanced profile's burst capacity, separate from its
	// normal backlog target. Larger bursts may temporarily add this much delay.
	ringFrames = SampleRate * 250 / 1000
	// trimTargetFrames is the normal backlog target after gentle catch-up.
	trimTargetFrames = bufFrames * 3
	// crossfadeFrames is the 2 ms splice window used when trimming overflow
	// so the waveform never suffers a hard step discontinuity.
	crossfadeFrames = SampleRate * 2 / 1000
	// Burst peaks alone do not prove clock drift. The queue must remain above
	// high-water for a full jitter window before correction can start.
	backlogWindow  = 100 * time.Millisecond
	startupCushion = 20 * time.Millisecond
)

// Windows waveOut constants.
const (
	waveMapper    = 0xFFFFFFFF
	callbackNull  = 0x00000000
	whdrDone      = 0x00000001
	waveFormatPCM = 1
)

var (
	winmm = windows.NewLazySystemDLL("winmm.dll")

	procWaveOutOpen       = winmm.NewProc("waveOutOpen")
	procWaveOutClose      = winmm.NewProc("waveOutClose")
	procWaveOutPrepare    = winmm.NewProc("waveOutPrepareHeader")
	procWaveOutUnprepare  = winmm.NewProc("waveOutUnprepareHeader")
	procWaveOutWrite      = winmm.NewProc("waveOutWrite")
	procWaveOutReset      = winmm.NewProc("waveOutReset")
	procWaveOutGetNumDevs = winmm.NewProc("waveOutGetNumDevs")
)

// waveformatex mirrors the Win32 structure. Only the plain PCM fields are used.
type waveformatex struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	Size           uint16
}

// wavehdr mirrors WAVEHDR.
type wavehdr struct {
	Data          uintptr
	BufferLength  uint32
	BytesRecorded uint32
	User          uintptr
	Flags         uint32
	Loops         uint32
	Next          uintptr
	Reserved      uintptr
}

// buffer is one device buffer together with the header the driver reads.
//
// prepared is set once, when the header is handed to the driver. inFlight marks
// a buffer the driver has not finished with yet; the pump never rewrites one.
type buffer struct {
	hdr      wavehdr
	data     []byte
	prepared bool
	inFlight bool
}

// Stats holds a read-only telemetry snapshot of the audio output ring buffer.
type Stats struct {
	OccupancyMs     float64
	HighWaterMs     float64
	HighWaterMarkMs float64
	TargetMs        float64
	CapacityMs      float64
	UnderrunCount   uint64
	// TrimCount counts overflow splices that discard audio. Gentle rate
	// correction is counted separately, since it does not discard a queue.
	TrimCount  uint64
	DriftCount uint64
}

// Player is a waveOut device fed from a ring buffer.
//
// Write may be called from the network goroutine and Close from another one;
// both are serialised, and a write that arrives after Close is ignored rather
// than touching a closed device handle.
type Player struct {
	mu     sync.Mutex
	handle uintptr
	closed bool

	bufs       []buffer
	ring       []int16
	r, w       int
	queued     int
	peakQueued int
	trimTarget int
	highWater  int

	trimActive  bool
	trimCounted bool
	playing     bool
	// driftPhase is the fractional read position carried across buffers while
	// backlog drift is active, so resampling stays continuous at boundaries.
	driftPhase          float64
	aboveHighWaterSince time.Time
	primingUntil        time.Time // pump-owned; a time cushion even for whole bursts

	muted      atomic.Bool
	attenMilli atomic.Int32 // 0 = 100% volume, 1000 = 0% volume (zero-value is full volume)

	underrunCount atomic.Uint64
	trimCount     atomic.Uint64
	driftCount    atomic.Uint64

	submitBuf func(b *buffer, frames int) // nil in production; overridden in tests
	now       func() time.Time            // nil uses the monotonic wall clock

	wake     chan struct{}
	done     chan struct{}
	pumpDone chan struct{}
}

// Available reports whether the machine has a wave output device at all.
func Available() bool {
	n, _, _ := procWaveOutGetNumDevs.Call()
	return n > 0
}

func profileParams(profile string) (count, ring, trim int) {
	switch profile {
	case "low":
		// Capacity is jitter headroom, not steady-state latency: Wi-Fi delivers
		// audio in bursts. Keep burst capacity separate from the normal target;
		// only sustained backlog warrants gentle catch-up.
		// waveOut returns headers after the audio engine has consumed them.
		// Three 10 ms buffers delivered only ~36 kframes/s on the test device;
		// even four occasionally stalled. Five cover return/scheduling jitter.
		return 5, SampleRate * 200 / 1000, bufFrames * 2
	case "stable":
		return 6, SampleRate * 350 / 1000, bufFrames * 4
	default:
		return bufCount, ringFrames, trimTargetFrames
	}
}

func highWaterFramesFor(ringCap, trimTarget int) int {
	// Correct backlog gently once the queue is more than ~40 ms above target;
	// the rest of the capacity is burst headroom.
	water := trimTarget + SampleRate*40/1000
	if half := ringCap / 2; water > half {
		water = half
	}
	return water
}

func (p *Player) ringFramesCap() int {
	return len(p.ring) / Channels
}

func (p *Player) effectiveTrimTarget() int {
	if p.trimTarget > 0 {
		return p.trimTarget
	}
	return trimTargetFrames
}

func (p *Player) effectiveHighWater() int {
	if p.highWater > 0 {
		return p.highWater
	}
	return highWaterFramesFor(p.ringFramesCap(), p.effectiveTrimTarget())
}

// Open starts playback on the default output device with the balanced buffer profile.
func Open() (*Player, error) {
	return OpenWithProfile("balanced")
}

// OpenWithProfile starts playback using the named jitter-buffer profile
// ("low", "balanced", or "stable").
func OpenWithProfile(profile string) (*Player, error) {
	if !Available() {
		return nil, errors.New("audioout: no output device")
	}

	count, ring, trim := profileParams(profile)
	p := &Player{
		bufs:       make([]buffer, count),
		ring:       make([]int16, ring*Channels),
		trimTarget: trim,
		highWater:  highWaterFramesFor(ring, trim),
		wake:       make(chan struct{}, 1),
		done:       make(chan struct{}),
		pumpDone:   make(chan struct{}),
	}

	format := waveformatex{
		FormatTag:      waveFormatPCM,
		Channels:       Channels,
		SamplesPerSec:  SampleRate,
		AvgBytesPerSec: SampleRate * BytesPerFr,
		BlockAlign:     BytesPerFr,
		BitsPerSample:  16,
	}

	var handle uintptr
	ret, _, _ := procWaveOutOpen.Call(
		uintptr(unsafe.Pointer(&handle)),
		waveMapper,
		uintptr(unsafe.Pointer(&format)),
		callbackNull, 0, 0)
	if ret != 0 {
		return nil, fmt.Errorf("audioout: waveOutOpen: %d", ret)
	}
	p.handle = handle

	for i := range p.bufs {
		p.bufs[i].data = make([]byte, bufFrames*BytesPerFr)
		p.bufs[i].hdr.Data = uintptr(unsafe.Pointer(&p.bufs[i].data[0]))
		p.bufs[i].hdr.BufferLength = uint32(len(p.bufs[i].data))
		ret, _, _ := procWaveOutPrepare.Call(handle,
			uintptr(unsafe.Pointer(&p.bufs[i].hdr)), unsafe.Sizeof(wavehdr{}))
		if ret != 0 {
			// The pump has not started yet, so nothing else can touch the handle.
			procWaveOutClose.Call(handle)
			return nil, fmt.Errorf("audioout: waveOutPrepareHeader: %d", ret)
		}
		p.bufs[i].prepared = true
	}

	go p.pump()
	return p, nil
}

// SetMuted toggles silent output while keeping buffer timing intact.
func (p *Player) SetMuted(m bool) { p.muted.Store(m) }

// Muted reports whether output is currently muted.
func (p *Player) Muted() bool { return p.muted.Load() }

// SetVolume sets linear output gain in [0, 1].
func (p *Player) SetVolume(v float64) {
	if v < 0 {
		v = 0
	} else if v > 1 {
		v = 1
	}
	p.attenMilli.Store(int32((1.0-v)*1000 + 0.5))
}

// Volume reports the current output gain in [0, 1].
func (p *Player) Volume() float64 {
	return float64(1000-p.attenMilli.Load()) / 1000.0
}

// Stats returns a read-only telemetry snapshot of the player's buffer state.
func (p *Player) Stats() Stats {
	p.mu.Lock()
	queuedFrames := p.queued / Channels
	peakFrames := p.peakQueued / Channels
	targetFrames := p.effectiveTrimTarget()
	highWaterFrames := p.effectiveHighWater()
	capFrames := p.ringFramesCap()
	p.mu.Unlock()

	toMs := func(frames int) float64 {
		return float64(frames) * 1000.0 / float64(SampleRate)
	}
	return Stats{
		OccupancyMs:     toMs(queuedFrames),
		HighWaterMs:     toMs(peakFrames),
		HighWaterMarkMs: toMs(highWaterFrames),
		TargetMs:        toMs(targetFrames),
		CapacityMs:      toMs(capFrames),
		UnderrunCount:   p.underrunCount.Load(),
		TrimCount:       p.trimCount.Load(),
		DriftCount:      p.driftCount.Load(),
	}
}

// crossfadeDrop blends up to crossfadeFrames from the head being discarded into
// the kept tail starting at (p.r + drop) so an overflow jump never clicks.
// Caller must hold p.mu and ensure drop <= p.queued.
func (p *Player) crossfadeDrop(drop int) {
	keepSamples := p.queued - drop
	cfFrames := crossfadeFrames
	if maxFrames := keepSamples / Channels; cfFrames > maxFrames {
		cfFrames = maxFrames
	}
	if cfFrames <= 0 {
		return
	}
	var head [crossfadeFrames * Channels]int16
	cfSamples := cfFrames * Channels
	for i := 0; i < cfSamples; i++ {
		head[i] = p.ring[(p.r+i)%len(p.ring)]
	}
	for f := 0; f < cfFrames; f++ {
		wOut := int32(cfFrames - f)
		wIn := int32(f)
		for c := 0; c < Channels; c++ {
			idx := (p.r + drop + f*Channels + c) % len(p.ring)
			oldVal := int32(head[f*Channels+c])
			newVal := int32(p.ring[idx])
			p.ring[idx] = int16((oldVal*wOut + newVal*wIn) / int32(cfFrames))
		}
	}
}

// Write queues one decoded frame. Samples are interleaved 16-bit at
// SampleRate. The caller may reuse the slice as soon as this returns.
func (p *Player) Write(frames []int16) {
	if len(frames) == 0 {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	room := len(p.ring) - p.queued
	if len(frames) > room {
		// Overflow: trim down to the high-water mark rather than all the way to
		// the trim target, so one burst costs one moderate jump instead of
		// discarding the whole queue. Drift continues from there.
		target := p.effectiveHighWater()
		keep := target * Channels
		if keep > len(p.ring)-len(frames) {
			keep = len(p.ring) - len(frames)
		}
		if keep < 0 {
			keep = 0
		}
		drop := p.queued - keep
		if rem := drop % Channels; rem != 0 {
			drop += Channels - rem
		}
		if drop > p.queued {
			drop = p.queued
		}
		if drop > 0 {
			p.crossfadeDrop(drop)
			p.r = (p.r + drop) % len(p.ring)
			p.queued -= drop
		}
		if len(frames) > len(p.ring) {
			// Single write larger than the whole ring: keep only the newest tail.
			frames = frames[len(frames)-len(p.ring):]
		}
		p.trimCount.Add(1)
		p.trimActive = false
		p.trimCounted = false
		p.driftPhase = 0
		p.aboveHighWaterSince = time.Time{}
	}
	muted := p.muted.Load()
	gain := int32(1000) - p.attenMilli.Load()
	if muted || gain <= 0 {
		for range frames {
			p.ring[p.w] = 0
			p.w = (p.w + 1) % len(p.ring)
		}
	} else if gain < 1000 {
		for _, s := range frames {
			p.ring[p.w] = int16((int32(s) * gain) / 1000)
			p.w = (p.w + 1) % len(p.ring)
		}
	} else {
		for _, s := range frames {
			p.ring[p.w] = s
			p.w = (p.w + 1) % len(p.ring)
		}
	}
	p.queued += len(frames)
	if p.queued > p.peakQueued {
		p.peakQueued = p.queued
	}
	p.mu.Unlock()

	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Close stops the device and releases it. It waits for the fill loop to exit.
func (p *Player) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	handle := p.handle
	p.mu.Unlock()

	close(p.done)
	<-p.pumpDone // the pump is the only other writer of the headers

	if handle != 0 {
		procWaveOutReset.Call(handle)
		for i := range p.bufs {
			if p.bufs[i].prepared {
				procWaveOutUnprepare.Call(handle,
					uintptr(unsafe.Pointer(&p.bufs[i].hdr)), unsafe.Sizeof(wavehdr{}))
				p.bufs[i].prepared = false
			}
		}
		procWaveOutClose.Call(handle)
		p.mu.Lock()
		p.handle = 0
		p.mu.Unlock()
	}
}

// pump keeps the device fed. It is the only goroutine that touches the headers
// after they are queued, so no locking is needed around them.
func (p *Player) pump() {
	defer close(p.pumpDone)
	ticker := time.NewTicker(3 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
		case <-p.wake:
		}
		p.pumpStep()
	}
}

// pumpStep reclaims finished device buffers and queues any ready audio.
func (p *Player) pumpStep() {
	// Reclaim finished buffers and count how many are still playing.
	inFlight := 0
	for i := range p.bufs {
		b := &p.bufs[i]
		if b.inFlight {
			if b.hdr.Flags&whdrDone != 0 {
				b.inFlight = false
			} else {
				inFlight++
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
		if p.submitBuf != nil {
			p.submitBuf(b, n)
		} else {
			procWaveOutWrite.Call(p.handle,
				uintptr(unsafe.Pointer(&b.hdr)), unsafe.Sizeof(wavehdr{}))
		}
		b.inFlight = true
		p.playing = true
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

func (p *Player) clockNow() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}
