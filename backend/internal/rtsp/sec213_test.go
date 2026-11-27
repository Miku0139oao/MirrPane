package rtsp_test

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"howett.net/plist"

	"github.com/Miku0139oao/MirrPane/backend/internal/fairplay"
	"github.com/Miku0139oao/MirrPane/backend/internal/pairing"
	"github.com/Miku0139oao/MirrPane/backend/internal/protocol"
	"github.com/Miku0139oao/MirrPane/backend/internal/receiver"
	"github.com/Miku0139oao/MirrPane/backend/internal/rtsp"
	"github.com/Miku0139oao/MirrPane/backend/internal/trust"
)

type pipeAddr string

func (a pipeAddr) Network() string { return "tcp" }
func (a pipeAddr) String() string  { return string(a) }

type addrPipeConn struct {
	net.Conn
	remote net.Addr
}

func (c *addrPipeConn) RemoteAddr() net.Addr { return c.remote }

type wireResponse struct {
	status int
	body   []byte
}

type controlClient struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
	cseq int
}

// pipeListener hands pre-created net.Conn pairs to rtsp.Server.Serve, exercising
// Server.Serve/serveConn end-to-end without binding a real TCP port.
type pipeListener struct {
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{
		ch:     make(chan net.Conn, 16),
		closed: make(chan struct{}),
	}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr("127.0.0.1:7000") }

func (l *pipeListener) dial(t *testing.T, remote string) *controlClient {
	t.Helper()
	serverSide, clientSide := net.Pipe()
	l.ch <- &addrPipeConn{Conn: serverSide, remote: pipeAddr(remote)}
	return &controlClient{t: t, conn: clientSide, r: bufio.NewReader(clientSide)}
}

func (c *controlClient) close() {
	_ = c.conn.Close()
}

func (c *controlClient) do(method, target, proto, contentType string, body []byte) wireResponse {
	c.t.Helper()
	c.cseq++
	_ = c.conn.SetDeadline(time.Now().Add(5 * time.Second))
	var req bytes.Buffer
	fmt.Fprintf(&req, "%s %s %s\r\n", method, target, proto)
	fmt.Fprintf(&req, "CSeq: %d\r\n", c.cseq)
	if contentType != "" {
		fmt.Fprintf(&req, "Content-Type: %s\r\n", contentType)
	}
	if len(body) > 0 {
		fmt.Fprintf(&req, "Content-Length: %d\r\n", len(body))
	}
	req.WriteString("\r\n")
	if len(body) > 0 {
		req.Write(body)
	}
	if _, err := c.conn.Write(req.Bytes()); err != nil {
		c.t.Fatalf("%s %s write: %v", method, target, err)
	}
	statusLine, err := c.r.ReadString('\n')
	if err != nil {
		c.t.Fatalf("%s %s status: %v", method, target, err)
	}
	parts := strings.SplitN(strings.TrimRight(statusLine, "\r\n"), " ", 3)
	if len(parts) < 2 {
		c.t.Fatalf("malformed status line %q", statusLine)
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil {
		c.t.Fatalf("status code %q: %v", parts[1], err)
	}
	cl := 0
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			c.t.Fatalf("%s %s header: %v", method, target, err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if name, val, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			cl, err = strconv.Atoi(strings.TrimSpace(val))
			if err != nil {
				c.t.Fatalf("Content-Length %q: %v", val, err)
			}
		}
	}
	var respBody []byte
	if cl > 0 {
		respBody = make([]byte, cl)
		if _, err := io.ReadFull(c.r, respBody); err != nil {
			c.t.Fatalf("%s %s body: %v", method, target, err)
		}
	}
	return wireResponse{status: status, body: respBody}
}

func (c *controlClient) doPlist(method, target string, payload map[string]any) wireResponse {
	c.t.Helper()
	b, err := plist.Marshal(payload, plist.BinaryFormat)
	if err != nil {
		c.t.Fatal(err)
	}
	return c.do(method, target, "RTSP/1.0", "application/x-apple-binary-plist", b)
}

func wirePairVerify(t *testing.T, c *controlClient, recvPub ed25519.PublicKey, senderPriv ed25519.PrivateKey) {
	t.Helper()
	resp := c.do("POST", "/pair-setup", "RTSP/1.0", "application/octet-stream", make([]byte, pairing.SetupRequestLen))
	if resp.status != 200 {
		t.Fatalf("/pair-setup status = %d, want 200", resp.status)
	}
	senderPub := senderPriv.Public().(ed25519.PublicKey)
	x, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	step1 := append([]byte{1, 0, 0, 0}, x.PublicKey().Bytes()...)
	step1 = append(step1, senderPub...)
	resp = c.do("POST", "/pair-verify", "RTSP/1.0", "application/octet-stream", step1)
	if resp.status != 200 || len(resp.body) != pairing.X25519KeyLen+pairing.SignatureLen {
		t.Fatalf("/pair-verify step 1 = %d len %d", resp.status, len(resp.body))
	}
	serverKey, err := ecdh.X25519().NewPublicKey(resp.body[:pairing.X25519KeyLen])
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
	sig := make([]byte, pairing.SignatureLen)
	stream.XORKeyStream(sig, resp.body[pairing.X25519KeyLen:])
	msg := append(append([]byte(nil), serverKey.Bytes()...), x.PublicKey().Bytes()...)
	if !ed25519.Verify(recvPub, msg, sig) {
		t.Fatal("receiver signature verification failed")
	}
	msg = append(append([]byte(nil), x.PublicKey().Bytes()...), serverKey.Bytes()...)
	stream.XORKeyStream(sig, ed25519.Sign(senderPriv, msg))
	resp = c.do("POST", "/pair-verify", "RTSP/1.0", "application/octet-stream", append([]byte{0, 0, 0, 0}, sig...))
	if resp.status != 200 {
		t.Fatalf("/pair-verify step 2 status = %d", resp.status)
	}
}

func wireFPSetup(t *testing.T, c *controlClient, seed byte) {
	t.Helper()
	step1 := make([]byte, fairplay.SetupRequestLen)
	copy(step1, "FPLY\x03")
	if resp := c.do("POST", "/fp-setup", "RTSP/1.0", "application/octet-stream", step1); resp.status != 200 {
		t.Fatalf("/fp-setup step 1 status = %d", resp.status)
	}
	step2 := bytes.Repeat([]byte{seed}, fairplay.HandshakeRequestLen)
	copy(step2, "FPLY\x03")
	step2[12] = 0
	if resp := c.do("POST", "/fp-setup", "RTSP/1.0", "application/octet-stream", step2); resp.status != 200 {
		t.Fatalf("/fp-setup step 2 status = %d", resp.status)
	}
}

func TestIPCServerIsolatesSameIPControlConnections(t *testing.T) {
	keys, err := pairing.NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	store, err := trust.Open(t.TempDir(), keys.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	info := protocol.NewDeviceInfo("鏡屏", "AA:BB:CC:DD:EE:FF")
	info.PublicKey = keys.PublicKeyHex()

	var stateMu sync.Mutex
	var states []receiver.Status
	disconnected := make(chan string, 16)

	recv, err := receiver.New(info, keys, store, log.New(io.Discard, "", 0), receiver.Events{
		OnState: func(s receiver.Status, _ string) {
			stateMu.Lock()
			states = append(states, s)
			stateMu.Unlock()
		},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer recv.Close()

	srv := rtsp.New(info, rtsp.Display{Width: 1920, Height: 1080})
	srv.OnFPSetup = recv.FPSetup
	srv.OnPairSetup = recv.PairSetup
	srv.OnPairVerify = recv.PairVerify
	srv.OnSetup = recv.Setup
	srv.OnTeardown = recv.Teardown
	srv.OnDisconnect = func(remote string) {
		recv.DropSession(remote)
		disconnected <- remote
	}
	srv.OnStop = recv.DropAllSessions

	ln := newPipeListener()
	if err := srv.Serve(ln); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	_, ownerPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ownerFP := trust.Fingerprint(ownerPriv.Public().(ed25519.PublicKey))

	// 1. Owner on conn 1 verifies, gets approved, completes fp-setup + SETUP.
	conn1 := ln.dial(t, "192.0.2.10:50001")
	defer conn1.close()
	wirePairVerify(t, conn1, keys.PublicKey(), ownerPriv)
	if resp := conn1.doPlist("SETUP", "/stream", map[string]any{
		"streams": []any{map[string]any{"type": uint64(110), "streamConnectionID": uint64(1)}},
	}); resp.status != 403 {
		t.Fatalf("unapproved SETUP = %d, want 403", resp.status)
	}
	if err := recv.ApproveSender(ownerFP); err != nil {
		t.Fatal(err)
	}
	wireFPSetup(t, conn1, 1)
	if resp := conn1.doPlist("SETUP", "/stream", map[string]any{
		"ekey": bytes.Repeat([]byte{1}, fairplay.EKeyLen),
		"eiv":  bytes.Repeat([]byte{1}, 16),
	}); resp.status != 200 {
		t.Fatalf("owner key SETUP = %d, want 200", resp.status)
	}
	if resp := conn1.doPlist("SETUP", "/stream", map[string]any{
		"streams": []any{
			map[string]any{"type": uint64(110), "streamConnectionID": uint64(42)},
			map[string]any{"type": uint64(96), "ct": uint64(8), "spf": uint64(480)},
		},
	}); resp.status != 200 {
		t.Fatalf("owner stream SETUP = %d, want 200", resp.status)
	}

	// 2. Same-IP attacker on conn 2 tries /fp-setup + SETUP without pair-verify -> 403.
	conn2 := ln.dial(t, "192.0.2.10:50002")
	wireFPSetup(t, conn2, 2)
	if resp := conn2.doPlist("SETUP", "/stream", map[string]any{
		"ekey": bytes.Repeat([]byte{2}, fairplay.EKeyLen),
		"eiv":  bytes.Repeat([]byte{2}, 16),
		"streams": []any{
			map[string]any{"type": uint64(110), "streamConnectionID": uint64(99)},
		},
	}); resp.status != 403 {
		t.Fatalf("same-IP attacker SETUP = %d, want 403", resp.status)
	}
	// Attacker TEARDOWN is ignored.
	if resp := conn2.do("TEARDOWN", "/stream", "RTSP/1.0", "", nil); resp.status != 200 {
		t.Fatalf("TEARDOWN status = %d", resp.status)
	}

	// 3. Auxiliary /info connection from same IP opens and closes -> Owner unaffected.
	aux := ln.dial(t, "192.0.2.10:59599")
	if resp := aux.do("GET", "/info", "RTSP/1.0", "", nil); resp.status != 200 {
		t.Fatalf("aux /info = %d", resp.status)
	}
	aux.close()
	if got := <-disconnected; got != "192.0.2.10:59599" {
		t.Fatalf("aux disconnect = %q, want 192.0.2.10:59599", got)
	}

	// 4. Closing attacker conn 2 does not disturb Owner on conn 1.
	conn2.close()
	if got := <-disconnected; got != "192.0.2.10:50002" {
		t.Fatalf("attacker disconnect = %q, want 192.0.2.10:50002", got)
	}
	stateMu.Lock()
	for _, s := range states {
		if s == receiver.StatusIdle {
			stateMu.Unlock()
			t.Fatal("auxiliary or attacker connection close emitted StatusIdle")
		}
	}
	stateMu.Unlock()

	// Owner can still perform audio-only TEARDOWN + SETUP on conn 1.
	if resp := conn1.doPlist("TEARDOWN", "/stream", map[string]any{
		"streams": []any{map[string]any{"type": uint64(96)}},
	}); resp.status != 200 {
		t.Fatalf("owner audio teardown = %d", resp.status)
	}
	if resp := conn1.doPlist("SETUP", "/stream", map[string]any{
		"streams": []any{map[string]any{"type": uint64(96), "ct": uint64(8), "spf": uint64(480)}},
	}); resp.status != 200 {
		t.Fatalf("owner audio re-setup = %d", resp.status)
	}

	// 5. Owner closes conn 1 -> DropSession fires for conn 1; unverified conn 3 is rejected,
	// while trusted reconnect on conn 4 with fresh pair-verify succeeds within grace.
	conn1.close()
	if got := <-disconnected; got != "192.0.2.10:50001" {
		t.Fatalf("owner disconnect = %q, want 192.0.2.10:50001", got)
	}

	conn3 := ln.dial(t, "192.0.2.10:50003")
	defer conn3.close()
	wireFPSetup(t, conn3, 3)
	if resp := conn3.doPlist("SETUP", "/stream", map[string]any{
		"ekey": bytes.Repeat([]byte{3}, fairplay.EKeyLen),
		"eiv":  bytes.Repeat([]byte{3}, 16),
	}); resp.status != 403 {
		t.Fatalf("unverified post-close SETUP = %d, want 403", resp.status)
	}

	conn4 := ln.dial(t, "192.0.2.10:50099")
	defer conn4.close()
	wirePairVerify(t, conn4, keys.PublicKey(), ownerPriv)
	wireFPSetup(t, conn4, 4)
	if resp := conn4.doPlist("SETUP", "/stream", map[string]any{
		"ekey": bytes.Repeat([]byte{4}, fairplay.EKeyLen),
		"eiv":  bytes.Repeat([]byte{4}, 16),
	}); resp.status != 200 {
		t.Fatalf("trusted reconnect key SETUP = %d, want 200", resp.status)
	}
	if resp := conn4.doPlist("SETUP", "/stream", map[string]any{
		"streams": []any{
			map[string]any{"type": uint64(110), "streamConnectionID": uint64(43)},
			map[string]any{"type": uint64(96), "ct": uint64(8), "spf": uint64(480)},
		},
	}); resp.status != 200 {
		t.Fatalf("trusted reconnect stream SETUP = %d, want 200", resp.status)
	}
}
