//go:build !windows

// Package audioout plays decoded audio on the default output device.
//
// Only the Windows backend exists so far; the other platforms get a player that
// opens successfully and discards audio, so the rest of the receiver still runs.
// The API mirrors the Windows build so shared receiver code compiles unchanged.
package audioout

import "sync/atomic"

// SampleRate and Channels describe the format Write accepts.
const (
	SampleRate = 48000
	Channels   = 2
	BytesPerFr = Channels * 2
)

// Stats mirrors the Windows telemetry snapshot; every field stays zero here.
type Stats struct {
	OccupancyMs     float64
	HighWaterMs     float64
	HighWaterMarkMs float64
	TargetMs        float64
	CapacityMs      float64
	UnderrunCount   uint64
	TrimCount       uint64
	DriftCount      uint64
	DeviceErrors    uint64
	DeviceReopens   uint64
}

// Player is a sink for decoded audio.
type Player struct {
	muted      atomic.Bool
	attenMilli atomic.Int32
	generation atomic.Uint64
}

// Available reports whether an output device exists.
func Available() bool { return false }

// Open always succeeds; the discarded audio keeps the pipeline identical.
func Open() (*Player, error) { return &Player{}, nil }

// OpenWithProfile mirrors Open on platforms without waveOut.
func OpenWithProfile(string) (*Player, error) { return &Player{}, nil }

// NewReceiverPlayer mirrors the Windows long-lived controller. There is no
// device to supervise, so it simply accepts and discards audio.
func NewReceiverPlayer(string) *Player { return &Player{} }

// Write discards the audio.
func (p *Player) Write(frames []int16) {}

// Flush drops any notion of queued audio and advances the session generation.
func (p *Player) Flush() { p.generation.Add(1) }

// Generation counts Flush calls.
func (p *Player) Generation() uint64 { return p.generation.Load() }

func (p *Player) SetMuted(m bool) { p.muted.Store(m) }
func (p *Player) Muted() bool     { return p.muted.Load() }

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

// Stats returns a zero snapshot.
func (p *Player) Stats() Stats { return Stats{} }

// DevicePresent is always false without a backend.
func (p *Player) DevicePresent() bool { return false }

// DeviceError is always nil without a backend.
func (p *Player) DeviceError() error { return nil }

// Close releases the player.
func (p *Player) Close() {}
