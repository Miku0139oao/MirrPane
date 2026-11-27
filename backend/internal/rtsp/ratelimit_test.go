package rtsp

import (
	"fmt"
	"testing"
)

func TestDispatchRateLimitsPerSender(t *testing.T) {
	s := newTestServer()
	req := &Request{Method: "OPTIONS", Target: "*", Protocol: "RTSP/1.0"}

	allowed, limited := 0, 0
	for i := 0; i < 500; i++ {
		switch s.dispatch(req, "192.168.1.20").status() {
		case 200:
			allowed++
		case 429:
			limited++
		default:
			t.Fatalf("unexpected status for request %d", i)
		}
	}
	if allowed < requestBurst || allowed > requestBurst+20 || limited == 0 {
		t.Fatalf("allowed=%d limited=%d, want about %d allowed then 429s", allowed, limited, requestBurst)
	}

	// A second sender has its own budget.
	if got := s.dispatch(req, "192.168.1.21").status(); got != 200 {
		t.Fatalf("second sender status = %d, want 200", got)
	}

	// In-process callers (empty remote) are never limited.
	for i := 0; i < 500; i++ {
		if got := s.dispatch(req, "").status(); got != 200 {
			t.Fatalf("local request status = %d, want 200", got)
		}
	}
}

// Tokens are per source address: a flood from one device must not rate-limit a
// different device on the same IPv6 /64.
func TestRateLimitTokensArePerAddress(t *testing.T) {
	s := newTestServer()
	req := &Request{Method: "OPTIONS", Target: "*", Protocol: "RTSP/1.0"}
	for i := 0; i < 500; i++ {
		s.dispatch(req, "2001:db8:1:2::1")
	}
	if got := s.dispatch(req, "2001:db8:1:2::1").status(); got != 429 {
		t.Fatalf("flooded address status = %d, want 429", got)
	}
	if got := s.dispatch(req, "2001:db8:1:2::2").status(); got != 200 {
		t.Fatalf("neighbour in the same /64 status = %d, want 200", got)
	}
}

// Rotating privacy addresses within one /64 cannot mint unlimited buckets.
func TestRateLimitCapsBucketsPerPrefix(t *testing.T) {
	s := newTestServer()
	req := &Request{Method: "OPTIONS", Target: "*", Protocol: "RTSP/1.0"}
	limited := false
	for i := 0; i < requestPrefixCreations+8; i++ {
		addr := fmt.Sprintf("2001:db8:2:3::%x", i+1)
		if s.dispatch(req, addr).status() == 429 {
			limited = true
		}
	}
	if !limited {
		t.Fatal("rotating addresses within one /64 never hit the creation cap")
	}
	// A different /64 is unaffected.
	if got := s.dispatch(req, "2001:db8:2:4::1").status(); got != 200 {
		t.Fatalf("second /64 status = %d, want 200", got)
	}
}

// The bucket map is bounded even under a multi-address flood.
func TestRateLimitBucketMapIsBounded(t *testing.T) {
	s := newTestServer()
	for i := 0; i < requestBucketLimit+100; i++ {
		addr := fmt.Sprintf("10.%d.%d.%d", i/65536, (i/256)%256, i%256)
		s.allowRequest(addr)
	}
	s.limMu.Lock()
	n := len(s.limiters)
	s.limMu.Unlock()
	if n > requestBucketLimit {
		t.Fatalf("limiter map holds %d entries, cap is %d", n, requestBucketLimit)
	}
}

// Ephemeral TCP ports from the same peer IP must share one rate-limit bucket so
// opening new connections cannot bypass request rate limits.
func TestRateLimitSharesBucketAcrossPortsFromSameIP(t *testing.T) {
	s := newTestServer()
	req := &Request{Method: "OPTIONS", Target: "*", Protocol: "RTSP/1.0"}
	limited := false
	for port := 50000; port < 50400; port++ {
		if s.dispatch(req, fmt.Sprintf("192.168.1.20:%d", port)).status() == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("requests spread across ephemeral ports bypassed the per-IP rate limit")
	}
	if got := s.dispatch(req, "192.168.1.21:50000").status(); got != 200 {
		t.Fatalf("different IP status = %d, want 200", got)
	}
}
