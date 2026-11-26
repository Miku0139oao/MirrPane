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
	"errors"
	"fmt"
	"runtime"
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

// Windows waveOut constants.
const (
	waveMapper                 = 0xFFFFFFFF
	callbackNull               = 0x00000000
	callbackEvent              = 0x00050000
	whdrDone                   = 0x00000001
	waveFormatPCM              = 1
	threadPriorityTimeCritical = 15
)

var (
	winmm    = windows.NewLazySystemDLL("winmm.dll")
	avrt     = windows.NewLazySystemDLL("avrt.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procWaveOutOpen       = winmm.NewProc("waveOutOpen")
	procWaveOutClose      = winmm.NewProc("waveOutClose")
	procWaveOutPrepare    = winmm.NewProc("waveOutPrepareHeader")
	procWaveOutUnprepare  = winmm.NewProc("waveOutUnprepareHeader")
	procWaveOutWrite      = winmm.NewProc("waveOutWrite")
	procWaveOutReset      = winmm.NewProc("waveOutReset")
	procWaveOutGetNumDevs = winmm.NewProc("waveOutGetNumDevs")
	procWaveOutMessage    = winmm.NewProc("waveOutMessage")
	procTimeBeginPeriod   = winmm.NewProc("timeBeginPeriod")
	procTimeEndPeriod     = winmm.NewProc("timeEndPeriod")

	procAvSetMmThreadCharacteristicsW   = avrt.NewProc("AvSetMmThreadCharacteristicsW")
	procAvRevertMmThreadCharacteristics = avrt.NewProc("AvRevertMmThreadCharacteristics")
	procSetThreadPriority               = kernel32.NewProc("SetThreadPriority")
)

var errNoOutputDevice = errors.New("audioout: no output device")

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
func (p *Player) Generation() uint64 { return p.generation.Load() }

func (p *Player) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// hasDeviceLocked reports whether a device is available to submit to. Caller
// must hold devMu. An injected submit path only counts for the pre-supervisor
// tests that model a device without a handle.
func (p *Player) hasDeviceLocked() bool {
	return p.handle != 0 || (p.submitBuf != nil && !p.supervisor)
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

// elevatePumpThread binds the audio pump to one OS thread and joins Windows's
// MMCSS "Pro Audio" class so 60 fps D3D11VA/sws_scale work cannot preempt
// waveOut header refills.
func elevatePumpThread() func() {
	runtime.LockOSThread()
	var mmcss uintptr
	if procAvSetMmThreadCharacteristicsW.Find() == nil {
		if name, err := windows.UTF16PtrFromString("Pro Audio"); err == nil {
			var taskIndex uint32
			h, _, _ := procAvSetMmThreadCharacteristicsW.Call(
				uintptr(unsafe.Pointer(name)),
				uintptr(unsafe.Pointer(&taskIndex)))
			runtime.KeepAlive(name)
			mmcss = h
		}
	}
	if mmcss == 0 && procSetThreadPriority.Find() == nil {
		if th, err := windows.GetCurrentThread(); err == nil {
			procSetThreadPriority.Call(uintptr(th), uintptr(threadPriorityTimeCritical))
		}
	}
	return func() {
		if mmcss != 0 && procAvRevertMmThreadCharacteristics.Find() == nil {
			procAvRevertMmThreadCharacteristics.Call(mmcss)
		}
		runtime.UnlockOSThread()
	}
}

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

// Ask the same wave mapper used by waveOutOpen for the current endpoint ID.
// Two endpoints can share a model and display name, and wave IDs can be reused.
func defaultDeviceSignature() (string, error) {
	const (
		preferredGet = 0x2000 + 21 // DRVM_MAPPER_PREFERRED_GET, mmddk.h
		instanceID   = 0x0800 + 17 // DRV_QUERYFUNCTIONINSTANCEID
		instanceSize = 0x0800 + 18 // DRV_QUERYFUNCTIONINSTANCEIDSIZE
	)
	var id, flags, size uint32
	ret, _, _ := procWaveOutMessage.Call(waveMapper, preferredGet, uintptr(unsafe.Pointer(&id)), uintptr(unsafe.Pointer(&flags)))
	if ret != 0 {
		return "", fmt.Errorf("audioout: query preferred wave device: %d", ret)
	}
	if id == waveMapper {
		return "", errNoOutputDevice
	}
	ret, _, _ = procWaveOutMessage.Call(uintptr(id), instanceSize, uintptr(unsafe.Pointer(&size)), 0)
	if ret != 0 || size < 2 || size > 65536 || size%2 != 0 {
		return "", fmt.Errorf("audioout: query endpoint ID size: %d (%d bytes)", ret, size)
	}
	buf := make([]uint16, size/2)
	ret, _, _ = procWaveOutMessage.Call(uintptr(id), instanceID, uintptr(unsafe.Pointer(&buf[0])), uintptr(size))
	if ret != 0 {
		return "", fmt.Errorf("audioout: query endpoint ID: %d", ret)
	}
	return windows.UTF16ToString(buf), nil
}
