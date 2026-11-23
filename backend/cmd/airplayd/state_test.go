package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/miku0139/airplay-go/backend/internal/config"
	"github.com/miku0139/airplay-go/backend/internal/protocol"
	"github.com/miku0139/airplay-go/backend/internal/receiver"
)

func newTestAPIHandler(receivers ...receiverControl) http.Handler {
	info := protocol.NewDeviceInfo("鏡屏", "AA:BB:CC:DD:EE:FF")
	cfg := &config.Config{
		Name:        "鏡屏",
		Width:       1920,
		Height:      1080,
		MaxFPS:      60,
		DecoderMode: "auto",
		ColorRange:  "auto",
		ScaleFilter: "fast",
		AudioBuffer: "balanced",
	}
	var recv receiverControl
	if len(receivers) > 0 {
		recv = receivers[0]
	}
	return apiHandler(info, 7000, cfg, newRuntimeState(), nil, recv, nil, "")
}

type trustResetControl struct {
	resets                    int
	err                       error
	pending                   []receiver.PendingSender
	approved, rejected        []string
	approvalErr, rejectionErr error
}

func (c *trustResetControl) Disconnect() {}
func (c *trustResetControl) ClearTrustedSenders() error {
	c.resets++
	if c.err == nil {
		c.pending = nil
	}
	return c.err
}

func (c *trustResetControl) PendingSenders() []receiver.PendingSender {
	return append([]receiver.PendingSender(nil), c.pending...)
}

func (c *trustResetControl) AudioLateDropped() uint64 { return 0 }

func (c *trustResetControl) ApproveSender(fingerprint string) error {
	if c.approvalErr != nil {
		return c.approvalErr
	}
	for i, pending := range c.pending {
		if pending.Fingerprint == fingerprint {
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			c.approved = append(c.approved, fingerprint)
			return nil
		}
	}
	return receiver.ErrNotPending
}

func (c *trustResetControl) RejectSender(fingerprint string) error {
	if c.rejectionErr != nil {
		return c.rejectionErr
	}
	for i, pending := range c.pending {
		if pending.Fingerprint == fingerprint {
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			break
		}
	}
	c.rejected = append(c.rejected, fingerprint)
	return nil
}

func TestTrustRecoveryEndpointIsLoopbackOnlyAndIdempotent(t *testing.T) {
	control := &trustResetControl{}
	h := newTestAPIHandler(control)
	for _, remote := range []string{"127.0.0.1:1234", "[::1]:1234", "[::ffff:127.0.0.1]:1234"} {
		req := httptest.NewRequest(http.MethodPost, "/api/pairing/trust/reset", nil)
		req.Host = "127.0.0.1:54321"
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" ||
			strings.TrimSpace(rec.Body.String()) != `{"ok":true}` {
			t.Fatalf("trust reset from %s = %d %q", remote, rec.Code, rec.Body.String())
		}
	}
	if control.resets != 3 {
		t.Fatalf("trust reset called receiver %d times, want 3", control.resets)
	}
	for _, tc := range []struct {
		name, method, remote, host, origin string
		status                             int
	}{
		{"LAN source with loopback Host", http.MethodPost, "192.168.1.20:1234", "127.0.0.1:54321", "", 403},
		{"IPv6 LAN source", http.MethodPost, "[2001:db8::20]:1234", "127.0.0.1:54321", "", 403},
		{"missing source", http.MethodPost, "", "127.0.0.1:54321", "", 403},
		{"invalid source", http.MethodPost, "localhost:1234", "127.0.0.1:54321", "", 403},
		{"cross origin", http.MethodPost, "127.0.0.1:1234", "127.0.0.1:54321", "http://evil.example", 403},
		{"opaque origin", http.MethodPost, "127.0.0.1:1234", "127.0.0.1:54321", "null", 403},
		{"rebound Host", http.MethodPost, "127.0.0.1:1234", "evil.example:54321", "", 403},
		{"read method", http.MethodGet, "127.0.0.1:1234", "127.0.0.1:54321", "", 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/pairing/trust/reset", nil)
			req.Host, req.RemoteAddr = tc.host, tc.remote
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status || control.resets != 3 {
				t.Fatalf("rejected reset = %d, receiver calls %d", rec.Code, control.resets)
			}
		})
	}
}

func TestTrustRecoveryEndpointReportsFailures(t *testing.T) {
	for _, control := range []receiverControl{nil, &trustResetControl{err: errors.New("trust persistence failed")}} {
		h := newTestAPIHandler(control)
		req := httptest.NewRequest(http.MethodPost, "/api/pairing/trust/reset", nil)
		req.Host, req.RemoteAddr = "127.0.0.1:54321", "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		want := "receiver unavailable"
		if control != nil {
			want = "trust persistence failed"
		}
		if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("failed trust reset = %d %q", rec.Code, rec.Body.String())
		}
	}
}

func TestAPIHostAndOriginValidation(t *testing.T) {
	h := newTestAPIHandler()

	accepted := []struct {
		name   string
		host   string
		origin string
	}{
		{name: "ipv4 loopback no port no origin", host: "127.0.0.1"},
		{name: "ipv4 loopback with port no origin", host: "127.0.0.1:54321"},
		{name: "localhost no port no origin", host: "localhost"},
		{name: "localhost with port no origin", host: "localhost:54321"},
		{name: "ipv6 loopback no port no origin", host: "[::1]"},
		{name: "ipv6 loopback with port no origin", host: "[::1]:54321"},
		{name: "ipv4 origin", host: "127.0.0.1:54321", origin: "http://127.0.0.1:8080"},
		{name: "localhost origin", host: "localhost:54321", origin: "http://localhost:3000"},
	}

	for _, tc := range accepted {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
			req.RemoteAddr = "127.0.0.1:1234"
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
			}
			var body apiState
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("invalid JSON body: %v", err)
			}
			if body.Name != "鏡屏" || body.Port != 7000 {
				t.Fatalf("unexpected apiState body: %+v", body)
			}
		})
	}

	rejected := []struct {
		name   string
		method string
		path   string
		host   string
		origin string
		emptyO bool
	}{
		{name: "empty host", method: http.MethodGet, path: "/api/state", host: ""},
		{name: "dns rebind host", method: http.MethodGet, path: "/api/state", host: "rebind.evil.com:54321"},
		{name: "subdomain spoof host", method: http.MethodGet, path: "/api/state", host: "localhost.evil.com:54321"},
		{name: "lan ip host", method: http.MethodPost, path: "/api/disconnect", host: "192.168.1.10:54321"},
		{name: "bare ipv6 host", method: http.MethodGet, path: "/api/state", host: "::1"},
		{name: "host invalid port", method: http.MethodGet, path: "/api/state", host: "127.0.0.1:abc"},
		{name: "host empty port", method: http.MethodGet, path: "/api/state", host: "localhost:"},
		{name: "evil origin", method: http.MethodPost, path: "/api/disconnect", host: "127.0.0.1:54321", origin: "http://evil.com"},
		{name: "null origin", method: http.MethodPost, path: "/api/disconnect", host: "127.0.0.1:54321", origin: "null"},
		{name: "https localhost origin", method: http.MethodPost, path: "/api/audio", host: "127.0.0.1:54321", origin: "https://localhost:3000"},
		{name: "origin without port", method: http.MethodGet, path: "/api/state", host: "127.0.0.1:54321", origin: "http://127.0.0.1"},
		{name: "origin with path", method: http.MethodGet, path: "/api/state", host: "127.0.0.1:54321", origin: "http://localhost:3000/app"},
		{name: "origin subdomain spoof", method: http.MethodGet, path: "/api/state", host: "127.0.0.1:54321", origin: "http://127.0.0.1.evil.com:3000"},
		{name: "empty origin header", method: http.MethodGet, path: "/api/state", host: "127.0.0.1:54321", emptyO: true},
	}

	for _, tc := range rejected {
		t.Run("reject/"+tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.RemoteAddr = "127.0.0.1:1234"
			req.Host = tc.host
			if tc.emptyO {
				req.Header.Set("Origin", "")
			} else if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
			if strings.TrimSpace(rec.Body.String()) == "" {
				t.Fatal("403 response had empty body")
			}
		})
	}
}
