package ipc

import (
	"bytes"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"howett.net/plist"
)

func TestTeardownDelegatesSessionLifetime(t *testing.T) {
	for _, streamType := range []uint64{96, 110} {
		body, err := plist.Marshal(map[string]any{
			"streams": []any{map[string]any{"type": streamType}},
		}, plist.BinaryFormat)
		if err != nil {
			t.Fatal(err)
		}
		s := newTestServer()
		teardowns := 0
		s.OnTeardown = func(remote string, got []byte) {
			teardowns++
			if remote != "192.168.1.20" || !bytes.Equal(got, body) {
				t.Errorf("TEARDOWN hook received remote %q, body %x", remote, got)
			}
		}
		s.OnDisconnect = func(remote string) {
			t.Errorf("TEARDOWN invoked the connection-close hook for %q", remote)
		}
		resp := s.dispatch(&Request{
			Method: "TEARDOWN", Target: "/stream", Protocol: "RTSP/1.0", Body: body,
		}, "192.168.1.20")
		if teardowns != 1 || resp.status() != 200 {
			t.Fatalf("TEARDOWN type %d: %d hook calls, status %d", streamType, teardowns, resp.status())
		}
	}
}

func TestHTTPStopReleasesAllSessions(t *testing.T) {
	s := newTestServer()
	sessions := map[string]bool{"192.168.1.20": true, "2001:db8::20": true}
	stops := 0
	s.OnStop = func() {
		stops++
		clear(sessions)
	}
	s.OnDisconnect = func(remote string) {
		t.Errorf("HTTP /stop used a per-sender disconnect for %q", remote)
	}
	for range 2 {
		resp := s.dispatch(&Request{Method: "POST", Target: "/stop", Protocol: "HTTP/1.1"}, "127.0.0.1")
		if resp.status() != 200 || resp.ContentType != "" || len(resp.Body) != 0 {
			t.Fatalf("HTTP /stop changed its empty 200 response: %+v", resp)
		}
		if len(sessions) != 0 {
			t.Fatalf("HTTP /stop left %d sessions", len(sessions))
		}
	}
	if stops != 2 {
		t.Fatalf("stop hook called %d times, want 2", stops)
	}
	// The endpoint remains optional for users of Server without a receiver.
	s.OnStop = nil
	if resp := s.dispatch(&Request{Method: "POST", Target: "/stop", Protocol: "HTTP/1.1"}, "127.0.0.1"); resp.status() != 200 {
		t.Fatalf("HTTP /stop without a hook returned %d", resp.status())
	}
}

func TestHTTPStopRestrictsToLoopback(t *testing.T) {
	cases := []struct {
		remote   string
		wantCode int
		wantStop bool
	}{
		{"127.0.0.1", 200, true},
		{"::1", 200, true},
		{"::ffff:127.0.0.1", 200, true},
		{"192.168.1.20", 403, false},
		{"10.0.0.2", 403, false},
		{"2001:db8::20", 403, false},
		{"::ffff:192.168.1.20", 403, false},
		{"0.0.0.0", 403, false},
		{"", 403, false},
	}

	for _, tc := range cases {
		t.Run("dispatch/"+tc.remote, func(t *testing.T) {
			s := newTestServer()
			called := false
			s.OnStop = func() { called = true }

			resp := s.dispatch(&Request{Method: "POST", Target: "/stop", Protocol: "HTTP/1.1"}, tc.remote)
			if resp.status() != tc.wantCode {
				t.Fatalf("POST /stop from %q -> %d, want %d", tc.remote, resp.status(), tc.wantCode)
			}
			if called != tc.wantStop {
				t.Fatalf("POST /stop from %q OnStop called=%v, want %v", tc.remote, called, tc.wantStop)
			}
		})
	}

	// Verify serveConn wire behavior for LAN vs loopback /stop, and that /info
	// remains LAN-accessible.
	for _, tc := range []struct {
		name       string
		remoteAddr string
		req        string
		wantPrefix string
		wantStop   bool
	}{
		{"lan stop rejected", "192.168.1.20:43210", "POST /stop HTTP/1.1\r\n\r\n", "HTTP/1.1 403 Forbidden\r\n", false},
		{"loopback stop accepted", "127.0.0.1:43210", "POST /stop HTTP/1.1\r\n\r\n", "HTTP/1.1 200 OK\r\n", true},
		{"ipv6 loopback stop accepted", "[::1]:43210", "POST /stop HTTP/1.1\r\n\r\n", "HTTP/1.1 200 OK\r\n", true},
		{"lan info accepted", "192.168.1.20:43210", "GET /info RTSP/1.0\r\nCSeq: 1\r\n\r\n", "RTSP/1.0 200 OK\r\n", false},
	} {
		t.Run("wire/"+tc.name, func(t *testing.T) {
			conn := &memoryControlConn{
				input:  strings.NewReader(tc.req),
				remote: testConnAddr(tc.remoteAddr),
			}
			s := newTestServer()
			called := false
			s.OnStop = func() { called = true }
			s.serveConn(conn)
			got := conn.output.String()
			if !strings.HasPrefix(got, tc.wantPrefix) {
				t.Fatalf("wire reply = %q, want prefix %q", got, tc.wantPrefix)
			}
			if called != tc.wantStop {
				t.Fatalf("OnStop called=%v, want %v", called, tc.wantStop)
			}
		})
	}
}

type testConnAddr string

func (a testConnAddr) Network() string { return "tcp" }
func (a testConnAddr) String() string  { return string(a) }

// memoryControlConn exercises every serveConn return without sockets, timers,
// or a second goroutine; EOF and I/O failures have the same handler paths.
type memoryControlConn struct {
	input    io.Reader
	output   bytes.Buffer
	remote   net.Addr
	readErr  error
	writeErr error
	closed   bool
}

func (c *memoryControlConn) Read(p []byte) (int, error) {
	if c.readErr != nil {
		return 0, c.readErr
	}
	return c.input.Read(p)
}

func (c *memoryControlConn) Write(p []byte) (int, error) {
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return c.output.Write(p)
}

func (c *memoryControlConn) Close() error                     { c.closed = true; return nil }
func (c *memoryControlConn) LocalAddr() net.Addr              { return testConnAddr("127.0.0.1:7000") }
func (c *memoryControlConn) RemoteAddr() net.Addr             { return c.remote }
func (c *memoryControlConn) SetDeadline(time.Time) error      { return nil }
func (c *memoryControlConn) SetReadDeadline(time.Time) error  { return nil }
func (c *memoryControlConn) SetWriteDeadline(time.Time) error { return nil }

func TestServeConnDisconnectsOnceOnEveryReturn(t *testing.T) {
	const options = "OPTIONS * RTSP/1.0\r\nCSeq: 1\r\n\r\n"
	for _, tc := range []struct {
		name     string
		input    string
		readErr  error
		writeErr error
		teardown bool
	}{
		{name: "EOF"},
		{name: "read failure", readErr: errors.New("connection reset")},
		{name: "write failure", input: options, writeErr: errors.New("broken pipe")},
		{name: "malformed request", input: "not a request\r\n"},
		{name: "Connection close", input: "OPTIONS * RTSP/1.0\r\nConnection: close\r\n\r\n"},
		{name: "full teardown then EOF", input: "TEARDOWN /stream RTSP/1.0\r\n\r\n" + options, teardown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &memoryControlConn{
				input: strings.NewReader(tc.input), remote: testConnAddr("192.168.1.20:43210"),
				readErr: tc.readErr, writeErr: tc.writeErr,
			}
			s := newTestServer()
			var events []string
			s.OnTeardown = func(remote string, body []byte) {
				events = append(events, "teardown")
			}
			s.OnDisconnect = func(remote string) {
				if remote != "192.168.1.20" || !conn.closed {
					t.Errorf("disconnect remote %q, connection closed %v", remote, conn.closed)
				}
				events = append(events, "disconnect")
			}
			s.serveConn(conn)
			want := []string{"disconnect"}
			if tc.teardown {
				want = []string{"teardown", "disconnect"}
			}
			if !conn.closed || !slices.Equal(events, want) {
				t.Fatalf("closed %v, events %v, want %v", conn.closed, events, want)
			}
		})
	}
}

// A sender may hold auxiliary control connections (for example a short /info
// request) while its mirror session is live. Only closing the last connection
// may forget the session.
func TestDisconnectOnlyFiresForLastConnection(t *testing.T) {
	s := newTestServer()
	s.retainConn("192.168.1.20")
	s.retainConn("192.168.1.20")
	if s.releaseConn("192.168.1.20") {
		t.Fatal("first connection close reported the sender disconnected")
	}
	if !s.releaseConn("192.168.1.20") {
		t.Fatal("last connection close did not report a disconnect")
	}
	if s.releaseConn("192.168.1.20") {
		t.Fatal("an extra release reported a disconnect")
	}
}

func TestServeConnDisconnectFiltersLocalAddresses(t *testing.T) {
	for _, tc := range []struct {
		remote string
		want   string
	}{
		{"192.168.1.20:43210", "192.168.1.20"},
		{"[2001:db8::20]:43210", "2001:db8::20"},
		{"[fe80::20%7]:43210", "fe80::20%7"},
		{"[::ffff:192.168.1.20]:43210", "::ffff:192.168.1.20"},
		{"", ""},
		{"pipe", ""},
		{"127.0.0.1:43210", ""},
		{"[::1]:43210", ""},
		{"[::ffff:127.0.0.1]:43210", ""},
		{"0.0.0.0:43210", ""},
		{"[::]:43210", ""},
	} {
		t.Run(tc.remote, func(t *testing.T) {
			conn := &memoryControlConn{input: strings.NewReader(""), remote: testConnAddr(tc.remote)}
			s := newTestServer()
			var remotes []string
			s.OnDisconnect = func(remote string) { remotes = append(remotes, remote) }
			s.serveConn(conn)
			if tc.want == "" {
				if len(remotes) != 0 {
					t.Fatalf("local connection disconnected receiver sessions: %v", remotes)
				}
			} else if !slices.Equal(remotes, []string{tc.want}) {
				t.Fatalf("disconnect remotes = %v, want [%s]", remotes, tc.want)
			}
		})
	}
}
