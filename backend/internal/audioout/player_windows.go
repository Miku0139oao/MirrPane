//go:build windows

// Package audioout plays decoded audio on the default output device.
//
// It talks to winmm's waveOut API through syscalls rather than pulling in a
// cgo audio library: the product ships as a folder that runs on a clean machine,
// so every extra DLL or runtime is a cost. waveOut is part of Windows itself.
//
// A receiver player (NewReceiverPlayer) is long-lived and self-healing: it
// reopens the output when waveOutWrite is rejected, when headers stop coming
// back, or when Windows moves the default endpoint. OpenWithProfile remains the
// explicit one-shot constructor used by hardware tests.
package audioout

import (
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
	// bufCount is the balanced profile's max device queue (80 ms). Under live
	// 2K 60 fps D3D11VA/sws_scale load, five 10 ms headers starved WHDR_DONE
	// turnaround by ~23 ms/s; extra headers only fill during turnaround spikes
	// and do not add steady-state latency.
	bufCount = 8
	// ringFrames is the balanced profile's burst capacity, separate from its
	// normal backlog target. Larger bursts may temporarily add this much delay.
	ringFrames = SampleRate * 300 / 1000
	// trimTargetFrames is the normal backlog target after gentle catch-up.
	trimTargetFrames = bufFrames * 3
	// crossfadeFrames is the 2 ms splice window used when trimming overflow
	// so the waveform never suffers a hard step discontinuity.
	crossfadeFrames = SampleRate * 2 / 1000
	// Burst peaks alone do not prove clock drift. The queue must remain above
	// high-water for a full jitter window before correction can start.
	backlogWindow  = 100 * time.Millisecond
	startupCushion = 20 * time.Millisecond

	// stallTimeout is how long an in-flight header may go without completing
	// before the endpoint is considered gone. A healthy waveOut header returns
	// within tens of milliseconds even under 2K decode load.
	stallTimeout = 2 * time.Second
	// endpointProbeInterval bounds how long a changed default endpoint can go
	// unnoticed. Querying the wave mapper opens nothing, so probing is
	// cheap.
	endpointProbeInterval = 2 * time.Second
	// Reopen attempts back off while no device is usable. The cap keeps a later
	// plugin (or a driver restart) noticed within two seconds without polling
	// winmm tightly.
	reopenMinDelay = 250 * time.Millisecond
	reopenMaxDelay = 2 * time.Second
	// pumpTick is both the wait timeout and the fallback polling cadence.
	pumpTick = 3 * time.Millisecond
)

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
	// DeviceErrors counts detected endpoint failures: rejected writes, stalled
	// headers, and default-endpoint changes. DeviceReopens counts successful
	// recoveries after the first open.
	DeviceErrors  uint64
	DeviceReopens uint64
}

// Player is a waveOut device fed from a ring buffer.
//
// Write may be called from the network goroutine; Flush from a session-change
// goroutine; and Close from teardown. All three are serialised. A write that
// arrives after Close is ignored rather than touching a closed device handle.
//
// The pump goroutine owns the device handle and the WAVEHDR flags. Callers only
// submit through Flush, which takes the same lock the pump uses around the
// device, so a reset can never race a waveOutWrite.
type Player struct {
	mu     sync.Mutex // ring state, closed, event handles
	closed bool

	devMu sync.Mutex // device handle, headers, supervisor state (pump-owned)

	supervisor bool

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

	// Device state; devMu guards all of it once the pump is running.
	handle       uintptr
	devEvent     windows.Handle
	devSignature string
	lastProgress time.Time
	nextProbe    time.Time
	reopenAt     time.Time
	reopenDelay  time.Duration
	openedBefore bool
	lastDevErr   error

	muted      atomic.Bool
	attenMilli atomic.Int32 // 0 = 100% volume, 1000 = 0% volume (zero-value is full volume)

	underrunCount atomic.Uint64
	trimCount     atomic.Uint64
	driftCount    atomic.Uint64
	deviceErrors  atomic.Uint64
	deviceReopens atomic.Uint64
	generation    atomic.Uint64

	// Test seams. All are nil in production: the real winmm path is used.
	submitBuf func(b *buffer, frames int) error // replaces waveOutWrite
	openDev   func() error                      // replaces waveOutOpen + prepare
	closeDev  func()                            // replaces waveOutClose
	resetDev  func()                            // replaces waveOutReset
	availDev  func() bool                       // replaces Available
	probeDev  func() (string, error)            // replaces defaultDeviceSignature
	now       func() time.Time                  // nil uses the monotonic wall clock

	doneEvent windows.Handle // manual-reset event signaled by Close
	wakeEvent windows.Handle // auto-reset event signaled by Write/Flush
	timer1ms  bool
	wake      chan struct{}
	done      chan struct{}
	pumpDone  chan struct{}
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
		// Under live 2K 60 fps D3D11VA + sws_scale load, five 10 ms waveOut
		// buffers starved WHDR_DONE turnaround by ~23 ms/s; seven cover 60 ms
		// of driver/scheduler turnaround without raising steady-state latency.
		return 7, SampleRate * 250 / 1000, bufFrames * 2
	case "stable":
		return 10, SampleRate * 400 / 1000, bufFrames * 4
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

// newPlayer allocates the ring and device buffers but does not touch winmm
// beyond starting the 1 ms timer resolution. openDeviceLocked prepares headers.
func newPlayer(profile string, supervisor bool) *Player {
	count, ring, trim := profileParams(profile)
	p := &Player{
		supervisor: supervisor,
		bufs:       make([]buffer, count),
		ring:       make([]int16, ring*Channels),
		trimTarget: trim,
		highWater:  highWaterFramesFor(ring, trim),
		wake:       make(chan struct{}, 1),
		done:       make(chan struct{}),
		pumpDone:   make(chan struct{}),
		availDev:   Available,
		probeDev:   defaultDeviceSignature,
	}
	for i := range p.bufs {
		p.bufs[i].data = make([]byte, bufFrames*BytesPerFr)
		p.bufs[i].hdr.Data = uintptr(unsafe.Pointer(&p.bufs[i].data[0]))
		p.bufs[i].hdr.BufferLength = uint32(len(p.bufs[i].data))
	}
	if procTimeBeginPeriod.Find() == nil {
		if r, _, _ := procTimeBeginPeriod.Call(1); r == 0 {
			p.timer1ms = true
		}
	}
	return p
}

// initEvents creates the player-lifetime wake and stop events. It is optional:
// without them the pump falls back to a short ticker.
func (p *Player) initEvents() error {
	if p.doneEvent != 0 {
		return nil
	}
	doneEvent, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return fmt.Errorf("audioout: CreateEvent(done): %w", err)
	}
	wakeEvent, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		_ = windows.CloseHandle(doneEvent)
		return fmt.Errorf("audioout: CreateEvent(wake): %w", err)
	}
	p.doneEvent = doneEvent
	p.wakeEvent = wakeEvent
	return nil
}

func (p *Player) closeEventHandles() {
	p.mu.Lock()
	if p.wakeEvent != 0 {
		_ = windows.CloseHandle(p.wakeEvent)
		p.wakeEvent = 0
	}
	if p.doneEvent != 0 {
		_ = windows.CloseHandle(p.doneEvent)
		p.doneEvent = 0
	}
	p.mu.Unlock()
}

func (p *Player) endTimerPeriod() {
	if p.timer1ms {
		p.timer1ms = false
		procTimeEndPeriod.Call(1)
	}
}

// Open starts playback on the default output device with the balanced buffer profile.
func Open() (*Player, error) {
	return OpenWithProfile("balanced")
}

// OpenWithProfile starts playback using the named jitter-buffer profile
// ("low", "balanced", or "stable"). It opens the device eagerly and fails when
// none is present, which keeps hardware tests explicit. The returned player is
// still supervised, so a device lost later is reopened on its own.
func OpenWithProfile(profile string) (*Player, error) {
	if !Available() {
		return nil, errNoOutputDevice
	}
	p := newPlayer(profile, true)
	if err := p.initEvents(); err != nil {
		p.endTimerPeriod()
		return nil, err
	}

	p.devMu.Lock()
	err := p.openDeviceLocked()
	if err == nil {
		now := p.clockNow()
		p.lastProgress = time.Time{}
		p.nextProbe = now.Add(endpointProbeInterval)
		p.openedBefore = true
	}
	p.devMu.Unlock()
	if err != nil {
		p.closeEventHandles()
		p.endTimerPeriod()
		return nil, err
	}

	go p.pump()
	return p, nil
}

// NewReceiverPlayer returns the long-lived player used by the receiver.
//
// Unlike OpenWithProfile it never fails. A machine with no output device keeps
// the audio in a bounded ring; the device is opened as soon as one appears
// (including speakers plugged in while the receiver is running). A device that
// is rejected by the driver, stops returning headers, or stops being the
// default endpoint is closed and reopened with capped backoff. Volume and mute
// settings live on the player, so they survive every reopen.
func NewReceiverPlayer(profile string) *Player {
	p := newPlayer(profile, true)
	// Event handles only avoid 3 ms polling; the ticker fallback is correct.
	_ = p.initEvents()
	go p.pump()
	return p
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
		DeviceErrors:    p.deviceErrors.Load(),
		DeviceReopens:   p.deviceReopens.Load(),
	}
}
func (p *Player) Generation() uint64 { return p.generation.Load() }

func (p *Player) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}
func (p *Player) clockNow() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
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
	if p.wakeEvent != 0 {
		_ = windows.SetEvent(p.wakeEvent)
	}
	p.mu.Unlock()

	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// dropQueued discards the ring and its playback bookkeeping. The caller holds
// devMu (or is the constructor), so the pump cannot be inside take.
func (p *Player) dropQueued() {
	p.mu.Lock()
	p.queued = 0
	p.peakQueued = 0
	p.r, p.w = 0, 0
	p.trimActive = false
	p.trimCounted = false
	p.driftPhase = 0
	p.aboveHighWaterSince = time.Time{}
	p.mu.Unlock()
}

// Flush synchronously discards audio for a finished media session: everything
// still in the PCM ring and every header the device has not returned yet. The
// next Write starts a fresh session (priming cushion included). Volume and mute
// are untouched.
func (p *Player) Flush() {
	p.devMu.Lock()
	defer p.devMu.Unlock()
	if p.isClosed() {
		return
	}
	p.dropQueued()
	p.playing = false
	p.primingUntil = time.Time{}
	p.lastProgress = time.Time{}
	p.resetDeviceLocked()
	p.generation.Add(1)
	p.wakePump()
}

func (p *Player) wakePump() {
	p.mu.Lock()
	if p.wakeEvent != 0 {
		_ = windows.SetEvent(p.wakeEvent)
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
	doneEvent := p.doneEvent
	p.mu.Unlock()

	if doneEvent != 0 {
		_ = windows.SetEvent(doneEvent)
	}
	if p.done != nil {
		close(p.done)
	}
	if p.pumpDone != nil {
		<-p.pumpDone // the pump is the only other writer of the headers
	}

	p.endTimerPeriod()
	p.closeEventHandles()
}
