package pairing

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"errors"
	"testing"
)

func clientStep1(c *testClient) []byte {
	req := make([]byte, 0, VerifyStep1Len)
	req = append(req, 1, 0, 0, 0)
	req = append(req, c.xPriv.PublicKey().Bytes()...)
	req = append(req, c.edPub...)
	return req
}

// clientStep2 derives the client's shared secret from the receiver's step-1
// reply and returns the signed step-2 body. corrupt flips a signature byte.
func clientStep2(t *testing.T, c *testClient, reply []byte, corrupt bool) []byte {
	t.Helper()
	if len(reply) != X25519KeyLen+SignatureLen {
		t.Fatalf("step 1 reply length = %d", len(reply))
	}
	peer, err := ecdh.X25519().NewPublicKey(reply[:X25519KeyLen])
	if err != nil {
		t.Fatal(err)
	}
	c.secret, err = c.xPriv.ECDH(peer)
	if err != nil {
		t.Fatal(err)
	}
	stream := cipherStream(c.secret)
	// Discard the receiver's encrypted signature, occupying the same keystream.
	filler := make([]byte, SignatureLen)
	stream.XORKeyStream(filler, filler)
	message := append(append([]byte(nil), c.xPriv.PublicKey().Bytes()...), reply[:X25519KeyLen]...)
	signature := ed25519.Sign(c.edPriv, message)
	if corrupt {
		signature[0] ^= 0xff
	}
	out := make([]byte, 0, VerifyStep2Len)
	out = append(out, 0, 0, 0, 0)
	out = append(out, signature...)
	stream.XORKeyStream(out[VerifyHeaderLen:], out[VerifyHeaderLen:])
	return out
}

// The pairing session is a small state machine: setup, handshake, finished.
// Every out-of-order or malformed step must leave it where it was.
func TestPairingStateMachineOrdering(t *testing.T) {
	keys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession(keys)
	c := newTestClient(t)

	if !errors.Is(s.HandshakeStatus(), ErrMissingKey) || s.Secret() != nil || s.Verified() {
		t.Fatal("new session is not in the initial state")
	}
	step2 := make([]byte, VerifyStep2Len)
	if _, err := s.Verify(step2); !errors.Is(err, ErrMissingKey) {
		t.Fatalf("verify before setup = %v, want ErrMissingKey", err)
	}
	if _, err := s.Setup(make([]byte, SetupRequestLen-1)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("short setup = %v, want ErrMalformed", err)
	}
	if !errors.Is(s.HandshakeStatus(), ErrMissingKey) {
		t.Fatal("malformed setup advanced the session")
	}
	if _, err := s.Setup(make([]byte, SetupRequestLen)); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := s.HandshakeStatus(); err != nil || s.Secret() != nil || s.Verified() {
		t.Fatalf("after setup: status=%v secret=%v verified=%v", err, s.Secret(), s.Verified())
	}

	// Step 2 before step 1, and an unknown step byte, are refused.
	if _, err := s.Verify(step2); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("step 2 before step 1 = %v, want ErrOutOfOrder", err)
	}
	unknown := append([]byte{2, 0, 0, 0}, make([]byte, SignatureLen)...)
	if _, err := s.Verify(unknown); !errors.Is(err, ErrMalformed) {
		t.Fatalf("unknown step = %v, want ErrMalformed", err)
	}
	if _, err := s.Verify([]byte{1, 0, 0, 0, 1, 2, 3}); !errors.Is(err, ErrMalformed) {
		t.Fatalf("short step 1 = %v, want ErrMalformed", err)
	}
	if s.Verified() || s.Secret() != nil {
		t.Fatal("refused steps changed the session")
	}

	firstReply, err := s.Verify(clientStep1(c))
	if err != nil {
		t.Fatalf("step 1: %v", err)
	}
	firstSecret := s.Secret()
	if firstSecret == nil || s.Verified() || s.PeerPublicKey() != nil {
		t.Fatal("step 1 did not reach the handshake state")
	}

	// A repeated step 1 restarts the handshake; a signature built for the
	// first keys can no longer finish it.
	secondReply, err := s.Verify(clientStep1(c))
	if err != nil {
		t.Fatalf("repeated step 1: %v", err)
	}
	if bytes.Equal(firstSecret, s.Secret()) {
		t.Fatal("repeated step 1 kept the previous shared secret")
	}
	if _, err := s.Verify(clientStep2(t, c, firstReply, false)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("stale step 2 = %v, want ErrBadSignature", err)
	}
	if s.Verified() {
		t.Fatal("a stale signature verified the sender")
	}

	// The current handshake finishes with the right signature only.
	if _, err := s.Verify(clientStep2(t, c, secondReply, true)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("corrupted step 2 = %v, want ErrBadSignature", err)
	}
	if s.Verified() || s.PeerPublicKey() != nil {
		t.Fatal("bad signature verified the sender")
	}
	if _, err := s.Verify(clientStep2(t, c, secondReply, false)); err != nil {
		t.Fatalf("step 2: %v", err)
	}
	if !s.Verified() || !bytes.Equal(s.PeerPublicKey(), c.edPub) {
		t.Fatal("valid step 2 did not verify the sender")
	}
	if !bytes.Equal(s.Secret(), c.secret) {
		t.Fatal("receiver and sender disagree on the shared secret")
	}

	// Finished is terminal until a new setup starts another attempt; a
	// finished session no longer accepts pairing steps at all.
	if _, err := s.Verify(clientStep2(t, c, secondReply, false)); !errors.Is(err, ErrMissingKey) {
		t.Fatalf("step 2 after finish = %v, want ErrMissingKey", err)
	}
	if _, err := s.Setup(make([]byte, SetupRequestLen)); err != nil {
		t.Fatalf("second setup: %v", err)
	}
	if s.Verified() || s.PeerPublicKey() != nil {
		t.Fatal("a new setup kept the sender verified")
	}
	if _, err := s.Verify(step2); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("step 2 after a new setup = %v, want ErrOutOfOrder", err)
	}
}

// An X25519 key that cannot produce a shared secret leaves the session ready
// for a valid retry instead of half-advanced.
func TestPairingRejectsUnusableX25519Key(t *testing.T) {
	keys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession(keys)
	if _, err := s.Setup(make([]byte, SetupRequestLen)); err != nil {
		t.Fatal(err)
	}
	c := newTestClient(t)
	bad := make([]byte, 0, VerifyStep1Len)
	bad = append(bad, 1, 0, 0, 0)
	bad = append(bad, make([]byte, X25519KeyLen)...) // low-order point
	bad = append(bad, c.edPub...)
	if _, err := s.Verify(bad); err == nil {
		t.Fatal("an unusable X25519 key was accepted")
	}
	if s.Verified() || s.Secret() != nil {
		t.Fatal("a failed step 1 advanced the session")
	}
	if _, err := s.Verify(clientStep1(c)); err != nil {
		t.Fatalf("retry after a bad key: %v", err)
	}
}
