package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/miku0139/airplay-go/backend/internal/config"
	"github.com/miku0139/airplay-go/backend/internal/protocol"
)

func newTestAPIHandler() http.Handler {
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
	return apiHandler(info, 7000, cfg, newRuntimeState(), nil, nil, nil, "")
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
