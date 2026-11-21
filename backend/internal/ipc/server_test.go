package ipc

import (
	"bytes"
	"strings"
	"testing"

	"howett.net/plist"

	"github.com/miku0139/airplay-go/backend/internal/protocol"
)

// wireStatus resolves the status a reply will go out with. Handlers leave
// Status at its zero value to mean 200, so the reply object is not the answer.
func wireStatus(t *testing.T, resp *Response, _ *Request) int {
	t.Helper()
	return resp.status()
}

// newTestServer builds a control server with a fixed identity and a TXT hook
// that returns deterministic record bytes.
func newTestServer() *Server {
	info := protocol.NewDeviceInfo("映屏", "AA:BB:CC:DD:EE:FF")
	info.PairingID = "00000000-1111-2222-3333-444444444444"
	info.PublicKey = "abcdef"
	s := New(info, Display{Width: 1920, Height: 1080})
	s.TXTRecord = func(raop bool) []byte {
		if raop {
			return []byte("\x09txtRAOP=1")
		}
		return []byte("\x09txtAirP=1")
	}
	return s
}

// txtReply decodes a /info reply and returns the TXT blob it carries.
func txtReply(t *testing.T, resp *Response, key string) []byte {
	t.Helper()
	if wires := wireStatus(t, resp, nil); wires != 200 {
		t.Fatalf("status = %d, want 200", wires)
	}
	if resp.ContentType != "application/x-apple-binary-plist" {
		t.Fatalf("content type = %q, want a binary plist", resp.ContentType)
	}
	var m map[string]any
	if _, err := plist.Unmarshal(resp.Body, &m); err != nil {
		t.Fatalf("reply is not a plist: %v", err)
	}
	if len(m) != 1 {
		t.Fatalf("reply has %d keys, want only %q: %v", len(m), key, keysOf(m))
	}
	data, ok := m[key].([]byte)
	if !ok {
		t.Fatalf("%q value is %T, want data", key, m[key])
	}
	return data
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestInfoTXTByQueryString(t *testing.T) {
	s := newTestServer()

	resp := s.dispatch(&Request{Method: "GET", Target: "/info?txtAirPlay", Protocol: "RTSP/1.0"}, "10.0.0.2")
	if got := txtReply(t, resp, "txtAirPlay"); !bytes.Equal(got, []byte("\x09txtAirP=1")) {
		t.Fatalf("txtAirPlay = %q", got)
	}

	resp = s.dispatch(&Request{Method: "GET", Target: "/info?txtRAOP", Protocol: "RTSP/1.0"}, "10.0.0.2")
	if got := txtReply(t, resp, "txtRAOP"); !bytes.Equal(got, []byte("\x09txtRAOP=1")) {
		t.Fatalf("txtRAOP = %q", got)
	}
}

func TestInfoTXTByBodyQualifier(t *testing.T) {
	s := newTestServer()
	body, err := plist.Marshal(map[string]any{"qualifier": []any{"txtAirPlay"}}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}

	resp := s.dispatch(&Request{
		Method:   "GET",
		Target:   "/info",
		Protocol: "RTSP/1.0",
		Headers:  map[string]string{"content-type": "application/x-apple-binary-plist"},
		Body:     body,
	}, "10.0.0.2")
	if got := txtReply(t, resp, "txtAirPlay"); !bytes.Equal(got, []byte("\x09txtAirP=1")) {
		t.Fatalf("txtAirPlay = %q", got)
	}
}

// A plain /info must still be the full device dictionary: the TXT branch must
// not swallow it.
func TestInfoWithoutQualifierReturnsDeviceDictionary(t *testing.T) {
	s := newTestServer()
	resp := s.dispatch(&Request{Method: "GET", Target: "/info", Protocol: "RTSP/1.0"}, "10.0.0.2")
	if wireStatus(t, resp, nil) != 200 || resp.ContentType != "application/x-apple-binary-plist" {
		t.Fatalf("status %d type %q", wireStatus(t, resp, nil), resp.ContentType)
	}
	var m map[string]any
	if _, err := plist.Unmarshal(resp.Body, &m); err != nil {
		t.Fatalf("reply is not a plist: %v", err)
	}
	if _, ok := m["features"]; !ok {
		t.Fatalf("device dictionary lacks features: %v", keysOf(m))
	}
	if _, ok := m["txtAirPlay"]; ok {
		t.Fatal("device dictionary must not carry the TXT record")
	}

	// deviceID and macAddress agree and use the _airplay form.
	if m["deviceID"] != "aa:bb:cc:dd:ee:ff" || m["macAddress"] != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("deviceID %v macAddress %v", m["deviceID"], m["macAddress"])
	}
}

func TestInfoRefreshRateIsFrameInterval(t *testing.T) {
	s := newTestServer()
	resp := s.dispatch(&Request{Method: "GET", Target: "/info", Protocol: "RTSP/1.0"}, "10.0.0.2")
	var m map[string]any
	if _, err := plist.Unmarshal(resp.Body, &m); err != nil {
		t.Fatal(err)
	}
	displays, _ := m["displays"].([]any)
	if len(displays) != 1 {
		t.Fatalf("displays = %v", m["displays"])
	}
	d, _ := displays[0].(map[string]any)
	rate, _ := d["refreshRate"].(float64)
	if rate <= 0 || rate > 1 {
		t.Fatalf("refreshRate = %v, want a frame interval near 1/60", d["refreshRate"])
	}
}

// A known method on an unknown URL is tolerated with 200, matching UxPlay.
func TestUnknownURLIsTolerated(t *testing.T) {
	s := newTestServer()
	for _, target := range []string{"/event", "/volume", "/pair-remove"} {
		resp := s.dispatch(&Request{Method: "POST", Target: target, Protocol: "RTSP/1.0"}, "10.0.0.2")
		if wires := wireStatus(t, resp, nil); wires != 200 {
			t.Fatalf("POST %s -> %d, want 200", target, wires)
		}
	}
}

func TestAudioModeIsEmptyOK(t *testing.T) {
	s := newTestServer()
	resp := s.dispatch(&Request{Method: "POST", Target: "/audioMode", Protocol: "RTSP/1.0"}, "10.0.0.2")
	if wireStatus(t, resp, nil) != 200 || resp.ContentType != "" || len(resp.Body) != 0 {
		t.Fatalf("audioMode -> %d %q %d bytes", wireStatus(t, resp, nil), resp.ContentType, len(resp.Body))
	}
}

func TestOptionsAdvertisesOnlyImplementedMethods(t *testing.T) {
	s := newTestServer()
	resp := s.dispatch(&Request{Method: "OPTIONS", Target: "*", Protocol: "RTSP/1.0"}, "10.0.0.2")
	var public string
	for _, h := range resp.Headers {
		if h.name == "Public" {
			public = h.value
		}
	}
	if public == "" {
		t.Fatal("OPTIONS reply lacks Public")
	}
	for _, unsupported := range []string{"ANNOUNCE", "PAUSE"} {
		if strings.Contains(public, unsupported) {
			t.Fatalf("Public advertises %s which the router does not serve: %q", unsupported, public)
		}
	}
}

// An empty handler result is a bare 200: that is how a pair-verify the state
// machine cannot process is answered.
func TestEmptyHandlerResultIsBareOK(t *testing.T) {
	s := newTestServer()
	s.OnPairVerify = func(string, []byte) ([]byte, error) { return nil, nil }
	resp := s.dispatch(&Request{Method: "POST", Target: "/pair-verify", Protocol: "RTSP/1.0"}, "10.0.0.2")
	if wireStatus(t, resp, nil) != 200 || resp.ContentType != "" || len(resp.Body) != 0 {
		t.Fatalf("pair-verify -> %d %q %d bytes", wireStatus(t, resp, nil), resp.ContentType, len(resp.Body))
	}
}

func TestFirstQualifier(t *testing.T) {
	cases := []struct {
		in   map[string]any
		want string
	}{
		{map[string]any{"qualifier": []any{"txtAirPlay"}}, "txtAirPlay"},
		{map[string]any{"qualifier": []any{"other", "txtRAOP"}}, "txtRAOP"},
		{map[string]any{"qualifier": "txtAirPlay"}, "txtAirPlay"},
		{map[string]any{"qualifier": []any{"device"}}, ""},
		{map[string]any{}, ""},
	}
	for _, c := range cases {
		if got := firstQualifier(c.in); got != c.want {
			t.Fatalf("firstQualifier(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
