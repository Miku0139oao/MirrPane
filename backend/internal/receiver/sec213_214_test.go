package receiver

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"runtime"
	"sync"
	"testing"

	"howett.net/plist"

	"github.com/miku0139/airplay-go/backend/internal/fairplay"
	"github.com/miku0139/airplay-go/backend/internal/pairing"
)

func startTrustedSender(t *testing.T, r *Receiver, connKey string, identity ed25519.PrivateKey, seed byte) string {
	t.Helper()
	fp := requestApproval(t, r, connKey, identity)
	if err := r.ApproveSender(fp); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(connKey, streamKeys(t, r, connKey, seed)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(connKey, streamRequest(110, 96)); err != nil {
		t.Fatal(err)
	}
	return fp
}

func TestSameIPLiveSessionPiggybackBlocked(t *testing.T) {
	r, m, a, _, _ := approvalReceiver(t)
	const (
		ownerConn      = "192.0.2.10:50001"
		attackerConn   = "192.0.2.10:50002"
		otherTrustConn = "192.0.2.10:50003"
		auxConn        = "192.0.2.10:59599"
		probeInGrace   = "192.0.2.10:50004"
	)

	var states []Status
	r.events.OnState = func(s Status, _ string) {
		states = append(states, s)
	}

	ownerID := senderIdentity(t)
	ownerFP := startTrustedSender(t, r, ownerConn, ownerID, 1)
	m.client, m.pending = true, false
	// Mirror data stream connected from the owner's sender IP.
	r.mu.Lock()
	r.recordStateLocked(StatusLive, "mirroring")
	r.mu.Unlock()
	r.flushState()

	origMirrorKey := append([]byte(nil), m.keys[0].Key...)
	origAudioKey := append([]byte(nil), a.keys[0].Key...)
	origAudioIV := append([]byte(nil), a.iv...)

	// Same-IP unverified attacker runs FairPlay and tries SETUP -> 403 ErrNotPaired.
	attackerKeys := streamKeys(t, r, attackerConn, 2)
	_, err := r.Setup(attackerConn, attackerKeys)
	wantProtocolError(t, err, 403, ErrNotPaired)
	_, err = r.Setup(attackerConn, streamRequest(110, 96))
	wantProtocolError(t, err, 403, ErrNotPaired)

	// Same-IP attacker sends audio-only and whole-session TEARDOWN -> ignored.
	audioBody, err := plist.Marshal(streamRequest(96), plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	r.Teardown(attackerConn, audioBody)
	r.Teardown(attackerConn, nil)

	// Same-IP second trusted device (different Ed25519 identity) -> 453 ErrBusy.
	otherID := senderIdentity(t)
	otherFP := requestApproval(t, r, otherTrustConn, otherID)
	if err := r.ApproveSender(otherFP); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(otherTrustConn, streamKeys(t, r, otherTrustConn, 3)); err != nil {
		t.Fatal(err)
	}
	_, err = r.Setup(otherTrustConn, streamRequest(110, 96))
	wantProtocolError(t, err, 453, ErrBusy)

	// Closing attacker / auxiliary / non-owner connections leaves owner live and starts no grace.
	clock := installGraceClock(r)
	r.DropSession(auxConn)
	r.DropSession(attackerConn)
	r.DropSession(otherTrustConn)

	if len(clock.timers) != 0 || states[len(states)-1] != StatusLive ||
		r.activeConn != ownerConn || r.activeRemote != "192.0.2.10" || r.activeFingerprint != ownerFP ||
		m.prepares != 1 || m.drops != 0 || m.resets != 0 || !m.client || !bytes.Equal(m.keys[0].Key, origMirrorKey) ||
		a.configures != 1 || a.resets != 0 || a.clears != 0 || !bytes.Equal(a.keys[0].Key, origAudioKey) || !bytes.Equal(a.iv, origAudioIV) {
		t.Fatal("same-IP non-owner traffic disturbed owner keys, media, state, or grace")
	}

	// Owner control close starts 5 s media grace and clears control proof immediately.
	r.DropSession(ownerConn)
	if len(clock.timers) != 1 {
		t.Fatalf("owner close started %d grace timers, want 1", len(clock.timers))
	}
	ownerCS := r.sessions[ownerConn]
	if ownerCS == nil || ownerCS.pair.Verified() || ownerCS.haveEKey || ownerCS.ecdh != nil {
		t.Fatal("owner control close retained pairing or stream key proof")
	}

	// Same-IP attacker probes and TEARDOWN during grace cannot cancel grace or drop media.
	_, _ = r.PairSetup(probeInGrace, make([]byte, pairing.SetupRequestLen))
	_, err = r.Setup(probeInGrace, streamKeys(t, r, probeInGrace, 4))
	wantProtocolError(t, err, 403, ErrNotPaired)
	r.Teardown(probeInGrace, nil)
	if clock.timers[0].stopped || m.drops != 0 || a.clears != 0 {
		t.Fatal("same-IP probe during grace canceled grace or dropped media")
	}

	clock.fireAll()
	if m.drops != 1 || m.resets != 1 || a.clears != 1 || states[len(states)-1] != StatusIdle ||
		r.activeConn != "" || r.activeRemote != "" || r.activeFingerprint != "" {
		t.Fatal("grace expiry did not release media and return to idle")
	}
}

func TestSameIPTrustedReconnectWithinGraceAndHandoff(t *testing.T) {
	t.Run("within grace on new port", func(t *testing.T) {
		r, m, a, _, _ := approvalReceiver(t)
		const oldConn = "192.0.2.10:50001"
		const newConn = "192.0.2.10:50099"
		id := senderIdentity(t)
		startTrustedSender(t, r, oldConn, id, 1)
		m.client, m.pending, a.busy = true, false, true

		clock := installGraceClock(r)
		r.DropSession(oldConn)
		if len(clock.timers) != 1 {
			t.Fatal("missing grace timer")
		}

		// Legitimate iPhone reconnects on a new ephemeral port with fresh verification.
		keys := streamKeys(t, r, newConn, 5)
		finishVerificationAs(t, r, newConn, id)
		if _, err := r.Setup(newConn, keys); err != nil {
			t.Fatalf("trusted reconnect key setup failed: %v", err)
		}
		if _, err := r.Setup(newConn, streamRequest(110, 96)); err != nil {
			t.Fatalf("trusted reconnect stream setup failed: %v", err)
		}
		clock.fireAll()
		if !clock.timers[0].stopped || r.activeConn != newConn || r.activeRemote != "192.0.2.10" ||
			r.sessions[oldConn] != nil || m.drops != 0 || a.clears != 0 || m.prepares != 2 || a.configures != 2 {
			t.Fatal("trusted same-IP reconnect within grace failed to adopt media cleanly")
		}
	})

	t.Run("same-fingerprint verified handoff over half-open socket", func(t *testing.T) {
		r, m, a, _, _ := approvalReceiver(t)
		const oldConn = "192.0.2.10:50001"
		const newConn = "192.0.2.10:50099"
		id := senderIdentity(t)
		startTrustedSender(t, r, oldConn, id, 1)
		m.client, m.pending, a.busy = true, false, true

		keys := streamKeys(t, r, newConn, 6)
		finishVerificationAs(t, r, newConn, id)
		if _, err := r.Setup(newConn, keys); err != nil {
			t.Fatalf("handoff key setup failed: %v", err)
		}
		if _, err := r.Setup(newConn, streamRequest(110, 96)); err != nil {
			t.Fatalf("handoff stream setup failed: %v", err)
		}
		// Late close of stale half-open socket must not disturb replacement connection.
		r.DropSession(oldConn)
		if r.activeConn != newConn || r.activeRemote != "192.0.2.10" ||
			r.sessions[newConn] == nil || r.sessions[oldConn] != nil || m.drops != 0 || a.clears != 0 {
			t.Fatal("stale control socket close disrupted verified same-identity handoff")
		}
	})
}

func TestSessionMapIsBoundedAndPreservesOwner(t *testing.T) {
	r, _, _, _, _ := approvalReceiver(t)
	const ownerConn = "192.0.2.10:50001"
	startTrustedSender(t, r, ownerConn, senderIdentity(t), 1)

	fpReq := make([]byte, fairplay.SetupRequestLen)
	fpReq[4] = 3
	for i := range maxSessions + 150 {
		conn := fmt.Sprintf("192.0.2.20:%d", 10000+i)
		if _, err := r.FPSetup(conn, fpReq); err != nil {
			t.Fatal(err)
		}
	}
	r.mu.Lock()
	n := len(r.sessions)
	ownerCS := r.sessions[ownerConn]
	active := r.activeConn
	r.mu.Unlock()
	if n > maxSessions || ownerCS == nil || active != ownerConn {
		t.Fatalf("sessions=%d (max %d), ownerCS=%v, active=%q", n, maxSessions, ownerCS != nil, active)
	}
}

func TestConnectingStateNeverOverwritesIdleOrLive(t *testing.T) {
	t.Run("connecting probe racing DropSession cannot leave StatusConnecting", func(t *testing.T) {
		r, _, _ := bindingReceiver(t)
		entered := make(chan struct{})
		release := make(chan struct{})
		var mu sync.Mutex
		var states []Status
		r.events.OnState = func(s Status, _ string) {
			mu.Lock()
			states = append(states, s)
			first := len(states) == 1
			mu.Unlock()
			if first {
				close(entered)
				<-release
			}
		}

		var wg sync.WaitGroup
		// First request occupies OnState so subsequent transitions queue deterministically.
		wg.Go(func() {
			_, _ = r.PairSetup("192.0.2.10:50001", make([]byte, pairing.SetupRequestLen))
		})
		<-entered

		// Second connection runs PairSetup then closes before the first OnState finishes.
		// Both flushState calls block on stateMu until release closes; DropSession's
		// StatusIdle must win and stale StatusConnecting must never fire after it.
		var raceWG sync.WaitGroup
		raceWG.Go(func() {
			_, _ = r.PairSetup("192.0.2.10:50002", make([]byte, pairing.SetupRequestLen))
		})
		for {
			r.mu.Lock()
			_, created := r.sessions["192.0.2.10:50002"]
			r.mu.Unlock()
			if created {
				break
			}
			runtime.Gosched()
		}
		raceWG.Go(func() {
			r.DropSession("192.0.2.10:50001")
			r.DropSession("192.0.2.10:50002")
		})
		for {
			r.mu.Lock()
			empty := len(r.sessions) == 0
			r.mu.Unlock()
			if empty {
				break
			}
			runtime.Gosched()
		}

		close(release)
		wg.Wait()
		raceWG.Wait()

		mu.Lock()
		defer mu.Unlock()
		if len(states) == 0 || states[len(states)-1] != StatusIdle {
			t.Fatalf("final UI state = %v, want StatusIdle", states)
		}
	})

	t.Run("connecting probe racing OnConnected cannot overwrite StatusLive", func(t *testing.T) {
		r, _, _ := bindingReceiver(t)
		entered := make(chan struct{})
		release := make(chan struct{})
		var mu sync.Mutex
		var states []Status
		r.events.OnState = func(s Status, _ string) {
			mu.Lock()
			states = append(states, s)
			first := len(states) == 1
			mu.Unlock()
			if first {
				close(entered)
				<-release
			}
		}

		var wg sync.WaitGroup
		wg.Go(func() {
			_, _ = r.PairSetup("192.0.2.10:50001", make([]byte, pairing.SetupRequestLen))
		})
		<-entered

		// Probe records StatusConnecting, then mirror goes live before OnState unblocks.
		var raceWG sync.WaitGroup
		raceWG.Go(func() {
			_, _ = r.PairSetup("192.0.2.10:50002", make([]byte, pairing.SetupRequestLen))
		})
		for {
			r.mu.Lock()
			_, created := r.sessions["192.0.2.10:50002"]
			r.mu.Unlock()
			if created {
				break
			}
			runtime.Gosched()
		}
		r.mu.Lock()
		r.cleanStop.Store(false)
		r.recordStateLocked(StatusLive, "mirroring")
		r.mu.Unlock()
		raceWG.Go(r.flushState)

		close(release)
		wg.Wait()
		raceWG.Wait()

		mu.Lock()
		defer mu.Unlock()
		if len(states) == 0 || states[len(states)-1] != StatusLive {
			t.Fatalf("final UI state = %v, want StatusLive", states)
		}
	})

	t.Run("concurrent PairSetup FPSetup DropSession and live transitions", func(t *testing.T) {
		r, _, _ := bindingReceiver(t)
		var mu sync.Mutex
		var states []Status
		r.events.OnState = func(s Status, _ string) {
			// Must never hold r.mu across OnState (lock order is stateMu -> r.mu).
			r.mu.Lock()
			r.mu.Unlock()
			mu.Lock()
			states = append(states, s)
			mu.Unlock()
		}

		fpReq := make([]byte, fairplay.SetupRequestLen)
		fpReq[4] = 3
		pairReq := make([]byte, pairing.SetupRequestLen)

		var wg sync.WaitGroup
		for i := range 8 {
			conn := fmt.Sprintf("192.0.2.10:%d", 50000+i)
			wg.Go(func() {
				for range 25 {
					_, _ = r.FPSetup(conn, fpReq)
					_, _ = r.PairSetup(conn, pairReq)
					r.DropSession(conn)
				}
			})
		}
		wg.Go(func() {
			for range 25 {
				r.mu.Lock()
				r.cleanStop.Store(false)
				r.recordStateLocked(StatusLive, "mirroring")
				r.mu.Unlock()
				r.flushState()
				r.Disconnect()
			}
		})
		wg.Wait()

		r.Disconnect()
		mu.Lock()
		last := states[len(states)-1]
		mu.Unlock()
		if last != StatusIdle {
			t.Fatalf("final state after disconnect = %v, want StatusIdle", last)
		}
	})
}
