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
	// bufCount is how many buffers stay queued: 40 ms max in flight.
	bufCount = 4
	// ringFrames caps the producer-side queue at 100 ms: large enough to absorb
	// normal 30-60 ms Wi-Fi bursts without slicing the waveform, small enough
	// that audio can never drift hundreds of milliseconds behind video.
	ringFrames = SampleRate / 10
	// trimTargetFrames is the backlog kept when an overflow triggers a catch-up
	// jump (30 ms), avoiding repeated per-packet cuts.
	trimTargetFrames = bufFrames * 3
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
	trimTarget int

	muted      atomic.Bool
	attenMilli  atomic.Int32 // 0 = 100% volume, 1000 = 0% volume (zero-value is full volume)

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
		return 3, SampleRate * 80 / 1000, bufFrames * 2
	case "stable":
		return 6, SampleRate * 150 / 1000, bufFrames * 4
	default:
		return bufCount, ringFrames, trimTargetFrames
	}
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
		// Overflow: jump cleanly down to trimTarget rather than nibbling
		// off a few milliseconds on every subsequent packet (which clicks).
		target := p.trimTarget
		if target <= 0 {
			target = trimTargetFrames
		}
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
			p.r = (p.r + drop) % len(p.ring)
			p.queued -= drop
		}
		if len(frames) > len(p.ring) {
			// Single write larger than the whole ring: keep only the newest tail.
			frames = frames[len(frames)-len(p.ring):]
		}
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
		// 20 ms cushion before submitting the first buffers so Wi-Fi jitter on
		// packet #2 does not cause an immediate underrun click.
		if inFlight == 0 && !p.hasAtLeast(bufFrames*2) {
			continue
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
			procWaveOutWrite.Call(p.handle,
				uintptr(unsafe.Pointer(&b.hdr)), unsafe.Sizeof(wavehdr{}))
			b.inFlight = true
		}
	}
}

// take copies one full buffer of audio, or returns 0 when fewer than one full
// buffer is queued. Leaving partial remainders in the ring (instead of padding
// them with zero silence) keeps the waveform continuous across AAC packet
// boundaries (~522 frames vs 480-frame device buffers).
func (p *Player) take(dst []byte) int {
	want := len(dst) / BytesPerFr

	p.mu.Lock()
	if p.closed || p.queued/Channels < want {
		p.mu.Unlock()
		return 0
	}
	samples := want * Channels

	out := unsafe.Slice((*int16)(unsafe.Pointer(&dst[0])), samples)
	for i := 0; i < samples; i++ {
		out[i] = p.ring[p.r]
		p.r = (p.r + 1) % len(p.ring)
	}
	p.queued -= samples
	p.mu.Unlock()
	return want
}

// hasAtLeast reports whether at least n frames are queued.
func (p *Player) hasAtLeast(n int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.queued >= n*Channels
}
