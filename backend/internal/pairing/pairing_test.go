package pairing

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"testing"
)

// testClient is the sender side of the exchange, written independently of the
// receiver so that a mistake in either half shows up as a failed round trip.
type testClient struct {
	edPub  ed25519.PublicKey
	edPriv ed25519.PrivateKey
	xPriv  *ecdh.PrivateKey
	secret []byte
}

func newTestClient(t *testing.T) *testClient {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	x, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &testClient{edPub: pub, edPriv: priv, xPriv: x}
}

func cipherStream(secret []byte) cipher.Stream {
	derive := func(salt string) []byte {
		sum := sha512.Sum512(append([]byte(salt), secret...))
		return sum[:16]
	}
	block, err := aes.NewCipher(derive(saltKey))
	if err != nil {
		panic(err)
	}
	return cipher.NewCTR(block, derive(saltIV))
}

func TestPairVerifyRoundTrip(t *testing.T) {
	keys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	session := NewSession(keys)
	client := newTestClient(t)

	if _, err := session.Verify(make([]byte, VerifyStep1Len)); err == ErrMissingKey {
		// Expected: pair-setup must come first.
	} else {
		t.Fatalf("verify before setup: got %v, want ErrMissingKey", err)
	}

	reply, err := session.Setup(make([]byte, SetupRequestLen))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !bytes.Equal(reply, keys.PublicKey()) {
		t.Fatal("setup reply is not the receiver public key")
	}

	// Step 1.
	req := make([]byte, 0, VerifyStep1Len)
	req = append(req, 1, 0, 0, 0)
	req = append(req, client.xPriv.PublicKey().Bytes()...)
	req = append(req, client.edPub...)

	resp, err := session.Verify(req)
	if err != nil {
		t.Fatalf("verify step 1: %v", err)
	}
	if len(resp) != X25519KeyLen+SignatureLen {
		t.Fatalf("step 1 reply length = %d, want %d", len(resp), X25519KeyLen+SignatureLen)
	}
	serverX := resp[:X25519KeyLen]
	encServerSig := resp[X25519KeyLen:]

	peer, err := ecdh.X25519().NewPublicKey(serverX)
	if err != nil {
		t.Fatal(err)
	}
	client.secret, err = client.xPriv.ECDH(peer)
	if err != nil {
		t.Fatal(err)
	}

	stream := cipherStream(client.secret)
	serverSig := make([]byte, SignatureLen)
	stream.XORKeyStream(serverSig, encServerSig)

	// The receiver signs its own key followed by the client's.
	msg := append(append([]byte(nil), serverX...), client.xPriv.PublicKey().Bytes()...)
	if !ed25519.Verify(keys.PublicKey(), msg, serverSig) {
		t.Fatal("receiver signature did not verify")
	}

	// Step 2: answer on the second half of the same keystream.
	clientMsg := append(append([]byte(nil), client.xPriv.PublicKey().Bytes()...), serverX...)
	raw := ed25519.Sign(client.edPriv, clientMsg)
	encClientSig := make([]byte, SignatureLen)
	stream.XORKeyStream(encClientSig, raw)

	req2 := append([]byte{0, 0, 0, 0}, encClientSig...)
	if _, err := session.Verify(req2); err != nil {
		t.Fatalf("verify step 2: %v", err)
	}
	if !bytes.Equal(session.Secret(), client.secret) {
		t.Fatal("shared secrets differ")
	}
}

func TestPairVerifyRejectsBadSignature(t *testing.T) {
	keys, _ := NewKeys()
	session := NewSession(keys)
	client := newTestClient(t)

	if _, err := session.Setup(make([]byte, SetupRequestLen)); err != nil {
		t.Fatal(err)
	}
	req := append([]byte{1, 0, 0, 0}, client.xPriv.PublicKey().Bytes()...)
	req = append(req, client.edPub...)
	if _, err := session.Verify(req); err != nil {
		t.Fatal(err)
	}

	bad := append([]byte{0, 0, 0, 0}, make([]byte, SignatureLen)...)
	if _, err := session.Verify(bad); err == nil {
		t.Fatal("expected a signature failure")
	}
}

func TestSetupRejectsWrongLength(t *testing.T) {
	keys, _ := NewKeys()
	session := NewSession(keys)
	if _, err := session.Setup(make([]byte, SetupRequestLen+1)); err != ErrMalformed {
		t.Fatalf("got %v, want ErrMalformed", err)
	}
}

func TestKeysPersistFromSeed(t *testing.T) {
	keys, _ := NewKeys()
	again, err := KeysFromSeed(keys.Seed())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(keys.PublicKey(), again.PublicKey()) {
		t.Fatal("restored identity differs")
	}
}
