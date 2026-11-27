package rtsp

import "testing"

func TestGenericControlRequestsDoNotResumePairingOrSetup(t *testing.T) {
	s := newTestServer()
	unexpected := func(remote string, body []byte) ([]byte, error) {
		t.Errorf("generic control request resumed a session for %q", remote)
		return nil, nil
	}
	s.OnFPSetup, s.OnPairSetup, s.OnPairVerify = unexpected, unexpected, unexpected
	s.OnSetup = func(remote string, _ map[string]any) (map[string]any, error) {
		t.Errorf("generic control request resumed setup for %q", remote)
		return nil, nil
	}
	for _, req := range []*Request{
		{Method: "OPTIONS", Target: "*", Protocol: "RTSP/1.0"},
		{Method: "GET", Target: "/info", Protocol: "RTSP/1.0"},
	} {
		if resp := s.dispatch(req, "192.168.1.20"); resp.status() != 200 {
			t.Fatalf("control request returned %d", resp.status())
		}
	}
}
