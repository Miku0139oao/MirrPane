package mirror

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"testing"
	"time"
)

type memoryAddr string

func (a memoryAddr) Network() string { return "tcp" }
func (a memoryAddr) String() string  { return string(a) }

// These connections exercise admission and serve without opening sockets.
type memoryStreamConn struct {
	remote   string
	input    *bytes.Reader
	closed   bool
	deadline time.Time
}

func (c *memoryStreamConn) Read(p []byte) (int, error) {
	if c.closed || c.input == nil {
		return 0, io.EOF
	}
	return c.input.Read(p)
}
func (c *memoryStreamConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *memoryStreamConn) Close() error                { c.closed = true; return nil }
func (c *memoryStreamConn) LocalAddr() net.Addr         { return memoryAddr("192.168.1.10:7100") }
func (c *memoryStreamConn) RemoteAddr() net.Addr        { return memoryAddr(c.remote) }
func (c *memoryStreamConn) SetDeadline(t time.Time) error {
	c.deadline = t
	return nil
}
func (c *memoryStreamConn) SetReadDeadline(t time.Time) error {
	c.deadline = t
	return nil
}
func (c *memoryStreamConn) SetWriteDeadline(time.Time) error { return nil }

func bindingServer() *Server {
	return NewServer(log.New(io.Discard, "", 0), Handlers{})
}

func prepareBinding(t *testing.T, s *Server, remote string, id uint64) {
	t.Helper()
	if !s.Prepare(remote, id, KeyCandidate{Name: "test", Key: []byte("0123456789abcdef")}) {
		t.Fatal("authenticated setup refused")
	}
}

func encryptedBindingFrame(t *testing.T, pending *pendingStream) []byte {
	t.Helper()
	key := pending.candidates[0]
	block, err := aes.NewCipher(key.Key)
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte{0, 0, 0, 2, 0x65, 1}
	enc := make([]byte, len(plain))
	cipher.NewCTR(block, key.IV).XORKeyStream(enc, plain)
	return enc
}

func proveBinding(t *testing.T, s *Server, conn *memoryStreamConn) {
	t.Helper()
	pending := s.attempts[conn].pending
	if pending == nil {
		pending = s.admit(conn)
	}
	if pending == nil {
		t.Fatal("stream admission refused")
	}
	key := pending.candidates[0]
	block, err := aes.NewCipher(key.Key)
	if err != nil {
		t.Fatal(err)
	}
	st := &streamState{srv: s, conn: conn, pending: pending, active: -1,
		cands: []cipher.Stream{cipher.NewCTR(block, key.IV)}, names: []string{"test"}}
	st.handleVideo(encryptedBindingFrame(t, pending), 0, 0)
	if s.conn != conn {
		t.Fatal("correct setup key did not establish stream ownership")
	}
}

func TestStreamRequiresFreshSetup(t *testing.T) {
	for _, name := range []string{"no setup", "expired setup"} {
		t.Run(name, func(t *testing.T) {
			s := bindingServer()
			if name != "no setup" {
				prepareBinding(t, s, "192.168.1.20", 42)
			}
			conn := &memoryStreamConn{remote: "192.168.1.20:5000"}
			if name == "expired setup" {
				s.pending.expires = time.Now().Add(-time.Second)
			}
			s.handlers.OnConnected = func(string) { t.Error("rejected peer was reported connected") }
			s.handlers.OnDisconnected = func() { t.Error("rejected peer disconnected the active sender") }
			s.serve(conn)
			if !conn.closed || s.conn != nil {
				t.Fatal("unbound connection was accepted")
			}
		})
	}
}

func TestRejectedPeerDoesNotPreemptHealthyStream(t *testing.T) {
	s := bindingServer()
	prepareBinding(t, s, "192.168.1.20", 42)
	old := &memoryStreamConn{remote: "192.168.1.20:5000"}
	proveBinding(t, s, old)
	prepareBinding(t, s, "192.168.1.20", 43)
	pending := s.pending
	if s.Prepare("192.168.1.21", 99, KeyCandidate{Key: make([]byte, 16)}) {
		t.Fatal("different sender overwrote live setup")
	}
	bad := &memoryStreamConn{remote: "192.168.1.21:5001"}
	s.serve(bad)
	if old.closed || s.conn != old || s.pending != pending || !bad.closed {
		t.Fatal("rejected setup/connection changed the healthy stream")
	}
}

func TestHalfOpenConnectionAndSameSenderReconnect(t *testing.T) {
	s := bindingServer()
	prepareBinding(t, s, "192.168.1.20", 42)
	old := &memoryStreamConn{remote: "192.168.1.20:5000"}
	if s.admit(old) == nil || old.deadline.IsZero() {
		t.Fatal("half-open connection has no initial read deadline")
	}
	// No new setup is needed while the initial, unused authorization is fresh.
	real := &memoryStreamConn{remote: "192.168.1.20:5001"}
	if s.admit(real) == nil || !old.closed || s.conn != nil || s.sender.IsValid() || s.streamID != 0 {
		t.Fatal("a half-open socket locked out the legitimate sender")
	}
	proveBinding(t, s, real)
	unprepared := &memoryStreamConn{remote: "192.168.1.20:5002"}
	if s.admit(unprepared) != nil || real.closed {
		t.Fatal("connection without a fresh setup preempted a live stream")
	}
	prepareBinding(t, s, "192.168.1.20", 43)
	if real.closed {
		t.Fatal("preparing a same-sender reconnect closed the healthy stream early")
	}
	reconnect := &memoryStreamConn{remote: "192.168.1.20:5003"}
	if s.admit(reconnect) == nil || real.closed || s.streamID != 42 {
		t.Fatal("unproven reconnect preempted the healthy stream")
	}
	proveBinding(t, s, reconnect)
	if !real.closed || s.streamID != 43 {
		t.Fatal("legitimate reconnect did not use the new stream ID")
	}
}

func TestStaleStreamCanBeReplaced(t *testing.T) {
	for _, validFrame := range []bool{false, true} {
		s := bindingServer()
		prepareBinding(t, s, "192.168.1.20", 42)
		old := &memoryStreamConn{remote: "192.168.1.20:5000"}
		if validFrame {
			proveBinding(t, s, old)
		} else {
			s.admit(old)
		}
		s.connectedAt = time.Now().Add(-pendingLifetime - time.Second)
		if s.pending != nil {
			s.pending.expires = s.connectedAt
		}
		if validFrame {
			s.lastFrame = s.connectedAt
		}
		if s.Busy() {
			t.Fatal("stale/half-open stream retained ownership indefinitely")
		}
		prepareBinding(t, s, "192.168.1.21", 99)
		next := &memoryStreamConn{remote: "192.168.1.21:5001"}
		proveBinding(t, s, next)
		if !old.closed || s.conn != next {
			t.Fatal("fresh authenticated setup could not replace the stale stream")
		}
	}
}

func TestOnlyDecryptedFramesRefreshLiveness(t *testing.T) {
	s := bindingServer()
	prepareBinding(t, s, "192.168.1.20", 42)
	conn := &memoryStreamConn{remote: "192.168.1.20:5000"}
	pending := s.admit(conn)
	candidate := pending.candidates[0]
	wantKey, wantIV := DeriveStreamKey(42, []byte("0123456789abcdef"))
	if !bytes.Equal(candidate.Key, wantKey[:]) || !bytes.Equal(candidate.IV, wantIV[:]) {
		t.Fatal("pending stream ID was not used for key derivation")
	}
	block, err := aes.NewCipher(candidate.Key)
	if err != nil {
		t.Fatal(err)
	}
	sender := cipher.NewCTR(block, candidate.IV)
	st := &streamState{srv: s, conn: conn, pending: pending, active: -1,
		cands: []cipher.Stream{cipher.NewCTR(block, candidate.IV)}, names: []string{"test"}}
	for _, plain := range [][]byte{nil, {0, 0, 0, 8, 0x65}} {
		enc := make([]byte, len(plain))
		sender.XORKeyStream(enc, plain)
		st.handleVideo(enc, 0, 0)
		if !s.lastFrame.IsZero() || s.pending == nil || conn.deadline.IsZero() {
			t.Fatal("invalid/empty frame refreshed liveness or consumed setup")
		}
	}
	plain := binary.BigEndian.AppendUint32(nil, 2)
	plain = append(plain, 0x65, 1)
	enc := make([]byte, len(plain))
	sender.XORKeyStream(enc, plain)
	st.handleVideo(enc, 0, 0)
	if s.lastFrame.IsZero() || s.pending != nil || !conn.deadline.IsZero() {
		t.Fatal("valid decrypted frame did not establish the stream")
	}
	// The consumed setup cannot be replayed, even from the same source IP.
	bad := &memoryStreamConn{remote: "192.168.1.20:5001"}
	s.serve(bad)
	if conn.closed || s.conn != conn {
		t.Fatal("consumed setup allowed preemption")
	}
}

func TestBindingNormalizesIPv4AndPreservesIPv6Zone(t *testing.T) {
	for _, tc := range []struct{ setup, data string }{
		{"::ffff:192.168.1.20", "192.168.1.20:5000"},
		{"192.168.1.20", "[::ffff:192.168.1.20]:5000"},
		{"2001:db8::20", "[2001:db8::20]:5000"},
		{"fe80::20%7", "[fe80::20%7]:5000"},
	} {
		s := bindingServer()
		prepareBinding(t, s, tc.setup, 42)
		if s.admit(&memoryStreamConn{remote: tc.data}) == nil {
			t.Fatalf("setup %s rejected data source %s", tc.setup, tc.data)
		}
	}
}

func TestDropAndDisconnectExpirePendingSetup(t *testing.T) {
	for _, action := range []string{"reset", "drop", "disconnect", "close"} {
		t.Run(action, func(t *testing.T) {
			s := bindingServer()
			prepareBinding(t, s, "192.168.1.20", 42)
			conn := &memoryStreamConn{remote: "192.168.1.20:5000"}
			switch action {
			case "reset":
				s.ResetKey()
			case "drop":
				s.admit(conn)
				s.DropClient()
				if !conn.closed || s.conn != nil {
					t.Fatal("drop retained the mirror client")
				}
			case "disconnect":
				header := make([]byte, HeaderLen)
				enc := encryptedBindingFrame(t, s.pending)
				binary.LittleEndian.PutUint32(header, uint32(len(enc)))
				conn.input = bytes.NewReader(append(header, enc...))
				s.serve(conn) // A proved stream followed by EOF.
			case "close":
				s.admit(conn)
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if s.pending != nil || s.Busy() {
				t.Fatal("cleanup retained pending key state or ownership")
			}
			next := &memoryStreamConn{remote: "192.168.1.20:5001"}
			s.serve(next)
			if !next.closed || s.conn != nil {
				t.Fatal("cleanup left the old setup usable")
			}
		})
	}
}

func TestSupersededConnectionCannotClearReconnect(t *testing.T) {
	s := bindingServer()
	prepareBinding(t, s, "192.168.1.20", 42)
	old := &memoryStreamConn{remote: "192.168.1.20:5000"}
	next := &memoryStreamConn{remote: "192.168.1.20:5001"}
	disconnects := 0
	s.handlers.OnDisconnected = func() { disconnects++ }
	s.handlers.OnConnected = func(remote string) {
		if remote != old.remote {
			return
		}
		prepareBinding(t, s, "192.168.1.20", 43)
		proveBinding(t, s, next)
		prepareBinding(t, s, "192.168.1.20", 44)
	}
	header := make([]byte, HeaderLen)
	enc := encryptedBindingFrame(t, s.pending)
	binary.LittleEndian.PutUint32(header, uint32(len(enc)))
	old.input = bytes.NewReader(append(header, enc...))
	s.serve(old)
	if disconnects != 0 || s.conn != next || s.streamID != 43 || s.pending == nil || s.pending.streamID != 44 {
		t.Fatal("superseded connection's cleanup erased the reconnect")
	}
}

func TestDifferentDataAddressRequiresFrameBeforeOwnership(t *testing.T) {
	for _, tc := range []struct{ control, data string }{
		{"192.168.1.20", "[2001:db8::20]:5000"},
		{"2001:db8::20", "192.168.1.20:5000"},
		{"2001:db8::20", "[2001:db8::21]:5000"},
	} {
		t.Run(tc.data, func(t *testing.T) {
			s := bindingServer()
			prepareBinding(t, s, tc.control, 42)
			conn := &memoryStreamConn{remote: tc.data}
			if s.admit(conn) == nil || s.conn != nil || s.sender.IsValid() || s.streamID != 0 || conn.deadline.IsZero() {
				t.Fatal("different-address admission either failed or claimed ownership without a frame")
			}
			proveBinding(t, s, conn)
			ip, _ := sourceIP(conn)
			if s.sender != ip || s.streamID != 42 || !conn.deadline.IsZero() {
				t.Fatal("valid frame did not commit the data address and setup stream ID")
			}
			// SETUP still uses the original control address after dual-stack data.
			prepareBinding(t, s, tc.control, 43)
			next := &memoryStreamConn{remote: tc.data}
			if s.admit(next) == nil || conn.closed {
				t.Fatal("same-address reconnect lost its fast path")
			}
			proveBinding(t, s, next)
		})
	}
}

func TestForeignKeylessPeerCannotConsumeSetupOrPublishState(t *testing.T) {
	s := bindingServer()
	prepareBinding(t, s, "192.168.1.20", 42)
	foreign := &memoryStreamConn{remote: "[2001:db8::99]:5000"}
	pending := s.admit(foreign)
	if pending == nil {
		t.Fatal("fresh setup did not permit an alternate data address")
	}
	connected, geometry, frames := 0, 0, 0
	s.handlers.OnConnected = func(string) { connected++ }
	s.handlers.OnGeometry = func(Size) { geometry++ }
	s.handlers.OnFrame = func(*Frame) { frames++ }
	st := &streamState{srv: s, conn: foreign, pending: pending}
	st.handleVideo([]byte{0, 0, 0, 2, 0x65, 1}, 0, 0) // No decryption key.
	payload := make([]byte, 15)
	binary.BigEndian.PutUint16(payload[6:8], 2)
	copy(payload[8:10], []byte{0x67, 1})
	binary.BigEndian.PutUint16(payload[11:13], 2)
	copy(payload[13:], []byte{0x68, 1})
	st.handleParams(make([]byte, HeaderLen), payload, 0)
	if s.conn != nil || s.sender.IsValid() || s.streamID != 0 || s.pending != pending || connected+geometry+frames != 0 {
		t.Fatal("keyless peer consumed authorization or published untrusted stream state")
	}
	legitimate := &memoryStreamConn{remote: "192.168.1.20:5001"}
	proveBinding(t, s, legitimate)
	if !foreign.closed || connected != 1 || geometry != 0 || frames != 1 {
		t.Fatal("foreign half-open connection locked out the real sender")
	}
}

func TestForeignEOFCannotInvalidatePendingSetup(t *testing.T) {
	s := bindingServer()
	prepareBinding(t, s, "192.168.1.20", 42)
	pending := s.pending
	s.serve(&memoryStreamConn{remote: "[2001:db8::99]:5000"})
	if s.pending != pending {
		t.Fatal("unproven peer's EOF revoked the legitimate setup")
	}
	proveBinding(t, s, &memoryStreamConn{remote: "192.168.1.20:5001"})
}

func TestSpeculativePeerLimitReservesExpectedSourceAdmission(t *testing.T) {
	s := bindingServer()
	prepareBinding(t, s, "192.168.1.20", 42)
	for i := range maxPendingPeers {
		peer := &memoryStreamConn{remote: fmt.Sprintf("[2001:db8::%x]:5000", i+1)}
		if s.admit(peer) == nil {
			t.Fatal("speculative connection rejected below the bound")
		}
	}
	if s.admit(&memoryStreamConn{remote: "[2001:db8::ff]:5000"}) == nil || len(s.attempts) != maxPendingPeers {
		t.Fatal("full speculative peer map did not rotate at its bound")
	}
	legitimate := &memoryStreamConn{remote: "192.168.1.20:5001"}
	proveBinding(t, s, legitimate)
	if s.conn != legitimate || len(s.attempts) != 0 {
		t.Fatal("foreign speculative sockets locked out the expected sender")
	}
}

func TestFullAttemptMapAdmitsDifferentFamilyDataPeer(t *testing.T) {
	s := bindingServer()
	prepareBinding(t, s, "2001:db8::20", 42)
	var oldestForeign *memoryStreamConn
	setupPeer := &memoryStreamConn{remote: "[2001:db8::20]:5000"}
	if s.admit(setupPeer) == nil {
		t.Fatal("setup source was not admitted")
	}
	setupAttempt := s.attempts[setupPeer]
	setupAttempt.at = time.Now().Add(-time.Minute) // Protected even when older.
	s.attempts[setupPeer] = setupAttempt
	for i := range maxPendingPeers - 1 {
		peer := &memoryStreamConn{remote: fmt.Sprintf("[2001:db8:1::%x]:5000", i+1)}
		if s.admit(peer) == nil {
			t.Fatal("foreign peer refused below the limit")
		}
		attempt := s.attempts[peer]
		attempt.at = time.Now().Add(time.Duration(i-maxPendingPeers) * time.Second)
		s.attempts[peer] = attempt
		if i == 0 {
			oldestForeign = peer
		}
	}
	legitimate := &memoryStreamConn{remote: "192.168.1.20:5001"}
	if s.admit(legitimate) == nil || !oldestForeign.closed || setupPeer.closed || len(s.attempts) != maxPendingPeers {
		t.Fatal("full map locked out alternate-family data or evicted the protected setup source")
	}
	proveBinding(t, s, legitimate)
}

func TestSameAddressAttemptReplacementPreservesOthers(t *testing.T) {
	s := bindingServer()
	prepareBinding(t, s, "2001:db8::20", 42)
	old := &memoryStreamConn{remote: "192.168.1.20:5000"}
	other := &memoryStreamConn{remote: "192.168.1.21:5000"}
	s.admit(old)
	s.admit(other)
	next := &memoryStreamConn{remote: "192.168.1.20:5001"}
	if s.admit(next) == nil || !old.closed || other.closed || len(s.attempts) != 2 {
		t.Fatal("same-address replacement evicted another source or grew the map")
	}
}

func TestExpiredPendingSetupCannotCommitValidKeyFrame(t *testing.T) {
	s := bindingServer()
	prepareBinding(t, s, "2001:db8::20", 42)
	peer := &memoryStreamConn{remote: "192.168.1.20:5000"}
	pending := s.admit(peer)
	key := pending.candidates[0]
	block, err := aes.NewCipher(key.Key)
	if err != nil {
		t.Fatal(err)
	}
	frames := 0
	s.handlers.OnFrame = func(*Frame) { frames++ }
	st := &streamState{srv: s, conn: peer, pending: pending, active: -1,
		cands: []cipher.Stream{cipher.NewCTR(block, key.IV)}, names: []string{"test"}}
	pending.expires = time.Now().Add(-time.Second)
	st.handleVideo(encryptedBindingFrame(t, pending), 0, 0)
	if st.active != 0 || s.conn != nil || s.sender.IsValid() || s.streamID != 0 || frames != 0 || peer.deadline.IsZero() {
		t.Fatal("valid decrypted frame committed ownership after setup expired")
	}
}
