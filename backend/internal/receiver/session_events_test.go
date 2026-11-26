package receiver

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"

	"howett.net/plist"

	"github.com/miku0139/airplay-go/backend/internal/mirror"
)

type stateEvent struct {
	status Status
	reason string
}

func recordStateEvents(t *testing.T, r *Receiver) func() []stateEvent {
	t.Helper()
	var mu sync.Mutex
	var events []stateEvent
	r.events.OnState = func(status Status, reason string) {
		// Every state callback must run with the receiver lock released.
		if !r.mu.TryLock() {
			t.Error("state callback ran with the receiver lock held")
		} else {
			r.mu.Unlock()
		}
		mu.Lock()
		events = append(events, stateEvent{status, reason})
		mu.Unlock()
	}
	return func() []stateEvent {
		mu.Lock()
		defer mu.Unlock()
		return append([]stateEvent(nil), events...)
	}
}

// P2.1: a successful trust clear revokes the session from every state the UI
// may already have shown, not only an undelivered "connecting".
func TestClearTrustReturnsIdleFromEveryDeliveredState(t *testing.T) {
	for _, tc := range []stateEvent{
		{StatusConnecting, "pair-setup"},
		{StatusLive, "mirroring"},
		{StatusLive, "audio"},
		{StatusInterrupted, "sender closed the stream"},
		{StatusIdle, "disconnected"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			r, _, _ := bindingReceiver(t)
			var sawClearIdle bool
			var mu sync.Mutex
			r.events.OnState = func(status Status, reason string) {
				if !r.mu.TryLock() {
					t.Error("state callback ran with the receiver lock held")
					return
				}
				r.mu.Unlock()
				mu.Lock()
				if status == StatusIdle && reason == "trust cleared" {
					sawClearIdle = true
				}
				mu.Unlock()
			}
			r.mu.Lock()
			r.recordStateLocked(tc.status, tc.reason)
			r.mu.Unlock()
			r.flushState()

			if err := r.ClearTrustedSenders(); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if !sawClearIdle {
				t.Fatalf("clear trust from %v did not deliver idle", tc.status)
			}
			if r.status != StatusIdle || r.pendingEmit {
				t.Fatalf("receiver state after clear = %v (pending %v), want idle", r.status, r.pendingEmit)
			}
		})
	}
}

// A trust clear ends real media too, so both consumers are told to discard
// buffered pictures and PCM.
func TestClearTrustResetsActiveMedia(t *testing.T) {
	r, m, a := bindingReceiver(t)
	const remote = "192.168.1.20:50000"
	verifySender(t, r, remote)
	keys := streamKeys(t, r, remote, 1)
	keys["name"] = "Phone"
	if _, err := r.Setup(remote, keys); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(remote, streamRequest(110, 96)); err != nil {
		t.Fatal(err)
	}
	gen := r.preparedSession
	r.handleMirrorConnected(gen, remote)
	videoResets, audioResets := 0, 0
	r.events.OnVideoReset = func() { videoResets++ }
	r.events.OnAudioReset = func() { audioResets++ }
	cleared := false
	r.events.OnClient = func(deviceID, name, model string) {
		if deviceID == "" && name == "" && model == "" {
			cleared = true
		}
	}
	if err := r.ClearTrustedSenders(); err != nil {
		t.Fatal(err)
	}
	if videoResets != 1 || audioResets != 1 || !cleared || m.drops != 1 || a.clears != 1 {
		t.Fatalf("trust clear resets = video %d audio %d, client cleared %v, drops %d, audio clears %d",
			videoResets, audioResets, cleared, m.drops, a.clears)
	}
	if r.videoSession != 0 || r.preparedSession != 0 || r.audioBound {
		t.Fatal("trust clear retained media ownership")
	}
}

// P2.10: a connect or frame callback that arrives after the user stopped the
// connection must not mark it live or publish its picture.
func TestStaleMirrorCallbacksCannotReviveStoppedSession(t *testing.T) {
	r, m, _ := bindingReceiver(t)
	const remote = "192.168.1.20:50000"
	verifySender(t, r, remote)
	if _, err := r.Setup(remote, streamKeys(t, r, remote, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(remote, streamRequest(110)); err != nil {
		t.Fatal(err)
	}
	gen := r.preparedSession
	if gen == 0 || gen != m.generation {
		t.Fatalf("setup generation = %d, mirror generation = %d", gen, m.generation)
	}
	frames, resets := 0, 0
	r.events.OnFrame = func(*mirror.Frame) { frames++ }
	r.events.OnVideoReset = func() { resets++ }

	r.handleMirrorConnected(gen, remote)
	r.handleMirrorFrame(&mirror.Frame{Session: gen})
	if frames != 1 || resets != 1 {
		t.Fatalf("live session frames/resets = %d/%d, want 1/1", frames, resets)
	}

	r.Disconnect()
	if r.status != StatusIdle || r.videoSession != 0 || r.preparedSession != 0 {
		t.Fatal("disconnect retained the session or its status")
	}

	// The mirror may deliver these after the stop; none may change state.
	r.handleMirrorConnected(gen, remote)
	r.handleMirrorFrame(&mirror.Frame{Session: gen})
	r.handleMirrorDisconnected(gen)
	if frames != 1 || resets != 2 {
		t.Fatalf("stopped session delivered frames/resets = %d/%d, want 1/2", frames, resets)
	}
	r.mu.Lock()
	status := r.status
	r.mu.Unlock()
	if status != StatusIdle {
		t.Fatalf("stopped session state = %v, want idle", status)
	}
}

// P2.10: a same-sender reconnect creates a new generation; the superseded
// generation's late close and frames cannot interrupt or repaint the new one.
func TestSupersededSessionCannotChangeNewerState(t *testing.T) {
	r, m, _ := bindingReceiver(t)
	const remote = "192.168.1.20:50000"
	verifySender(t, r, remote)
	if _, err := r.Setup(remote, streamKeys(t, r, remote, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(remote, streamRequest(110)); err != nil {
		t.Fatal(err)
	}
	first := r.preparedSession
	r.handleMirrorConnected(first, remote)

	// The same control connection authorizes a reconnect before the new data
	// connection arrives.
	if _, err := r.Setup(remote, streamRequest(110)); err != nil {
		t.Fatal(err)
	}
	second := r.preparedSession
	if second == 0 || second == first || second != m.generation {
		t.Fatalf("reconnect generation = %d, first = %d, mirror = %d", second, first, m.generation)
	}
	frames := 0
	r.events.OnFrame = func(*mirror.Frame) { frames++ }

	r.handleMirrorFrame(&mirror.Frame{Session: first})
	r.handleMirrorConnected(second, remote)
	r.handleMirrorDisconnected(first) // stale close of the superseded stream
	r.handleMirrorFrame(&mirror.Frame{Session: first})
	r.handleMirrorFrame(&mirror.Frame{Session: second})
	if frames != 2 { // the pre-reconnect frame and the new session's frame
		t.Fatalf("frames delivered = %d, want 2", frames)
	}
	r.mu.Lock()
	status, session := r.status, r.videoSession
	r.mu.Unlock()
	if status != StatusLive || session != second {
		t.Fatalf("state after stale close = %v session %d, want live session %d", status, session, second)
	}
}

// Legitimate split setup: audio can become live before the mirror connection
// exists, and the later video setup must not disturb the playing audio.
func TestSplitSetupKeepsAudioLiveUntilVideoConnects(t *testing.T) {
	r, m, a := bindingReceiver(t)
	states := recordStateEvents(t, r)
	const remote = "192.168.1.20:50000"
	verifySender(t, r, remote)
	if _, err := r.Setup(remote, streamKeys(t, r, remote, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(remote, streamRequest(96)); err != nil {
		t.Fatal(err)
	}
	all := states()
	if last := all[len(all)-1]; last.status != StatusLive || last.reason != "audio" {
		t.Fatalf("split audio state = %v/%q, want live/audio", last.status, last.reason)
	}
	if a.configures != 1 {
		t.Fatalf("audio configured %d times, want 1", a.configures)
	}
	if _, err := r.Setup(remote, streamRequest(110)); err != nil {
		t.Fatal(err)
	}
	gen := r.preparedSession
	if gen == 0 || m.prepares != 1 || a.configures != 1 {
		t.Fatalf("video half of split setup: gen %d prepares %d audio configs %d", gen, m.prepares, a.configures)
	}
	r.handleMirrorConnected(gen, remote)
	all = states()
	if last := all[len(all)-1]; last.status != StatusLive || last.reason != "mirroring" {
		t.Fatalf("state after video connect = %v/%q, want live/mirroring", last.status, last.reason)
	}
}

// P3.1: an audio-only sender becomes live with the audio reason, and ending
// that track returns the UI to idle.
func TestAudioOnlySessionReportsLiveAudioAndIdle(t *testing.T) {
	r, m, a := bindingReceiver(t)
	states := recordStateEvents(t, r)
	audioResets := 0
	r.events.OnAudioReset = func() { audioResets++ }
	const remote = "192.168.1.20:50000"
	verifySender(t, r, remote)
	if _, err := r.Setup(remote, streamKeys(t, r, remote, 1)); err != nil {
		t.Fatal(err)
	}
	audioReq := map[string]any{
		"name":     "Phone",
		"deviceID": "AA:BB:CC:DD:EE:FF",
		"model":    "iPhone16,2",
		"streams": []any{map[string]any{
			"type":        uint64(96),
			"ct":          uint64(8),
			"controlPort": uint64(54321),
		}},
	}
	if _, err := r.Setup(remote, audioReq); err != nil {
		t.Fatal(err)
	}
	if m.prepares != 0 {
		t.Fatal("audio-only setup prepared a mirror stream")
	}
	if a.configures != 1 || a.controlPort != 54321 || a.peer != netip.MustParseAddr("192.168.1.20") {
		t.Fatalf("audio binding = configures %d peer %v control %d", a.configures, a.peer, a.controlPort)
	}
	if audioResets != 1 {
		t.Fatalf("audio resets on first track = %d, want 1", audioResets)
	}
	all := states()
	last := all[len(all)-1]
	if last.status != StatusLive || last.reason != "audio" {
		t.Fatalf("audio-only state = %v/%q, want live/audio", last.status, last.reason)
	}

	body, err := plist.Marshal(audioReq, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	r.Teardown(remote, body)
	if a.resets != 1 || a.clears != 0 {
		t.Fatalf("audio teardown retained the stream: resets %d clears %d", a.resets, a.clears)
	}
	if audioResets != 2 {
		t.Fatalf("audio resets after teardown = %d, want 2", audioResets)
	}
	all = states()
	last = all[len(all)-1]
	if last.status != StatusIdle || last.reason != "audio ended" {
		t.Fatalf("audio teardown state = %v/%q, want idle/audio ended", last.status, last.reason)
	}
	if r.liveSource != "" {
		t.Fatalf("audio teardown retained live source %q", r.liveSource)
	}
}

// A missing sender control port is passed as 0 rather than guessed.
func TestAudioSetupWithoutSenderControlPort(t *testing.T) {
	r, _, a := bindingReceiver(t)
	const remote = "192.168.1.20:50000"
	verifySender(t, r, remote)
	if _, err := r.Setup(remote, streamKeys(t, r, remote, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(remote, streamRequest(96)); err != nil {
		t.Fatal(err)
	}
	if a.configures != 1 || a.controlPort != 0 {
		t.Fatalf("audio control port = %d (configures %d), want 0", a.controlPort, a.configures)
	}
}

// A duplicate setup, or a video-only reconnect from the same owner, must not
// reset audio that is already playing. New key material is a real reconnect.
func TestDuplicateSetupKeepsOngoingAudio(t *testing.T) {
	r, _, a := bindingReceiver(t)
	audioResets := 0
	r.events.OnAudioReset = func() { audioResets++ }
	const remote = "192.168.1.20:50000"
	startSender(t, r, remote, 1)
	if a.configures != 1 || audioResets != 1 {
		t.Fatalf("initial audio config = %d, resets %d", a.configures, audioResets)
	}

	if _, err := r.Setup(remote, streamRequest(110, 96)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(remote, streamRequest(110)); err != nil {
		t.Fatal(err)
	}
	if a.configures != 1 || audioResets != 1 {
		t.Fatalf("duplicate/video-only setup reset audio: configs %d resets %d", a.configures, audioResets)
	}

	if _, err := r.Setup(remote, streamKeys(t, r, remote, 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(remote, streamRequest(110, 96)); err != nil {
		t.Fatal(err)
	}
	if a.configures != 2 || audioResets != 2 {
		t.Fatalf("new stream keys did not reconfigure audio: configs %d resets %d", a.configures, audioResets)
	}
}

// P3.3: the approved sender's advertised identity reaches the UI and is
// cleared when the session ends. An unapproved sender never names it.
func TestOnClientNamesApprovedSenderAndClearsOnIdle(t *testing.T) {
	r, _, _ := bindingReceiver(t)
	var mu sync.Mutex
	var calls [][3]string
	r.events.OnClient = func(deviceID, name, model string) {
		mu.Lock()
		calls = append(calls, [3]string{deviceID, name, model})
		mu.Unlock()
	}
	const remote = "192.168.1.20:50000"
	verifySender(t, r, remote)
	keys := streamKeys(t, r, remote, 1)
	keys["name"] = "Alice's iPhone"
	keys["deviceID"] = "AA:BB:CC:DD:EE:FF"
	keys["model"] = "iPhone16,2"
	if _, err := r.Setup(remote, keys); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(remote, streamRequest(110)); err != nil {
		t.Fatal(err)
	}
	r.Disconnect()

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("client callbacks = %v, want identify then clear", calls)
	}
	want := [3]string{"AA:BB:CC:DD:EE:FF", "Alice's iPhone", "iPhone16,2"}
	if calls[0] != want {
		t.Fatalf("client identity = %v, want %v", calls[0], want)
	}
	if calls[1] != [3]string{} {
		t.Fatalf("idle did not clear the client: %v", calls[1])
	}
}

// A trusted sender refused because another one owns the stream must not
// overwrite the live sender's name in the UI.
func TestBusySenderDoesNotRenameClient(t *testing.T) {
	r, m, _ := bindingReceiver(t)
	const owner, other = "192.168.1.20:50000", "192.168.1.21:50001"
	startSender(t, r, owner, 1)
	m.client = true
	var names []string
	r.events.OnClient = func(_, name, _ string) { names = append(names, name) }
	verifySender(t, r, other)
	req := streamKeys(t, r, other, 2)
	req["name"] = "Intruder"
	req["streams"] = streamRequest(110, 96)["streams"]
	_, err := r.Setup(other, req)
	wantProtocolError(t, err, 453, ErrBusy)
	if len(names) != 0 {
		t.Fatalf("busy sender named the UI: %v", names)
	}
	r.mu.Lock()
	stored := r.clientName
	r.mu.Unlock()
	if stored != "" {
		t.Fatalf("busy sender replaced the stored client name with %q", stored)
	}
}

func TestUnapprovedSenderDoesNotNameClient(t *testing.T) {
	r, _, _ := bindingReceiver(t)
	r.trust = failingTrust{}
	named := false
	r.events.OnClient = func(string, string, string) { named = true }
	const remote = "192.168.1.20:50000"
	verifySender(t, r, remote)
	keys := streamKeys(t, r, remote, 1)
	keys["name"] = "Intruder"
	_, err := r.Setup(remote, keys)
	wantProtocolError(t, err, 403, ErrUntrusted)
	if named {
		t.Fatal("unapproved sender named the UI")
	}
}

// P2.2/P2.11 ordering: the consumer is told to reset before the first frame of
// a new session, and no older generation's picture survives that reset.
func TestVideoResetPrecedesNewSessionFrames(t *testing.T) {
	r, _, _ := bindingReceiver(t)
	var order []string
	r.events.OnVideoReset = func() { order = append(order, "reset") }
	r.events.OnFrame = func(*mirror.Frame) { order = append(order, "frame") }
	const remote = "192.168.1.20:50000"
	verifySender(t, r, remote)
	if _, err := r.Setup(remote, streamKeys(t, r, remote, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(remote, streamRequest(110)); err != nil {
		t.Fatal(err)
	}
	first := r.preparedSession
	r.handleMirrorConnected(first, remote)
	r.handleMirrorFrame(&mirror.Frame{Session: first})
	r.handleMirrorFrame(&mirror.Frame{Session: first + 100}) // unknown generation

	if _, err := r.Setup(remote, streamRequest(110)); err != nil {
		t.Fatal(err)
	}
	second := r.preparedSession
	r.handleMirrorConnected(second, remote)
	r.handleMirrorFrame(&mirror.Frame{Session: first}) // stale picture
	r.handleMirrorFrame(&mirror.Frame{Session: second})

	want := []string{"reset", "frame", "reset", "frame"}
	if len(order) != len(want) {
		t.Fatalf("callback order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("callback order = %v, want %v", order, want)
		}
	}
}

// Concurrent lifecycle callbacks must not deadlock or race the state machine.
func TestMirrorLifecycleCallbacksAreSafeUnderConcurrency(t *testing.T) {
	r, _, _ := bindingReceiver(t)
	const remote = "192.168.1.20:50000"
	verifySender(t, r, remote)
	if _, err := r.Setup(remote, streamKeys(t, r, remote, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(remote, streamRequest(110)); err != nil {
		t.Fatal(err)
	}
	gen := r.preparedSession
	// Racing goroutines may legitimately hold mu while a callback runs, so
	// lock-ownership is checked elsewhere; here the race detector is the probe.
	var callbacks atomic.Int64
	r.events.OnFrame = func(*mirror.Frame) { callbacks.Add(1) }
	r.events.OnVideoReset = func() { callbacks.Add(1) }
	r.events.OnState = func(Status, string) { callbacks.Add(1) }

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 25 {
				r.handleMirrorConnected(gen, remote)
				r.handleMirrorFrame(&mirror.Frame{Session: gen})
				r.handleMirrorDisconnected(gen)
				r.handleMirrorFrame(&mirror.Frame{Session: gen})
				r.Disconnect()
			}
		})
	}
	wg.Wait()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.videoSession != 0 || r.preparedSession != 0 || r.status != StatusIdle {
		t.Fatalf("concurrent lifecycle left session %d/%d status %v", r.videoSession, r.preparedSession, r.status)
	}
}
