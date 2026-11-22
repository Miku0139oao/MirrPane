package receiver

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"errors"
	"io"
	"log"
	"sync"
	"testing"

	"howett.net/plist"

	"github.com/miku0139/airplay-go/backend/internal/audio"
	"github.com/miku0139/airplay-go/backend/internal/fairplay"
	"github.com/miku0139/airplay-go/backend/internal/mirror"
	"github.com/miku0139/airplay-go/backend/internal/pairing"
)

type testMirror struct {
	remote          string
	streamID        uint64
	keys            []mirror.KeyCandidate
	pending, client bool
	refuse          bool
	prepares        int
	drops, resets   int
}

func (m *testMirror) Port() int { return 7100 }
func (m *testMirror) Prepare(remote string, id uint64, keys ...mirror.KeyCandidate) bool {
	if m.refuse {
		return false
	}
	m.remote, m.streamID, m.keys = remote, id, keys
	m.pending = true
	m.prepares++
	return true
}
func (m *testMirror) Busy() bool              { return m.pending || m.client }
func (m *testMirror) DropClient()             { m.client = false; m.drops++ }
func (m *testMirror) ResetKey()               { m.pending = false; m.keys = nil; m.resets++ }
func (m *testMirror) Stats() (uint64, uint64) { return 0, 0 }
func (m *testMirror) Close() error            { return nil }

type testAudio struct {
	keys                       []audio.Key
	iv                         []byte
	busy                       bool
	configures, resets, clears int
}

// Media-ownership tests use multiple explicitly authorized identities. The
// persisted owner-approval policy is exercised separately in trust_test.go.
type allowTestSenders struct{}

func (allowTestSenders) TrustSender(string) error    { return nil }
func (allowTestSenders) IsTrustedSender(string) bool { return true }
func (allowTestSenders) ClearTrustedSenders() error  { return nil }

func (a *testAudio) Ports() (int, int) { return 7200, 7201 }
func (a *testAudio) Configure(keys []audio.Key, iv []byte, _ ...[]byte) {
	a.keys = make([]audio.Key, len(keys))
	for i, key := range keys {
		a.keys[i] = audio.Key{Name: key.Name, Key: append([]byte(nil), key.Key...)}
	}
	a.iv = append([]byte(nil), iv...)
	a.configures++
	a.busy = true
}
func (a *testAudio) Busy() bool                      { return a.busy }
func (a *testAudio) ResetStream()                    { a.resets++; a.busy = false }
func (a *testAudio) ResetKey()                       { a.keys, a.iv = nil, nil; a.busy = false; a.clears++ }
func (a *testAudio) Stats() (uint64, uint64, uint64) { return 0, 0, 0 }
func (a *testAudio) Close()                          {}

func bindingReceiver(t *testing.T) (*Receiver, *testMirror, *testAudio) {
	t.Helper()
	keys, err := pairing.NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	m, a := &testMirror{}, &testAudio{}
	r := &Receiver{keys: keys, trust: allowTestSenders{}, log: log.New(io.Discard, "", 0),
		mirror: m, audioRx: a, timing: &udpStub{port: 7300}, sessions: make(map[string]*clientSession)}
	return r, m, a
}

// beginVerification performs the real sender-side cryptography. The caller
// chooses when to send step 2, so the test can exercise incomplete pairing.
func beginVerification(t *testing.T, r *Receiver, remote string) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return beginVerificationAs(t, r, remote, priv)
}

func beginVerificationAs(t *testing.T, r *Receiver, remote string, priv ed25519.PrivateKey) []byte {
	t.Helper()
	if _, err := r.PairSetup(remote, make([]byte, pairing.SetupRequestLen)); err != nil {
		t.Fatal(err)
	}
	pub := priv.Public().(ed25519.PublicKey)
	x, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	req := append([]byte{1, 0, 0, 0}, x.PublicKey().Bytes()...)
	req = append(req, pub...)
	reply, err := r.PairVerify(remote, req)
	if err != nil || len(reply) != pairing.X25519KeyLen+pairing.SignatureLen {
		t.Fatalf("pair-verify step 1: length %d, error %v", len(reply), err)
	}
	serverKey, err := ecdh.X25519().NewPublicKey(reply[:pairing.X25519KeyLen])
	if err != nil {
		t.Fatal(err)
	}
	secret, err := x.ECDH(serverKey)
	if err != nil {
		t.Fatal(err)
	}
	key := sha512.Sum512(append([]byte("Pair-Verify-AES-Key"), secret...))
	iv := sha512.Sum512(append([]byte("Pair-Verify-AES-IV"), secret...))
	block, err := aes.NewCipher(key[:16])
	if err != nil {
		t.Fatal(err)
	}
	stream := cipher.NewCTR(block, iv[:16])
	signature := make([]byte, pairing.SignatureLen)
	stream.XORKeyStream(signature, reply[pairing.X25519KeyLen:])
	message := append(append([]byte(nil), serverKey.Bytes()...), x.PublicKey().Bytes()...)
	if !ed25519.Verify(r.keys.PublicKey(), message, signature) {
		t.Fatal("receiver's pair-verify signature failed")
	}
	message = append(append([]byte(nil), x.PublicKey().Bytes()...), serverKey.Bytes()...)
	stream.XORKeyStream(signature, ed25519.Sign(priv, message))
	return append([]byte{0, 0, 0, 0}, signature...)
}

func verifySender(t *testing.T, r *Receiver, remote string) {
	t.Helper()
	step2 := beginVerification(t, r, remote)
	if _, err := r.PairVerify(remote, step2); err != nil || !r.session(remote).pair.Verified() {
		t.Fatalf("pair-verify did not finish: %v", err)
	}
}

func streamKeys(t *testing.T, r *Receiver, remote string, seed byte) map[string]any {
	t.Helper()
	message := bytes.Repeat([]byte{seed}, fairplay.HandshakeRequestLen)
	copy(message, "FPLY\x03")
	message[12] = 0
	if _, err := r.FPSetup(remote, message); err != nil {
		t.Fatal(err)
	}
	return map[string]any{"ekey": bytes.Repeat([]byte{seed}, fairplay.EKeyLen), "eiv": bytes.Repeat([]byte{seed}, 16)}
}

func streamRequest(types ...uint64) map[string]any {
	streams := make([]any, len(types))
	for i, typ := range types {
		streams[i] = map[string]any{"type": typ, "streamConnectionID": uint64(42), "ct": uint64(8)}
	}
	return map[string]any{"streams": streams}
}

func wantProtocolError(t *testing.T, err error, status int, cause error) {
	t.Helper()
	var protocol *protocolError
	if !errors.As(err, &protocol) || protocol.HTTPStatus() != status || !errors.Is(err, cause) {
		t.Fatalf("error = %v, want protocol status %d with %v", err, status, cause)
	}
}

func startSender(t *testing.T, r *Receiver, remote string, seed byte) {
	t.Helper()
	verifySender(t, r, remote)
	if _, err := r.Setup(remote, streamKeys(t, r, remote, seed)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(remote, streamRequest(110, 96)); err != nil {
		t.Fatal(err)
	}
}

func TestSetupRequiresFinishedPairVerify(t *testing.T) {
	r, m, a := bindingReceiver(t)
	const remote = "192.168.1.20"
	keys := streamKeys(t, r, remote, 1)
	checkRejected := func() {
		t.Helper()
		for _, req := range []map[string]any{keys, {"ekey": []byte{1}}, {"eiv": make([]byte, 16)}, streamRequest(110), streamRequest(96)} {
			_, err := r.Setup(remote, req)
			wantProtocolError(t, err, 403, ErrNotPaired)
		}
		if m.prepares != 0 || a.configures != 0 || r.session(remote).haveEKey {
			t.Fatal("unpaired setup changed shared or per-sender stream keys")
		}
	}
	checkRejected()
	step2 := beginVerification(t, r, remote)
	// Even a known ECDH secret and the compatibility hook do not grant access.
	r.PairVerifySecret(remote, r.session(remote).pair.Secret())
	checkRejected()
	if _, err := r.PairVerify(remote, make([]byte, pairing.VerifyStep2Len)); err != nil {
		t.Fatal(err) // Invalid signatures keep the existing empty-200 response.
	}
	checkRejected()
	if _, err := r.PairVerify(remote, step2); err != nil {
		t.Fatal(err)
	}
	if !r.session(remote).pair.Verified() {
		t.Fatal("valid pair-verify did not reach finished")
	}
	if _, err := r.Setup(remote, keys); err != nil {
		t.Fatal(err)
	}
	if a.configures != 0 || m.prepares != 0 {
		t.Fatal("key-only setup enabled shared media before stream acceptance")
	}
	response, err := r.Setup(remote, streamRequest(110, 96))
	if err != nil {
		t.Fatal(err)
	}
	streams := response["streams"].([]any)
	if len(streams) != 2 || streams[0].(map[string]any)["dataPort"] != uint64(7100) ||
		streams[1].(map[string]any)["controlPort"] != uint64(7201) {
		t.Fatalf("verified setup changed the sender-visible response: %v", response)
	}
	if m.remote != remote || m.streamID != 42 || m.prepares != 1 || a.configures != 1 || len(a.keys) != 2 {
		t.Fatal("verified setup failed to bind mirror and audio to the sender")
	}
}

func TestOtherSenderCannotReconfigureActiveAudio(t *testing.T) {
	r, m, a := bindingReceiver(t)
	const owner, other = "192.168.1.20", "192.168.1.21"
	startSender(t, r, owner, 1)
	m.client = true
	oldKey, oldIV := append([]byte(nil), a.keys[0].Key...), append([]byte(nil), a.iv...)
	otherKeys := streamKeys(t, r, other, 2)
	_, err := r.Setup(other, otherKeys)
	wantProtocolError(t, err, 403, ErrNotPaired)
	verifySender(t, r, other)
	if _, err := r.Setup(other, otherKeys); err != nil {
		t.Fatal(err)
	}
	for _, types := range [][]uint64{{110}, {96}, {110, 96}} {
		req := streamRequest(types...)
		for k, v := range otherKeys {
			req[k] = v
		}
		_, err := r.Setup(other, req)
		wantProtocolError(t, err, 453, ErrBusy)
	}
	// Auxiliary control connections/teardowns from another IP release only
	// their own session, never the active sender's audio or mirror keys.
	r.Teardown(other, nil)
	r.DropSession(other)
	if a.configures != 1 || a.resets != 0 || a.clears != 0 ||
		!bytes.Equal(a.keys[0].Key, oldKey) || !bytes.Equal(a.iv, oldIV) ||
		m.prepares != 1 || m.drops != 0 || m.remote != owner || r.activeRemote != owner {
		t.Fatal("another sender changed active audio keys/buffers or mirror ownership")
	}
}

func TestAudioOnlySenderRetainsOwnership(t *testing.T) {
	r, m, a := bindingReceiver(t)
	const owner, other = "192.168.1.20", "192.168.1.21"
	startSender(t, r, owner, 1)
	m.client, m.pending = false, false
	verifySender(t, r, other)
	if _, err := r.Setup(other, streamKeys(t, r, other, 2)); err != nil {
		t.Fatal(err)
	}
	_, err := r.Setup(other, streamRequest(96))
	wantProtocolError(t, err, 453, ErrBusy)
	if a.configures != 1 || a.resets != 0 {
		t.Fatal("another sender reconfigured a live audio-only stream")
	}
}

func TestSameSenderReconnectAndStaleTakeover(t *testing.T) {
	r, m, a := bindingReceiver(t)
	const owner, other = "192.168.1.20", "192.168.1.21"
	startSender(t, r, owner, 1)
	m.client = true
	if _, err := r.Setup(owner, streamRequest(110)); err != nil {
		t.Fatalf("same-sender reconnect refused: %v", err)
	}
	if m.prepares != 2 || a.configures != 2 {
		t.Fatal("same-sender reconnect did not prepare its stream")
	}
	verifySender(t, r, other)
	if _, err := r.Setup(other, streamKeys(t, r, other, 2)); err != nil {
		t.Fatal(err)
	}
	m.pending, m.client, a.busy = false, false, false
	if _, err := r.Setup(other, streamRequest(110, 96)); err != nil {
		t.Fatalf("stale owner locked out a new verified sender: %v", err)
	}
	r.DropSession(owner)
	if r.activeRemote != other || m.remote != other || m.drops != 0 || a.clears != 0 {
		t.Fatal("old control connection cleanup dropped the replacement sender")
	}
}

func TestRefusedMirrorPrepareDoesNotChangeAudio(t *testing.T) {
	r, m, a := bindingReceiver(t)
	const remote = "192.168.1.20"
	verifySender(t, r, remote)
	req := streamKeys(t, r, remote, 1)
	req["streams"] = streamRequest(110, 96)["streams"]
	m.refuse = true
	_, err := r.Setup(remote, req)
	wantProtocolError(t, err, 453, ErrBusy)
	if a.configures != 0 || a.resets != 0 || r.activeRemote != "" || r.session(remote).haveEKey {
		t.Fatal("refused setup partially committed keys or audio state")
	}
}

func TestVerifiedSetupRejectsMalformedOrMissingKeys(t *testing.T) {
	r, m, a := bindingReceiver(t)
	const remote = "192.168.1.20"
	verifySender(t, r, remote)
	_, err := r.Setup(remote, map[string]any{"ekey": []byte{1}})
	var protocol *protocolError
	if !errors.As(err, &protocol) || protocol.HTTPStatus() != 400 {
		t.Fatalf("malformed verified setup = %v, want 400", err)
	}
	for _, typ := range []uint64{110, 96} {
		_, err = r.Setup(remote, streamRequest(typ))
		wantProtocolError(t, err, 400, ErrNoKey)
	}
	if m.prepares != 0 || a.configures != 0 {
		t.Fatal("malformed setup enabled shared media")
	}
}

func TestFullCleanupDropsClientAndPendingKeys(t *testing.T) {
	for _, action := range []string{"teardown", "disconnect", "control close", "stop"} {
		t.Run(action, func(t *testing.T) {
			r, m, a := bindingReceiver(t)
			const remote = "192.168.1.20"
			startSender(t, r, remote, 1)
			m.client = true
			switch action {
			case "teardown":
				r.Teardown(remote, nil)
			case "disconnect":
				r.Disconnect()
			case "control close":
				clock := installGraceClock(r)
				r.DropSession(remote) // Wired to ipc.Server.OnDisconnect.
				clock.fireAll()
			case "stop":
				r.DropAllSessions()
			}
			if m.client || m.pending || len(m.keys) != 0 || m.drops != 1 || m.resets != 1 ||
				a.clears != 1 || len(a.keys) != 0 || r.activeRemote != "" || r.sessions[remote] != nil {
				t.Fatal("full cleanup retained the client, pending keys, audio keys or pairing session")
			}
		})
	}
}

func TestAudioTeardownKeepsMirrorBindingAndPairing(t *testing.T) {
	r, m, a := bindingReceiver(t)
	const remote = "192.168.1.20"
	startSender(t, r, remote, 1)
	m.client = true
	body, err := plist.Marshal(streamRequest(96), plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	r.Teardown(remote, body)
	if !m.client || !m.pending || m.drops != 0 || m.resets != 0 || a.resets != 1 || a.clears != 0 ||
		r.sessions[remote] == nil || !r.sessions[remote].pair.Verified() || !r.sessions[remote].haveEKey {
		t.Fatal("audio-only teardown dropped pairing, key material or mirror binding")
	}
	clock := installGraceClock(r)
	r.DropSession(remote)
	clock.fireAll()
	if m.client || m.pending || a.clears != 1 {
		t.Fatal("control close after audio teardown retained media state")
	}
}

func TestSetupAndControlCleanupAreSerialized(t *testing.T) {
	r, _, _ := bindingReceiver(t)
	const remote = "192.168.1.20"
	startSender(t, r, remote, 1)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 10 {
				_, _ = r.Setup(remote, streamRequest(110, 96))
				r.PairVerifySecret(remote, make([]byte, 32))
				r.DropSession(remote)
				r.Disconnect()
			}
		})
	}
	wg.Wait()
	if r.activeRemote != "" || len(r.sessions) != 0 {
		t.Fatal("concurrent setup/cleanup retained a sender")
	}
}
