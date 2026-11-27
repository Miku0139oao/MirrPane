package airplayd

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Miku0139oao/MirrPane/backend/internal/receiver"
	"github.com/Miku0139oao/MirrPane/backend/internal/trust"
)

func approvalAPIRequest(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host, req.RemoteAddr = "127.0.0.1:54321", "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAPIStateIncludesPendingSenderOrNull(t *testing.T) {
	fingerprint := trust.Fingerprint([]byte("sender"))
	for _, control := range []receiverControl{nil, &trustResetControl{}, &trustResetControl{
		pending: []receiver.PendingSender{{Fingerprint: fingerprint, Remote: "2001:db8::20", AgeSeconds: 42}},
	}} {
		rec := approvalAPIRequest(newTestAPIHandler(control), http.MethodGet, "/api/state", "")
		var body map[string]json.RawMessage
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			t.Fatalf("state = %d %q", rec.Code, rec.Body.String())
		}
		pending, exists := body["pendingSender"]
		if !exists || body["name"] == nil || body["port"] == nil || body["videoFrames"] == nil || body["audioBuffer"] == nil {
			t.Fatal("pendingSender or existing state keys disappeared")
		}
		if control == nil || len(control.PendingSenders()) == 0 {
			if string(pending) != "null" {
				t.Fatalf("empty pendingSender = %s, want null", pending)
			}
			continue
		}
		var sender receiver.PendingSender
		if err := json.Unmarshal(pending, &sender); err != nil || sender.Fingerprint != fingerprint || sender.Remote != "2001:db8::20" || sender.AgeSeconds != 42 {
			t.Fatalf("pendingSender = %s, %v", pending, err)
		}
	}
}

func TestApprovalAndRejectionAPI(t *testing.T) {
	fingerprint := trust.Fingerprint([]byte("sender"))
	body := `{"fingerprint":"` + fingerprint + `"}`
	for _, action := range []string{"approve", "reject", "reset"} {
		t.Run(action, func(t *testing.T) {
			control := &trustResetControl{pending: []receiver.PendingSender{{Fingerprint: fingerprint, Remote: "192.168.1.20"}}}
			h := newTestAPIHandler(control)
			rec := approvalAPIRequest(h, http.MethodPost, "/api/pairing/trust/"+action, body)
			if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"ok":true}` || rec.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("%s = %d %q", action, rec.Code, rec.Body.String())
			}
			if len(control.pending) != 0 || (action == "approve" && len(control.approved) != 1) ||
				(action == "reject" && len(control.rejected) != 1) || (action == "reset" && control.resets != 1) {
				t.Fatal("approval API did not call the matching receiver action")
			}
			state := approvalAPIRequest(h, http.MethodGet, "/api/state", "")
			if !strings.Contains(state.Body.String(), `"pendingSender":null`) {
				t.Fatal("resolved approval remained visible in state")
			}
		})
	}
	control := &trustResetControl{}
	h := newTestAPIHandler(control)
	if rec := approvalAPIRequest(h, http.MethodPost, "/api/pairing/trust/approve", body); rec.Code != 404 {
		t.Fatalf("unknown approval = %d, want 404", rec.Code)
	}
	for range 2 {
		if rec := approvalAPIRequest(h, http.MethodPost, "/api/pairing/trust/reject", body); rec.Code != 200 {
			t.Fatalf("idempotent reject = %d", rec.Code)
		}
	}
}

func TestApprovalAPIBadInputAndPersistenceErrors(t *testing.T) {
	fingerprint := trust.Fingerprint([]byte("sender"))
	validBody := `{"fingerprint":"` + fingerprint + `"}`
	for _, action := range []string{"approve", "reject"} {
		for _, body := range []string{"", "null", "[]", "{}", `{"fingerprint":1}`, `{"fingerprint":"bad"}`,
			validBody + `{}`, strings.TrimSuffix(validBody, "}") + `,"extra":true}`, `{"fingerprint":"` + strings.Repeat("a", 2000) + `"}`} {
			control := &trustResetControl{}
			rec := approvalAPIRequest(newTestAPIHandler(control), http.MethodPost, "/api/pairing/trust/"+action, body)
			if rec.Code != 400 || len(control.approved)+len(control.rejected) != 0 {
				t.Fatalf("%s bad body = %d %q", action, rec.Code, rec.Body.String())
			}
		}
		for _, control := range []receiverControl{nil, &trustResetControl{
			approvalErr: errors.New("storage unavailable"), rejectionErr: errors.New("storage unavailable"),
		}} {
			rec := approvalAPIRequest(newTestAPIHandler(control), http.MethodPost, "/api/pairing/trust/"+action, validBody)
			if rec.Code != 500 || (control != nil && !strings.Contains(rec.Body.String(), "storage unavailable")) {
				t.Fatalf("%s failed operation = %d %q", action, rec.Code, rec.Body.String())
			}
		}
		if rec := approvalAPIRequest(newTestAPIHandler(), http.MethodGet, "/api/pairing/trust/"+action, ""); rec.Code != 405 {
			t.Fatalf("%s GET = %d, want 405", action, rec.Code)
		}
	}
}

func TestApprovalAPIRequiresLoopbackHostAndOrigin(t *testing.T) {
	body := `{"fingerprint":"` + trust.Fingerprint([]byte("sender")) + `"}`
	for _, path := range []string{"/api/state", "/api/pairing/trust/approve", "/api/pairing/trust/reject", "/api/pairing/trust/reset"} {
		for _, tc := range []struct{ remote, host, origin string }{
			{"192.168.1.20:1234", "127.0.0.1:54321", ""},
			{"[2001:db8::20]:1234", "127.0.0.1:54321", ""},
			{"", "127.0.0.1:54321", ""},
			{"127.0.0.1:1234", "evil.example:54321", ""},
			{"127.0.0.1:1234", "127.0.0.1:54321", "http://evil.example"},
			{"127.0.0.1:1234", "127.0.0.1:54321", "null"},
		} {
			control := &trustResetControl{}
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Host, req.RemoteAddr = tc.host, tc.remote
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			newTestAPIHandler(control).ServeHTTP(rec, req)
			if rec.Code != 403 || control.resets+len(control.approved)+len(control.rejected) != 0 {
				t.Fatalf("%s untrusted request = %d", path, rec.Code)
			}
		}
	}
}
