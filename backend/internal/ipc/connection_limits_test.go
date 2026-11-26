package ipc

import (
	"bufio"
	"io"
	"net"
	"testing"
	"time"
)

func TestAdmissionReleasesSlotsAndClosesIdleHandlers(t *testing.T) {
	s := newTestServer()
	var held []net.Conn
	for range maxControlConnections {
		a, b := net.Pipe()
		defer b.Close()
		if !s.admit(a) {
			t.Fatal("refused an available slot")
		}
		held = append(held, a)
	}
	next, peer := net.Pipe()
	defer peer.Close()
	if s.admit(next) {
		t.Fatal("accepted beyond the connection limit")
	}
	for _, c := range held {
		c.Close()
		s.finish(c)
	}
	if !s.admit(next) {
		t.Fatal("released slot stayed unavailable")
	}
	go func() {
		defer s.finish(next)
		s.serveConn(next)
	}()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.admit(next) {
		t.Fatal("admission reopened after shutdown")
	}
}

type deadlineConn struct {
	net.Conn
	deadlines chan time.Duration
}

func (c deadlineConn) SetReadDeadline(t time.Time) error {
	c.deadlines <- time.Until(t)
	return c.Conn.SetReadDeadline(t)
}

func TestFirstRequestHasShorterIdleDeadline(t *testing.T) {
	server, peer := net.Pipe()
	defer peer.Close()
	c := deadlineConn{Conn: server, deadlines: make(chan time.Duration, 2)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		newTestServer().serveConn(c)
	}()
	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	first := <-c.deadlines
	if _, err := io.WriteString(peer, "OPTIONS * RTSP/1.0\r\nCSeq: 1\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(peer)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	established := <-c.deadlines
	if first <= 0 || first > 10*time.Second || established < idleTimeout-time.Second {
		t.Fatalf("first request %s, established session %s", first, established)
	}
	peer.Close()
	<-done
}
