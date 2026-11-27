package rtsp

import (
	"log"
	"net/netip"
	"time"
)

// Per-sender request budgets bound how much work one LAN peer can force through
// the receiver-wide control lock (pairing and FairPlay work is serialised there).
// Legitimate senders issue a few requests per session; the burst allowance is
// far above that while still capping a flood.
//
// Tokens are accounted per source address, so one device on an IPv6 LAN cannot
// drain a /64 shared by other devices. To stop privacy-address rotation from
// minting unlimited bursts, bucket creation is capped per /64 window.
const (
	requestRatePerSecond   = 100
	requestBurst           = 200
	requestBucketTTL       = 10 * time.Minute
	requestBucketLimit     = 4096
	requestPrefixCreations = 32
	requestPrefixLimit     = 4096
)

type requestBudget struct {
	tokens float64
	last   time.Time
}

type prefixWindow struct {
	count int
	start time.Time
}

// rateAddress splits a remote into the token key (full address) and the
// creation-cap key (IPv6 /64, otherwise the address itself). Unparsable remotes
// key as-is with no prefix cap.
func rateAddress(remote string) (key, prefix string) {
	addr, err := netip.ParseAddr(remote)
	if err != nil {
		return remote, ""
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is6() {
		if p, err := addr.Prefix(64); err == nil {
			return addr.String(), p.String()
		}
	}
	return addr.String(), addr.String()
}

// allowRequest applies one token from the sender's bucket. Rate limiting is
// always keyed by peer IP (never per ephemeral TCP port) so opening multiple
// connections cannot bypass the budget. An empty remote is the in-process/local
// caller used by tests and loopback helpers, which is not rate-limited.
func (s *Server) allowRequest(remote string) bool {
	remote = ipOf(remote)
	if remote == "" {
		return true
	}
	key, prefix := rateAddress(remote)
	now := time.Now()
	s.limMu.Lock()
	defer s.limMu.Unlock()
	if s.limiters == nil {
		s.limiters = make(map[string]*requestBudget)
	}
	b := s.limiters[key]
	if b == nil {
		if prefix != "" && !s.allowNewPrefixLocked(prefix, now) {
			return false
		}
		if len(s.limiters) >= requestBucketLimit {
			s.evictOldestLocked()
		}
		b = &requestBudget{tokens: requestBurst, last: now}
		s.limiters[key] = b
		// Pruning only on insertion keeps the per-request path O(1).
		if len(s.limiters) > 256 {
			s.pruneLocked(now)
		}
	} else {
		elapsed := now.Sub(b.last).Seconds()
		b.tokens = min(requestBurst, b.tokens+elapsed*requestRatePerSecond)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// noteRateLimited logs at most one line per second, so a flood cannot grow the
// log without bound while being rejected.
func (s *Server) noteRateLimited(remote string) {
	now := time.Now()
	s.limMu.Lock()
	s.limitLogCount++
	if !s.limitLogAt.IsZero() && now.Sub(s.limitLogAt) < time.Second {
		s.limMu.Unlock()
		return
	}
	count := s.limitLogCount
	s.limitLogCount = 0
	s.limitLogAt = now
	s.limMu.Unlock()
	log.Printf("rtsp: rate limited %d request(s), last from %s", count, remote)
}

func (s *Server) allowNewPrefixLocked(prefix string, now time.Time) bool {
	if s.prefixes == nil {
		s.prefixes = make(map[string]*prefixWindow)
	}
	if len(s.prefixes) >= requestPrefixLimit {
		var oldestKey string
		var oldest time.Time
		found := false
		for k, v := range s.prefixes {
			if !found || v.start.Before(oldest) {
				oldestKey, oldest, found = k, v.start, true
			}
		}
		if found {
			delete(s.prefixes, oldestKey)
		}
	}
	w := s.prefixes[prefix]
	if w == nil || now.Sub(w.start) > requestBucketTTL {
		w = &prefixWindow{start: now}
		s.prefixes[prefix] = w
	}
	if w.count >= requestPrefixCreations {
		return false
	}
	w.count++
	return true
}

func (s *Server) pruneLocked(now time.Time) {
	for ip, v := range s.limiters {
		if now.Sub(v.last) > requestBucketTTL {
			delete(s.limiters, ip)
		}
	}
	for p, w := range s.prefixes {
		if now.Sub(w.start) > requestBucketTTL {
			delete(s.prefixes, p)
		}
	}
}

func (s *Server) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	found := false
	for key, v := range s.limiters {
		if !found || v.last.Before(oldest) {
			oldestKey, oldest, found = key, v.last, true
		}
	}
	if found {
		delete(s.limiters, oldestKey)
	}
}
