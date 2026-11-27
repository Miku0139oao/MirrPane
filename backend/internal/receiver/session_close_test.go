package receiver

import (
	"bytes"
	"testing"

	"github.com/Miku0139oao/MirrPane/backend/internal/fairplay"
)

func TestLastControlClosePreventsSameIPPairingInheritance(t *testing.T) {
	r, m, a, store, _ := approvalReceiver(t)
	const remote = "192.168.1.20"
	identity := senderIdentity(t)
	fingerprint := requestApproval(t, r, remote, identity)
	if err := r.ApproveSender(fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(remote, streamKeys(t, r, remote, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(remote, streamRequest(110, 96)); err != nil {
		t.Fatal(err)
	}
	m.client, m.pending = true, false
	clock := installGraceClock(r)
	cs := r.sessions[remote]
	oldPair, oldFairPlay := cs.pair, cs.fp
	audioKey, audioIV := append([]byte(nil), a.keys[0].Key...), append([]byte(nil), a.iv...)
	r.DropSession(remote)
	if cs.pair == oldPair || cs.fp == oldFairPlay || cs.pair.Verified() || len(cs.pair.PeerPublicKey()) != 0 ||
		cs.ecdh != nil || cs.aes != nil || cs.iv != nil || cs.aesCandidates != nil || cs.haveEKey || cs.lastStreamID != 0 {
		t.Fatal("last control close retained pairing or derived key material")
	}
	if !m.client || m.drops != 0 || a.clears != 0 || !store.IsTrustedSender(fingerprint) {
		t.Fatal("control reset prematurely dropped media or owner approval")
	}
	attackerKeys := streamKeys(t, r, remote, 2)
	if clock.timers[0].stopped {
		t.Fatal("attacker's FairPlay exchange canceled grace")
	}
	_, err := r.Setup(remote, attackerKeys)
	wantProtocolError(t, err, 403, ErrNotPaired)
	if m.prepares != 1 || a.configures != 1 || !bytes.Equal(audioKey, a.keys[0].Key) || !bytes.Equal(audioIV, a.iv) {
		t.Fatal("same-IP attacker reconfigured the owner's media after control close")
	}
	// Even completing a new verification with key B cannot inherit key A's trust.
	finishVerificationAs(t, r, remote, senderIdentity(t))
	_, err = r.Setup(remote, attackerKeys)
	wantProtocolError(t, err, 403, ErrUntrusted)
	if m.prepares != 1 || a.configures != 1 {
		t.Fatal("different pairing identity inherited the former owner's setup")
	}
}

func TestTrustedReconnectReverifiesWithinMediaGrace(t *testing.T) {
	r, m, a, _, _ := approvalReceiver(t)
	const remote = "192.168.1.20"
	identity := senderIdentity(t)
	fingerprint := requestApproval(t, r, remote, identity)
	if err := r.ApproveSender(fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(remote, streamKeys(t, r, remote, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(remote, streamRequest(110, 96)); err != nil {
		t.Fatal(err)
	}
	m.client = true
	clock := installGraceClock(r)
	r.DropSession(remote)
	keys := streamKeys(t, r, remote, 3)
	if clock.timers[0].stopped {
		t.Fatal("FairPlay alone canceled reconnect grace")
	}
	finishVerificationAs(t, r, remote, identity)
	if _, err := r.Setup(remote, keys); err != nil {
		t.Fatalf("trusted sender could not restore fresh stream keys: %v", err)
	}
	if _, err := r.Setup(remote, streamRequest(110, 96)); err != nil {
		t.Fatalf("trusted sender reconnect failed: %v", err)
	}
	clock.fireAll() // Already queued callbacks must not tear down the reconnect.
	if !clock.timers[0].stopped || m.drops != 0 || a.clears != 0 || m.prepares != 2 || a.configures != 2 ||
		!r.sessions[remote].pair.Verified() || !r.sessions[remote].haveEKey {
		t.Fatal("fresh verified reconnect did not retain its media session")
	}
}

func TestLastUnauthorizedControlCloseReturnsToIdle(t *testing.T) {
	r, m, a := bindingReceiver(t)
	var states []Status
	r.events.OnState = func(status Status, _ string) {
		if !r.mu.TryLock() {
			t.Error("state callback ran with receiver lock held")
			return
		}
		r.mu.Unlock()
		states = append(states, status)
	}
	request := make([]byte, fairplay.SetupRequestLen)
	request[4] = 3
	for _, remote := range []string{"192.168.1.20", "192.168.1.21"} {
		if _, err := r.FPSetup(remote, request); err != nil {
			t.Fatal(err)
		}
		_, err := r.Setup(remote, map[string]any{"ekey": make([]byte, fairplay.EKeyLen), "eiv": make([]byte, 16)})
		wantProtocolError(t, err, 403, ErrNotPaired)
	}
	r.DropSession("192.168.1.20")
	if states[len(states)-1] != StatusConnecting {
		t.Fatal("first close hid the remaining connecting session")
	}
	r.DropSession("192.168.1.21")
	if states[len(states)-1] != StatusIdle || len(r.sessions) != 0 || m.drops != 0 || a.clears != 0 {
		t.Fatal("last unauthorized control close left connecting status or touched media")
	}
}

func TestUnauthorizedProbeDoesNotChangeLiveOwnerState(t *testing.T) {
	r, _, _ := bindingReceiver(t)
	startSender(t, r, "192.168.1.20", 1)
	last := StatusLive
	r.events.OnState = func(status Status, _ string) { last = status }
	request := make([]byte, fairplay.SetupRequestLen)
	request[4] = 3
	if _, err := r.FPSetup("192.168.1.21", request); err != nil {
		t.Fatal(err)
	}
	r.DropSession("192.168.1.21")
	if last != StatusLive {
		t.Fatal("unowned probe changed the live owner's UI state")
	}
}
