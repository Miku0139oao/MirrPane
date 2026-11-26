//go:build windows

package audioout

import (
	"errors"
	"sync"
	"testing"
	"time"
	"unsafe"
)

// endpoint is a fake waveOut device for supervisor tests. Exactly one goroutine
// drives pumpStep unless a test explicitly starts the pump; readings after
// Close are ordered by pumpDone.
type endpoint struct {
	now        time.Duration
	openCalls  int
	closeCalls int
	resets     int
	submits    int
	frames     int
	failOpen   bool
	failWrite  bool
	available  bool
	signature  string
	complete   bool // mark submitted headers done immediately
	submitted  []*buffer
}

func (e *endpoint) advance(d time.Duration) { e.now += d }

func newSupervisedTestPlayer(ep *endpoint) *Player {
	p := &Player{
		supervisor: true,
		bufs:       make([]buffer, bufCount),
		ring:       make([]int16, ringFrames*Channels),
		trimTarget: trimTargetFrames,
		highWater:  highWaterFramesFor(ringFrames, trimTargetFrames),
		wake:       make(chan struct{}, 1),
		done:       make(chan struct{}),
		pumpDone:   make(chan struct{}),
		now:        func() time.Time { return time.Unix(0, int64(ep.now)) },
		availDev:   func() bool { return ep.available },
		probeDev: func() (string, error) {
			if !ep.available {
				return "", errors.New("no default device")
			}
			return ep.signature, nil
		},
	}
	for i := range p.bufs {
		p.bufs[i].data = make([]byte, bufFrames*BytesPerFr)
		p.bufs[i].hdr.Data = uintptr(unsafe.Pointer(&p.bufs[i].data[0]))
		p.bufs[i].hdr.BufferLength = uint32(len(p.bufs[i].data))
	}
	p.openDev = func() error {
		ep.openCalls++
		if ep.failOpen {
			return errors.New("audioout: waveOutOpen: 4")
		}
		p.handle = 1
		p.devSignature = ep.signature
		for i := range p.bufs {
			p.bufs[i].inFlight = false
			p.bufs[i].prepared = true
			p.bufs[i].hdr.Flags = 0
		}
		return nil
	}
	p.closeDev = func() {
		ep.closeCalls++
		for i := range p.bufs {
			p.bufs[i].inFlight = false
			p.bufs[i].prepared = false
			p.bufs[i].hdr.Flags = 0
		}
	}
	p.resetDev = func() {
		ep.resets++
		for i := range p.bufs {
			p.bufs[i].inFlight = false
			p.bufs[i].hdr.Flags &^= whdrDone
		}
	}
	p.submitBuf = func(b *buffer, frames int) error {
		if ep.failWrite {
			return errors.New("audioout: waveOutWrite: 5")
		}
		ep.submits++
		ep.frames += frames
		ep.submitted = append(ep.submitted, b)
		if ep.complete {
			b.hdr.Flags |= whdrDone
		}
		return nil
	}
	return p
}

// pumpFor submits queued audio, advancing the fake clock past the startup
// cushion between steps.
func pumpFor(p *Player, ep *endpoint, wantSubmits int) {
	for i := 0; i < 6 && ep.submits < wantSubmits; i++ {
		p.pumpStep()
		ep.advance(startupCushion + time.Millisecond)
	}
}

func silence(frames int) []int16 { return make([]int16, frames*Channels) }

func TestReceiverPlayerOpensOnFirstStepAndPlays(t *testing.T) {
	ep := &endpoint{available: true, signature: "Speakers"}
	p := newSupervisedTestPlayer(ep)

	p.pumpStep() // lazy open, no audio yet
	if ep.openCalls != 1 || !p.DevicePresent() {
		t.Fatalf("open on first step: calls=%d present=%v", ep.openCalls, p.DevicePresent())
	}
	if p.Generation() != 0 || p.Stats().DeviceReopens != 0 {
		t.Fatalf("initial open counted as recovery: gen=%d stats=%+v", p.Generation(), p.Stats())
	}

	p.Write(silence(bufFrames * 3))
	pumpFor(p, ep, 3)
	if ep.submits != 3 {
		t.Fatalf("submitted %d buffers, want 3", ep.submits)
	}
	if p.DeviceError() != nil {
		t.Fatalf("unexpected device error: %v", p.DeviceError())
	}
	if p.Stats().DeviceErrors != 0 {
		t.Fatalf("healthy start counted device errors: %+v", p.Stats())
	}
}

func TestRejectedWriteReopensAfterBackoffAndRecovers(t *testing.T) {
	ep := &endpoint{available: true, signature: "Speakers"}
	p := newSupervisedTestPlayer(ep)

	p.pumpStep()
	p.Write(silence(bufFrames * 3))
	pumpFor(p, ep, 3)

	// The driver rejects the next write. The rejected header must not be kept
	// in flight (it would never complete), and the stale queue must be dropped.
	ep.failWrite = true
	p.Write(silence(bufFrames * 2))
	p.pumpStep()

	if p.Stats().DeviceErrors != 1 {
		t.Fatalf("DeviceErrors = %d, want 1 (%+v)", p.Stats().DeviceErrors, p.Stats())
	}
	if ep.closeCalls != 1 || p.DevicePresent() {
		t.Fatalf("failed device not closed: closeCalls=%d present=%v", ep.closeCalls, p.DevicePresent())
	}
	if p.DeviceError() == nil {
		t.Fatal("DeviceError() is nil after rejected write")
	}
	p.mu.Lock()
	queued := p.queued
	p.mu.Unlock()
	if queued != 0 {
		t.Fatalf("stale PCM kept after device failure: queued=%d samples", queued)
	}
	for i := range p.bufs {
		if p.bufs[i].inFlight {
			t.Fatalf("buffer %d marked in flight after rejected write", i)
		}
	}

	// A write during the backoff window must not hammer waveOutOpen.
	p.Write(silence(bufFrames * 2))
	p.pumpStep()
	if ep.openCalls != 1 {
		t.Fatalf("reopened before backoff expired: openCalls=%d", ep.openCalls)
	}
	ep.advance(reopenMinDelay - time.Millisecond)
	p.pumpStep()
	if ep.openCalls != 1 {
		t.Fatalf("reopened %v after failure, want >= %v", reopenMinDelay-time.Millisecond, reopenMinDelay)
	}

	// The backoff expires: the next audio reopens the device and plays.
	ep.advance(time.Millisecond)
	ep.failWrite = false
	p.pumpStep()
	if ep.openCalls != 2 || !p.DevicePresent() {
		t.Fatalf("did not reopen after backoff: openCalls=%d present=%v", ep.openCalls, p.DevicePresent())
	}
	if p.DeviceError() != nil {
		t.Fatalf("device error not cleared by reopen: %v", p.DeviceError())
	}
	p.Write(silence(bufFrames * 3))
	pumpFor(p, ep, 6)
	if ep.submits != 6 {
		t.Fatalf("playback did not resume: submits=%d, want 6", ep.submits)
	}
	st := p.Stats()
	if st.DeviceReopens != 1 || st.DeviceErrors != 1 {
		t.Fatalf("recovery telemetry = %+v, want 1 error and 1 reopen", st)
	}
}

func TestAbsentEndpointRetriesAreCappedAndRecover(t *testing.T) {
	ep := &endpoint{available: false}
	p := newSupervisedTestPlayer(ep)

	p.pumpStep()
	if ep.openCalls != 0 {
		t.Fatalf("attempted open without a device: %d", ep.openCalls)
	}
	ep.advance(time.Second)
	p.pumpStep()
	if ep.openCalls != 0 {
		t.Fatalf("attempted open before the absence retry interval: %d", ep.openCalls)
	}

	// A device appears but the open still fails: retries must back off from
	// 250 ms and stop growing at the 2 s cap.
	ep.available = true
	ep.failOpen = true
	ep.advance(time.Second) // exactly the 2 s absence retry point
	p.pumpStep()
	if ep.openCalls != 1 {
		t.Fatalf("openCalls = %d after availability returned, want 1", ep.openCalls)
	}

	steps := []time.Duration{reopenMinDelay, reopenMinDelay * 2, reopenMinDelay * 4, reopenMaxDelay, reopenMaxDelay}
	for i, wait := range steps {
		ep.advance(wait - time.Millisecond)
		p.pumpStep()
		if got := ep.openCalls; got != i+1 {
			t.Fatalf("step %d: opened before the %v backoff elapsed (calls=%d)", i, wait, got)
		}
		ep.advance(time.Millisecond)
		p.pumpStep()
		if got := ep.openCalls; got != i+2 {
			t.Fatalf("step %d: did not retry after %v (calls=%d)", i, wait, got)
		}
	}

	// The speaker is usable again; the same player recovers without a restart.
	ep.failOpen = false
	ep.advance(reopenMaxDelay)
	p.pumpStep()
	if !p.DevicePresent() {
		t.Fatal("player did not recover after the endpoint appeared")
	}
	if p.DeviceError() != nil {
		t.Fatalf("DeviceError() = %v after recovery, want nil", p.DeviceError())
	}
	if p.Stats().DeviceErrors != 0 {
		t.Fatalf("open retries were reported as endpoint faults: %+v", p.Stats())
	}
}

func TestFlushDropsRingAndDeviceBuffersAndKeepsSettings(t *testing.T) {
	ep := &endpoint{available: true, signature: "Speakers"}
	p := newSupervisedTestPlayer(ep)

	p.pumpStep()
	p.Write(silence(bufFrames * 4))
	pumpFor(p, ep, 4)
	p.SetVolume(0.25)
	p.SetMuted(true)

	p.Flush()

	if p.Generation() != 1 {
		t.Fatalf("Generation = %d, want 1", p.Generation())
	}
	if ep.resets != 1 {
		t.Fatalf("device resets = %d, want 1", ep.resets)
	}
	p.mu.Lock()
	queued := p.queued
	p.mu.Unlock()
	if queued != 0 {
		t.Fatalf("Flush kept %d queued samples", queued)
	}
	for i := range p.bufs {
		if p.bufs[i].inFlight {
			t.Fatalf("buffer %d still in flight after Flush", i)
		}
	}
	if p.Volume() != 0.25 || !p.Muted() {
		t.Fatalf("Flush changed volume/mute: volume=%v muted=%v", p.Volume(), p.Muted())
	}

	// Stale completions from the previous session arrive after the flush.
	// They must not produce playback of discarded audio.
	for i := range p.bufs {
		p.bufs[i].hdr.Flags |= whdrDone
	}
	p.pumpStep()
	if ep.submits != 4 {
		t.Fatalf("stale completions resubmitted audio: submits=%d", ep.submits)
	}

	// The next session starts from fresh samples only.
	p.SetMuted(false)
	p.SetVolume(1)
	fresh := make([]int16, bufFrames*3*Channels)
	for i := range fresh {
		fresh[i] = 7
	}
	p.Write(fresh)
	pumpFor(p, ep, 7)
	if ep.submits != 7 {
		t.Fatalf("new session not submitted: submits=%d, want 7", ep.submits)
	}
	last := ep.submitted[len(ep.submitted)-1]
	got := unsafe.Slice((*int16)(unsafe.Pointer(&last.data[0])), len(last.data)/2)
	for i, s := range got {
		if s != 7 {
			t.Fatalf("new session sample %d = %d, want 7", i, s)
		}
	}
}

func TestChangedDefaultEndpointTriggersReopen(t *testing.T) {
	ep := &endpoint{available: true, signature: "Speakers", complete: true}
	p := newSupervisedTestPlayer(ep)

	p.pumpStep()
	p.Write(silence(bufFrames * 3))
	pumpFor(p, ep, 3)
	p.pumpStep() // reclaim the completed headers

	ep.signature = "Headphones"
	ep.advance(endpointProbeInterval)
	p.pumpStep()
	if p.Stats().DeviceErrors != 1 || ep.closeCalls != 1 {
		t.Fatalf("endpoint change not detected: stats=%+v closeCalls=%d", p.Stats(), ep.closeCalls)
	}
	if p.DevicePresent() {
		t.Fatal("old endpoint kept open after default changed")
	}

	// Audio that arrived while the switch was pending belongs to the old
	// endpoint; the reopen drops it instead of replaying it on the new one.
	p.Write(silence(bufFrames * 2))
	p.pumpStep()
	if ep.openCalls != 2 || !p.DevicePresent() {
		t.Fatalf("new default not opened: openCalls=%d present=%v", ep.openCalls, p.DevicePresent())
	}
	p.mu.Lock()
	queued := p.queued
	p.mu.Unlock()
	if queued != 0 || ep.submits != 3 {
		t.Fatalf("pre-switch audio was not dropped: queued=%d submits=%d", queued, ep.submits)
	}
	p.Write(silence(bufFrames * 2))
	pumpFor(p, ep, 5)
	if ep.submits != 5 {
		t.Fatalf("playback did not continue on the new endpoint: submits=%d", ep.submits)
	}
}

func TestStalledDeviceIsReopened(t *testing.T) {
	ep := &endpoint{available: true, signature: "Speakers"}
	p := newSupervisedTestPlayer(ep)

	p.pumpStep()
	p.Write(silence(bufFrames * 3))
	pumpFor(p, ep, 3)

	// The driver never returns the headers: the endpoint was removed without
	// a write error, which is the silent-death case from the review.
	ep.advance(stallTimeout + 50*time.Millisecond)
	p.pumpStep()
	if p.Stats().DeviceErrors != 1 || ep.closeCalls != 1 || p.DevicePresent() {
		t.Fatalf("stalled device not reset: stats=%+v closeCalls=%d present=%v",
			p.Stats(), ep.closeCalls, p.DevicePresent())
	}
	if err := p.DeviceError(); err == nil {
		t.Fatal("DeviceError() nil after stall")
	}

	ep.advance(reopenMinDelay)
	p.pumpStep()
	if !p.DevicePresent() {
		t.Fatal("device not reopened after stall backoff")
	}
	ep.submits = 0
	p.Write(silence(bufFrames * 2))
	pumpFor(p, ep, 2)
	if ep.submits != 2 {
		t.Fatalf("playback did not resume after stall: submits=%d", ep.submits)
	}
}

func TestCloseStopsSupervisorAndIsIdempotent(t *testing.T) {
	ep := &endpoint{available: true, signature: "Speakers"}
	p := newSupervisedTestPlayer(ep)
	go p.pump()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			packet := silence(bufFrames)
			for {
				select {
				case <-stop:
					return
				default:
					p.Write(packet)
					time.Sleep(time.Millisecond)
				}
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	for i := 0; i < 5; i++ {
		p.Flush()
		time.Sleep(2 * time.Millisecond)
	}
	close(stop)
	wg.Wait()

	closed := make(chan struct{})
	go func() {
		p.Close()
		p.Close() // idempotent
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not stop the pump")
	}
	if p.DevicePresent() {
		t.Fatal("device still open after Close")
	}
	// Writes and flushes after teardown must be harmless.
	p.Write(silence(bufFrames))
	p.Flush()
}
