package mirror

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
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

func TestStreamRequiresFreshSetupFromMatchingIP(t *testing.T) {
	for _, name := range []string{"no setup", "wrong IP", "expired setup"} {
		t.Run(name, func(t *testing.T) {
			s := bindingServer()
			if name != "no setup" {
				prepareBinding(t, s, "192.168.1.20", 42)
			}
			conn := &memoryStreamConn{remote: "192.168.1.20:5000"}
			if name == "wrong IP" {
				conn.remote = "192.168.1.21:5000"
			}
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
	s.admit(old)
	s.lastFrame = time.Now()
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
	if s.admit(real) == nil || !old.closed || s.conn != real {
		t.Fatal("a half-open socket locked out the legitimate sender")
	}
	s.lastFrame = time.Now()
	s.pending = nil // A valid frame consumes the initial setup.
	unprepared := &memoryStreamConn{remote: "192.168.1.20:5002"}
	if s.admit(unprepared) != nil || real.closed {
		t.Fatal("connection without a fresh setup preempted a live stream")
	}
	prepareBinding(t, s, "192.168.1.20", 43)
	if real.closed {
		t.Fatal("preparing a same-sender reconnect closed the healthy stream early")
	}
	reconnect := &memoryStreamConn{remote: "192.168.1.20:5003"}
	if s.admit(reconnect) == nil || !real.closed || s.streamID != 43 {
		t.Fatal("legitimate reconnect did not use the new stream ID")
	}
}

func TestStaleStreamCanBeReplaced(t *testing.T) {
	for _, validFrame := range []bool{false, true} {
		s := bindingServer()
		prepareBinding(t, s, "192.168.1.20", 42)
		old := &memoryStreamConn{remote: "192.168.1.20:5000"}
		s.admit(old)
		s.connectedAt = time.Now().Add(-pendingLifetime - time.Second)
		s.pending.expires = s.connectedAt
		if validFrame {
			s.lastFrame = s.connectedAt
		}
		if s.Busy() {
			t.Fatal("stale/half-open stream retained ownership indefinitely")
		}
		prepareBinding(t, s, "192.168.1.21", 99)
		next := &memoryStreamConn{remote: "192.168.1.21:5001"}
		if s.admit(next) == nil || !old.closed || s.conn != next {
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
				s.serve(conn) // EOF from an admitted connection.
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
	s.handlers.OnConnected = func(string) {
		prepareBinding(t, s, "192.168.1.20", 43)
		if s.admit(next) == nil {
			t.Fatal("reconnect refused")
		}
	}
	s.serve(old)
	if disconnects != 0 || s.conn != next || s.pending == nil || s.pending.streamID != 43 {
		t.Fatal("superseded connection's cleanup erased the reconnect")
	}
}
