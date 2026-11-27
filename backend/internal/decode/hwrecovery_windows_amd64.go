//go:build windows && amd64

package decode

import (
	"fmt"
	"time"
)

// Hardware failure policy. A run of consecutive failed access units while
// D3D11VA is configured triggers recovery: either hwFailureFrames failures, or
// at least hwFailureMinFrames spanning hwFailureWindow (a static screen sends
// few frames, so a frame count alone could keep the receiver dark for long).
const (
	hwFailureFrames    = 30
	hwFailureMinFrames = 5
	hwFailureWindow    = time.Second
	hwDeviceRetries    = 1 // device recreations before permanent software mode
)

type hwState int

const (
	hwHealthy  hwState = iota // hardware configured, last access unit decoded
	hwDegraded                // hardware configured, consecutive failures below threshold
	hwSoftware                // permanently software for this decoder
)

func (s hwState) String() string {
	switch s {
	case hwHealthy:
		return "healthy"
	case hwDegraded:
		return "degraded"
	case hwSoftware:
		return "software"
	}
	return fmt.Sprintf("hwState(%d)", int(s))
}

type hwAction int

const (
	hwActionNone           hwAction = iota
	hwActionRecreateDevice          // rebuild with a fresh D3D11VA device
	hwActionUseSoftware             // rebuild in software and stay there
)

// hwFailureTracker is the pure decision logic behind hardware recovery. It
// holds no FFmpeg state so it can be tested without a GPU.
type hwFailureTracker struct {
	maxFrames, minFrames int
	window               time.Duration
	retries              int

	state        hwState
	consecutive  int
	firstFailure time.Time
}

func newHWFailureTracker() hwFailureTracker {
	return hwFailureTracker{
		maxFrames: hwFailureFrames,
		minFrames: hwFailureMinFrames,
		window:    hwFailureWindow,
		retries:   hwDeviceRetries,
	}
}

// success records a decoded picture and clears any failure run.
func (t *hwFailureTracker) success() {
	if t.state == hwSoftware {
		return
	}
	t.state = hwHealthy
	t.consecutive = 0
}

// failure records one failed access unit at now. When the run crosses the
// threshold it returns the recovery action and the run length that caused it,
// and starts counting afresh.
func (t *hwFailureTracker) failure(now time.Time) (hwAction, int) {
	if t.state == hwSoftware {
		return hwActionNone, 0
	}
	if t.consecutive == 0 {
		t.firstFailure = now
	}
	t.consecutive++
	t.state = hwDegraded
	run := t.consecutive
	if run < t.maxFrames && (run < t.minFrames || now.Sub(t.firstFailure) < t.window) {
		return hwActionNone, run
	}
	t.consecutive = 0
	if t.retries > 0 {
		t.retries--
		t.state = hwHealthy
		return hwActionRecreateDevice, run
	}
	t.state = hwSoftware
	return hwActionUseSoftware, run
}

// forceSoftware pins the tracker to software, e.g. when no device could be
// recreated.
func (t *hwFailureTracker) forceSoftware() {
	t.state = hwSoftware
	t.consecutive = 0
}
