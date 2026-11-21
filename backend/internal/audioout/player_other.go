//go:build !windows

// Package audioout plays decoded audio on the default output device.
//
// Only the Windows backend exists so far; the other platforms get a player that
// opens successfully and discards audio, so the rest of the receiver still runs.
package audioout

// SampleRate and Channels describe the format Write accepts.
const (
	SampleRate = 48000
	Channels   = 2
)

// Player is a sink for decoded audio.
type Player struct{}

// Available reports whether an output device exists.
func Available() bool { return false }

// Open always succeeds; the discarded audio keeps the pipeline identical.
func Open() (*Player, error) { return &Player{}, nil }

// OpenWithProfile mirrors Open on platforms without waveOut.
func OpenWithProfile(string) (*Player, error) { return &Player{}, nil }

// Write discards the audio.
func (p *Player) Write(frames []int16) {}

func (p *Player) SetMuted(bool)     {}
func (p *Player) Muted() bool       { return false }
func (p *Player) SetVolume(float64) {}
func (p *Player) Volume() float64   { return 1 }

// Close releases the player.
func (p *Player) Close() {}
