package receiver

import (
	"sync"
	"testing"
	"time"

	"github.com/miku0139/airplay-go/backend/internal/pairing"
)

type graceTimer struct {
	callback func()
	stopped  bool
}

func (timer *graceTimer) Stop() bool {
	wasStopped := timer.stopped
	timer.stopped = true
	return !wasStopped
}

type graceClock struct {
	timers []*graceTimer
	delays []time.Duration
}

func installGraceClock(r *Receiver) *graceClock {
	clock := &graceClock{}
	r.afterFunc = func(delay time.Duration, callback func()) disconnectTimer {
		timer := &graceTimer{callback: callback}
		clock.timers = append(clock.timers, timer)
		clock.delays = append(clock.delays, delay)
		return timer
	}
	return clock
}

// Deliberately run even stopped callbacks: time.AfterFunc may already have
// queued one when Stop runs, so generation checks must make it harmless.
func (clock *graceClock) fireAll() {
	for _, timer := range clock.timers {
		timer.callback()
	}
}

func TestControlCloseKeepsMediaUntilGraceExpires(t *testing.T) {
	r, m, a := bindingReceiver(t)
	const remote = "192.168.1.20"
	startSender(t, r, remote, 1)
	clock := installGraceClock(r)
	r.DropSession(remote)
	r.DropSession(remote)
	if len(clock.timers) != 1 || clock.delays[0] != 5*time.Second ||
		r.sessions[remote] == nil || m.drops != 0 || a.clears != 0 {
		t.Fatal("control close immediately dropped media or extended its deadline")
	}
	r.events.OnState = func(Status, string) {
		if !r.mu.TryLock() {
			t.Error("grace expiry invoked the state callback with the receiver lock held")
			return
		}
		r.mu.Unlock()
	}
	clock.fireAll()
	if r.sessions[remote] != nil || r.activeRemote != "" || m.drops != 1 || m.resets != 1 || a.clears != 1 {
		t.Fatal("expired grace retained the session or media keys")
	}
}

func TestPairingOrSetupRequestsCancelGrace(t *testing.T) {
	for _, action := range []string{"pairing", "setup"} {
		t.Run(action, func(t *testing.T) {
			r, m, a := bindingReceiver(t)
			const remote = "192.168.1.20"
			startSender(t, r, remote, 1)
			clock := installGraceClock(r)
			r.controlGrace = 2 * time.Second
			r.DropSession(remote)
			old := clock.timers[0]
			if clock.delays[0] != 2*time.Second {
				t.Fatal("injected grace duration was ignored")
			}
			switch action {
			case "pairing":
				if _, err := r.PairSetup(remote, make([]byte, pairing.SetupRequestLen)); err != nil {
					t.Fatal(err)
				}
			case "setup":
				_, err := r.Setup(remote, streamRequest(110))
				wantProtocolError(t, err, 403, ErrNotPaired)
			}
			old.callback()
			if !old.stopped || r.sessions[remote] == nil || r.activeRemote != remote || m.drops != 0 || a.clears != 0 {
				t.Fatal("canceled grace callback dropped a reconnected sender")
			}
			r.DropSession(remote)
			old.callback()
			if r.sessions[remote] == nil || r.sessions[remote].closeTimer == nil {
				t.Fatal("obsolete timer canceled a newer disconnect")
			}
			clock.timers[1].callback()
			if m.drops != 1 || a.clears != 1 {
				t.Fatal("new grace timer did not clean up")
			}
		})
	}
}

func TestAnotherRemoteCannotCancelOwnerGrace(t *testing.T) {
	r, m, _ := bindingReceiver(t)
	const remote = "192.168.1.20"
	startSender(t, r, remote, 1)
	clock := installGraceClock(r)
	r.DropSession(remote)
	streamKeys(t, r, "192.168.1.21", 2)
	clock.fireAll()
	if m.drops != 1 {
		t.Fatal("another IP extended the owner's grace period")
	}
}

func TestExplicitStopsStayImmediateAndCancelGrace(t *testing.T) {
	for _, action := range []string{"disconnect", "teardown", "stop", "clear trust"} {
		t.Run(action, func(t *testing.T) {
			r, m, a := bindingReceiver(t)
			const remote = "192.168.1.20"
			startSender(t, r, remote, 1)
			clock := installGraceClock(r)
			r.DropSession(remote)
			switch action {
			case "disconnect":
				r.Disconnect()
			case "teardown":
				r.Teardown(remote, nil)
			case "stop":
				r.DropAllSessions()
			case "clear trust":
				if err := r.ClearTrustedSenders(); err != nil {
					t.Fatal(err)
				}
			}
			if m.drops != 1 || a.clears != 1 || r.sessions[remote] != nil {
				t.Fatal("explicit stop waited for control-close grace")
			}
			clock.fireAll()
			if m.drops != 1 || a.clears != 1 {
				t.Fatal("obsolete timer repeated cleanup after explicit stop")
			}
		})
	}
}

func TestReceiverCloseCancelsGrace(t *testing.T) {
	r, m, _ := bindingReceiver(t)
	const remote = "192.168.1.20"
	startSender(t, r, remote, 1)
	r.timing = nil // This socket-free receiver has no actual timing listener.
	clock := installGraceClock(r)
	r.DropSession(remote)
	r.Close()
	clock.fireAll()
	if !clock.timers[0].stopped || m.drops != 0 {
		t.Fatal("closed receiver retained a live disconnect timer")
	}
}

func TestOldControlCloseCannotDropReplacementSender(t *testing.T) {
	r, m, a := bindingReceiver(t)
	const owner, next = "192.168.1.20", "192.168.1.21"
	startSender(t, r, owner, 1)
	clock := installGraceClock(r)
	r.DropSession(owner)
	m.client, m.pending, a.busy = false, false, false
	startSender(t, r, next, 2)
	clock.fireAll()
	if r.activeRemote != next || r.sessions[next] == nil || r.sessions[owner] != nil || m.drops != 0 || a.clears != 0 {
		t.Fatal("old owner's grace expiry dropped the replacement sender")
	}
}

func TestGraceCallbackAndNewRequestsAreSerialized(t *testing.T) {
	r, _, _ := bindingReceiver(t)
	const remote = "192.168.1.20"
	startSender(t, r, remote, 1)
	clock := installGraceClock(r)
	r.DropSession(remote)
	var wg sync.WaitGroup
	wg.Go(clock.timers[0].callback)
	for range 8 {
		wg.Go(func() { _, _ = r.PairSetup(remote, make([]byte, pairing.SetupRequestLen)) })
	}
	wg.Wait()
	if r.activeRemote != "" && r.sessions[remote] == nil {
		t.Fatal("timer/request race left media without an owning session")
	}
	r.Disconnect()
}

func TestFairPlayRequestsCannotCancelGrace(t *testing.T) {
	r, m, a := bindingReceiver(t)
	const remote = "192.168.1.20"
	startSender(t, r, remote, 1)
	clock := installGraceClock(r)
	r.DropSession(remote)
	timer := clock.timers[0]
	version := r.sessions[remote].closeVersion
	for range 20 {
		streamKeys(t, r, remote, 2)
	}
	if timer.stopped || r.sessions[remote].closeTimer != timer || r.sessions[remote].closeVersion != version {
		t.Fatal("FairPlay request canceled or extended the control-close timer")
	}
	clock.fireAll()
	if m.drops != 1 || a.clears != 1 || r.sessions[remote] != nil {
		t.Fatal("FairPlay traffic kept a closed control session alive")
	}
}
