package receiver

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/Miku0139oao/MirrPane/backend/internal/protocol"
	"github.com/Miku0139oao/MirrPane/backend/internal/trust"
)

func senderIdentity(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func finishVerificationAs(t *testing.T, r *Receiver, remote string, priv ed25519.PrivateKey) {
	t.Helper()
	step2 := beginVerificationAs(t, r, remote, priv)
	if _, err := r.PairVerify(remote, step2); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerApprovalPersistsAndAllowsTrustedReconnect(t *testing.T) {
	r, m, a := bindingReceiver(t)
	dir := t.TempDir()
	store, err := trust.Open(dir, r.keys.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	r.trust = store
	var logs bytes.Buffer
	r.log = log.New(&logs, "", 0)
	const owner, other = "192.168.1.20", "192.168.1.21"
	identity := senderIdentity(t)
	public := identity.Public().(ed25519.PublicKey)
	fingerprint := trust.Fingerprint(public)
	keys := streamKeys(t, r, owner, 1)
	_, err = r.Setup(owner, keys)
	wantProtocolError(t, err, 403, ErrNotPaired)
	if len(r.PendingSenders()) != 0 {
		t.Fatal("unverified sender created an approval request")
	}
	step2 := beginVerificationAs(t, r, owner, identity)
	if store.IsTrustedSender(fingerprint) {
		t.Fatal("incomplete verification enrolled an identity")
	}
	if _, err := r.PairVerify(owner, step2); err != nil {
		t.Fatal(err)
	}
	unknown := senderIdentity(t)
	finishVerificationAs(t, r, other, unknown)
	unknownFingerprint := trust.Fingerprint(unknown.Public().(ed25519.PublicKey))
	if store.IsTrustedSender(fingerprint) || store.IsTrustedSender(unknownFingerprint) || len(r.PendingSenders()) != 0 {
		t.Fatal("pair-verify alone created trust or pending approval")
	}
	_, err = r.Setup(owner, keys)
	wantProtocolError(t, err, 403, ErrUntrusted)
	_, err = r.Setup(other, streamKeys(t, r, other, 2))
	wantProtocolError(t, err, 403, ErrUntrusted)
	if len(r.PendingSenders()) != 2 || store.IsTrustedSender(fingerprint) || store.IsTrustedSender(unknownFingerprint) ||
		m.prepares != 0 || a.configures != 0 || r.sessions[owner].haveEKey {
		t.Fatal("unknown setup automatically approved a sender or configured media")
	}
	if err := r.ApproveSender(fingerprint); err != nil {
		t.Fatal(err)
	}
	if !store.IsTrustedSender(fingerprint) || !strings.Contains(logs.String(), "approved sender "+fingerprint) ||
		strings.Contains(logs.String(), hex.EncodeToString(public)) {
		t.Fatal("owner approval failed to persist/log its fingerprint or disclosed the pairing key")
	}
	reloaded, err := trust.Open(dir, r.keys.PublicKey())
	if err != nil || !reloaded.IsTrustedSender(fingerprint) {
		t.Fatalf("approved sender was not persisted: %v", err)
	}
	if _, err := r.Setup(owner, keys); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(owner, streamRequest(110, 96)); err != nil {
		t.Fatal(err)
	}
	for _, req := range []map[string]any{streamKeys(t, r, other, 2), streamRequest(110), streamRequest(96)} {
		_, err := r.Setup(other, req)
		wantProtocolError(t, err, 403, ErrUntrusted)
	}
	if a.configures != 1 || m.prepares != 1 || r.sessions[other].haveEKey ||
		store.IsTrustedSender(trust.Fingerprint(unknown.Public().(ed25519.PublicKey))) {
		t.Fatal("unknown verified sender changed stream state or extended trust")
	}
	r.Teardown(owner, nil)
	r.trust = reloaded // Reconnect uses the persisted policy, not a session flag.
	finishVerificationAs(t, r, owner, identity)
	if _, err := r.Setup(owner, streamKeys(t, r, owner, 3)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(owner, streamRequest(110, 96)); err != nil {
		t.Fatal(err)
	}
	if m.prepares != 2 || a.configures != 2 || strings.Count(logs.String(), "approved sender ") != 1 {
		t.Fatal("trusted reconnect failed or required another approval")
	}
}

func TestClearingTrustRevokesSessionsAndRequiresNewApproval(t *testing.T) {
	r, m, a := bindingReceiver(t)
	store, err := trust.Open(t.TempDir(), r.keys.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	r.trust = store
	const owner, other = "192.168.1.20", "192.168.1.21"
	first, second := senderIdentity(t), senderIdentity(t)
	finishVerificationAs(t, r, owner, first)
	keys := streamKeys(t, r, owner, 1)
	_, err = r.Setup(owner, keys)
	wantProtocolError(t, err, 403, ErrUntrusted)
	if err := r.ApproveSender(trust.Fingerprint(first.Public().(ed25519.PublicKey))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(owner, keys); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(owner, streamRequest(110)); err != nil {
		t.Fatal(err)
	}
	// Setup checks current trust, even when the pairing session is still verified.
	if err := store.ClearTrustedSenders(); err != nil {
		t.Fatal(err)
	}
	_, err = r.Setup(owner, streamRequest(110))
	wantProtocolError(t, err, 403, ErrUntrusted)
	if err := r.ClearTrustedSenders(); err != nil {
		t.Fatal(err)
	}
	if len(r.sessions) != 0 || r.activeRemote != "" || m.drops != 1 || a.clears != 1 {
		t.Fatal("owner-directed trust reset retained sessions/media")
	}
	seed := append([]byte(nil), r.keys.Seed()...)
	if err := r.ClearTrustedSenders(); err != nil || !bytes.Equal(seed, r.keys.Seed()) || len(r.sessions) != 0 {
		t.Fatalf("repeated trust reset failed or rotated the receiver identity: %v", err)
	}
	finishVerificationAs(t, r, other, second)
	keys = streamKeys(t, r, other, 2)
	_, err = r.Setup(other, keys)
	wantProtocolError(t, err, 403, ErrUntrusted)
	if store.IsTrustedSender(trust.Fingerprint(second.Public().(ed25519.PublicKey))) {
		t.Fatal("reset automatically trusted a new sender")
	}
	if err := r.ApproveSender(trust.Fingerprint(second.Public().(ed25519.PublicKey))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(other, keys); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(other, streamRequest(110)); err != nil {
		t.Fatal(err)
	}
	if !store.IsTrustedSender(trust.Fingerprint(second.Public().(ed25519.PublicKey))) ||
		store.IsTrustedSender(trust.Fingerprint(first.Public().(ed25519.PublicKey))) {
		t.Fatal("clearing trust did not require fresh owner approval")
	}
}

type failingTrust struct{}

func (failingTrust) TrustSender(string) error    { return errors.New("storage unavailable") }
func (failingTrust) IsTrustedSender(string) bool { return false }
func (failingTrust) ClearTrustedSenders() error  { return errors.New("storage unavailable") }

func TestUnavailableTrustFailsClosed(t *testing.T) {
	r, m, a := bindingReceiver(t)
	r.trust = failingTrust{}
	const remote = "192.168.1.20"
	step2 := beginVerification(t, r, remote)
	if _, err := r.PairVerify(remote, step2); err != nil {
		t.Fatalf("pair-verify attempted trust persistence: %v", err)
	}
	keys := streamKeys(t, r, remote, 1)
	combined := streamRequest(110, 96)
	for key, value := range keys {
		combined[key] = value
	}
	for _, req := range []map[string]any{keys, combined} {
		_, err := r.Setup(remote, req)
		wantProtocolError(t, err, 403, ErrUntrusted)
	}
	fingerprint := trust.Fingerprint(r.sessions[remote].pair.PeerPublicKey())
	if err := r.ApproveSender(fingerprint); err == nil || len(r.PendingSenders()) != 1 {
		t.Fatal("failed persistence approved a sender or removed its pending request")
	}
	if err := r.ClearTrustedSenders(); err == nil || m.drops != 0 || m.prepares != 0 || a.configures != 0 || r.sessions[remote].haveEKey {
		t.Fatal("failed trust reset changed receiver state")
	}
	r.trust = nil
	_, err := r.Setup(remote, streamRequest(110))
	var protocolErr *protocolError
	if !errors.As(err, &protocolErr) || protocolErr.HTTPStatus() != 503 {
		t.Fatalf("missing trust store = %v, want 503", err)
	}
	if _, err := New(protocol.DeviceInfo{}, r.keys, nil, nil, Events{}, false); err == nil {
		t.Fatal("receiver started without a trust policy")
	}
}

func TestUnknownSetupNeverApprovesSender(t *testing.T) {
	r, m, a := bindingReceiver(t)
	store, err := trust.Open(t.TempDir(), r.keys.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	r.trust = store
	const remote = "192.168.1.20"
	identity := senderIdentity(t)
	finishVerificationAs(t, r, remote, identity)
	fingerprint := trust.Fingerprint(identity.Public().(ed25519.PublicKey))
	for _, req := range []map[string]any{
		{},
		{"ekey": []byte{1}},
		{"ekey": make([]byte, 72), "eiv": make([]byte, 16)}, // No FairPlay handshake.
		streamRequest(110),
	} {
		_, err := r.Setup(remote, req)
		wantProtocolError(t, err, 403, ErrUntrusted)
		if store.IsTrustedSender(fingerprint) || m.prepares != 0 || a.configures != 0 || r.sessions[remote].haveEKey {
			t.Fatal("empty, malformed or keyless setup enrolled trust or changed media")
		}
	}
}

func TestAutoTrustEnrollsVerifiedSenderOnFirstSetup(t *testing.T) {
	r, m, a := bindingReceiver(t)
	dir := t.TempDir()
	store, err := trust.Open(dir, r.keys.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	r.trust = store
	r.requireApproval = false
	var logs bytes.Buffer
	r.log = log.New(&logs, "", 0)
	const remote = "192.168.1.20"
	identity := senderIdentity(t)
	fingerprint := trust.Fingerprint(identity.Public().(ed25519.PublicKey))
	finishVerificationAs(t, r, remote, identity)
	if store.IsTrustedSender(fingerprint) {
		t.Fatal("pair-verify alone enrolled an identity")
	}
	if _, err := r.Setup(remote, streamKeys(t, r, remote, 1)); err != nil {
		t.Fatalf("first setup of a verified sender: %v", err)
	}
	if !store.IsTrustedSender(fingerprint) || len(r.PendingSenders()) != 0 {
		t.Fatal("first use did not persist trust automatically")
	}
	if !strings.Contains(logs.String(), "auto-trusted sender "+fingerprint) {
		t.Fatal("automatic trust was not logged")
	}
	reloaded, err := trust.Open(dir, r.keys.PublicKey())
	if err != nil || !reloaded.IsTrustedSender(fingerprint) {
		t.Fatalf("auto-trusted sender was not persisted: %v", err)
	}
	if _, err := r.Setup(remote, streamRequest(110, 96)); err != nil {
		t.Fatal(err)
	}
	if m.prepares != 1 || a.configures != 1 {
		t.Fatal("auto-trusted setup did not configure media")
	}

	// A second device is trusted on its own first use as well.
	const other = "192.168.1.21"
	otherIdentity := senderIdentity(t)
	finishVerificationAs(t, r, other, otherIdentity)
	if _, err := r.Setup(other, streamKeys(t, r, other, 2)); err != nil {
		t.Fatal(err)
	}
	if !store.IsTrustedSender(trust.Fingerprint(otherIdentity.Public().(ed25519.PublicKey))) {
		t.Fatal("second verified sender was not auto-trusted")
	}
}

func TestAutoTrustPersistenceFailureFailsClosed(t *testing.T) {
	r, m, a := bindingReceiver(t)
	r.trust = failingTrust{}
	r.requireApproval = false
	const remote = "192.168.1.20"
	finishVerificationAs(t, r, remote, senderIdentity(t))
	_, err := r.Setup(remote, streamKeys(t, r, remote, 1))
	var protocolErr *protocolError
	if !errors.As(err, &protocolErr) || protocolErr.HTTPStatus() != 503 {
		t.Fatalf("auto-trust persistence failure = %v, want 503", err)
	}
	if m.prepares != 0 || a.configures != 0 || r.sessions[remote].haveEKey {
		t.Fatal("failed automatic trust changed media")
	}
}
