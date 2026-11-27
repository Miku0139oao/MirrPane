package receiver

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/Miku0139oao/MirrPane/backend/internal/trust"
)

const (
	maxPendingSenders   = 8
	maxRejectedSenders  = 32
	approvalLifetime    = 10 * time.Minute
	maxPendingPerSource = 2
	maxApprovalSources  = 128
)

var (
	ErrInvalidFingerprint = errors.New("receiver: invalid sender fingerprint")
	ErrNotPending         = errors.New("receiver: sender is not pending approval")
)

// PendingSender is a snapshot for the local approval UI. Labels are supplied
// by the remote device and are not proof of identity. No session keys appear.
type PendingSender struct {
	Fingerprint string `json:"fingerprint"`
	Remote      string `json:"remote"`
	AgeSeconds  int64  `json:"ageSeconds"`
	Label       string `json:"label,omitempty"`
}

type pendingApproval struct {
	remote              string
	firstSeen, lastSeen time.Time
	label               string
}

func (r *Receiver) approvalTimeLocked() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *Receiver) expireApprovalsLocked(now time.Time) {
	for fingerprint, pending := range r.pending {
		// Retries update lastSeen, but cannot extend the first-seen lifetime.
		if !now.Before(pending.firstSeen.Add(approvalLifetime)) {
			delete(r.pending, fingerprint)
			if r.pinnedSender == fingerprint {
				r.pinnedSender = ""
			}
		}
	}
	for fingerprint, expires := range r.rejected {
		if !now.Before(expires) {
			delete(r.rejected, fingerprint)
		}
	}
	for source, admissions := range r.approvalSources {
		live := admissions[:0]
		for _, admitted := range admissions {
			if now.Before(admitted.Add(approvalLifetime)) {
				live = append(live, admitted)
			}
		}
		if len(live) == 0 {
			delete(r.approvalSources, source)
		} else {
			r.approvalSources[source] = live
		}
	}
}

func approvalSource(remote string) (netip.Prefix, bool) {
	ip, err := netip.ParseAddr(remote)
	if err != nil {
		return netip.Prefix{}, false
	}
	ip = ip.Unmap().WithZone("")
	bits := 64
	if ip.Is4() {
		bits = 32
	}
	return netip.PrefixFrom(ip, bits).Masked(), true
}

func (r *Receiver) recordPendingLocked(fingerprint, remote, label string) {
	now := r.approvalTimeLocked()
	r.expireApprovalsLocked(now)
	if _, rejected := r.rejected[fingerprint]; rejected {
		return
	}
	if r.pending == nil {
		r.pending = make(map[string]pendingApproval)
	}
	pending, exists := r.pending[fingerprint]
	if !exists {
		source, valid := approvalSource(remote)
		admissions := r.approvalSources[source]
		if !valid || len(admissions) >= maxPendingPerSource {
			return
		}
		if len(admissions) == 0 && len(r.approvalSources) >= maxApprovalSources {
			// Evict the least-recently-admitted source instead of refusing a new
			// one, so a multi-source flood cannot hide the owner's device.
			var oldestSource netip.Prefix
			var oldestAt time.Time
			found := false
			for src, admitted := range r.approvalSources {
				if len(admitted) == 0 {
					continue
				}
				last := admitted[len(admitted)-1]
				if !found || last.Before(oldestAt) {
					oldestSource, oldestAt, found = src, last, true
				}
			}
			if found {
				delete(r.approvalSources, oldestSource)
			}
		}
		if len(r.pending) >= maxPendingSenders {
			var oldest string
			for key, candidate := range r.pending {
				if key == r.pinnedSender {
					continue
				}
				if oldest == "" || candidate.firstSeen.Before(r.pending[oldest].firstSeen) ||
					(candidate.firstSeen.Equal(r.pending[oldest].firstSeen) && key < oldest) {
					oldest = key
				}
			}
			if oldest == "" {
				return
			}
			delete(r.pending, oldest)
		}
		if r.approvalSources == nil {
			r.approvalSources = make(map[netip.Prefix][]time.Time)
		}
		// Keep admissions for their full TTL even after approval/rejection or
		// queue eviction; removal must not reopen a source's churn budget.
		r.approvalSources[source] = append(admissions, now)
		pending.firstSeen = now
	}
	pending.remote, pending.lastSeen = remote, now
	if label != "" {
		pending.label = label
	}
	r.pending[fingerprint] = pending
}

// PendingSenders returns oldest-first snapshots, excluding expired requests.
func (r *Receiver) PendingSenders() []PendingSender {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.approvalTimeLocked()
	r.expireApprovalsLocked(now)
	keys := make([]string, 0, len(r.pending))
	for fingerprint := range r.pending {
		keys = append(keys, fingerprint)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := r.pending[keys[i]], r.pending[keys[j]]
		if a.firstSeen.Equal(b.firstSeen) {
			return keys[i] < keys[j]
		}
		return a.firstSeen.Before(b.firstSeen)
	})
	if len(keys) > 0 {
		// /api/state displays this oldest request. It must remain actionable
		// until expiry or the owner's own approval/rejection.
		r.pinnedSender = keys[0]
	} else {
		r.pinnedSender = ""
	}
	result := make([]PendingSender, 0, len(keys))
	for _, fingerprint := range keys {
		pending := r.pending[fingerprint]
		age := max(int64(now.Sub(pending.firstSeen)/time.Second), 0)
		result = append(result, PendingSender{Fingerprint: fingerprint, Remote: pending.remote, AgeSeconds: age, Label: pending.label})
	}
	return result
}

// ApproveSender durably trusts only an unexpired pending identity. The sender
// can retry setup afterwards; an iPhone may need mirroring selected once again.
func (r *Receiver) ApproveSender(fingerprint string) error {
	if !trust.ValidFingerprint(fingerprint) {
		return ErrInvalidFingerprint
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireApprovalsLocked(r.approvalTimeLocked())
	pending, exists := r.pending[fingerprint]
	if !exists {
		return ErrNotPending
	}
	if r.trust == nil {
		return errors.New("receiver: sender trust store unavailable")
	}
	if err := r.trust.TrustSender(fingerprint); err != nil {
		return fmt.Errorf("approve sender: %w", err)
	}
	delete(r.pending, fingerprint)
	if r.pinnedSender == fingerprint {
		r.pinnedSender = ""
	}
	delete(r.rejected, fingerprint)
	if r.log != nil {
		r.log.Printf("pair: approved sender %s from %s", fingerprint, pending.remote)
	}
	return nil
}

// RejectSender dismisses a request and suppresses that fingerprint for ten
// minutes. Repeated rejection is harmless; it does not revoke approved trust.
func (r *Receiver) RejectSender(fingerprint string) error {
	if !trust.ValidFingerprint(fingerprint) {
		return ErrInvalidFingerprint
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.approvalTimeLocked()
	r.expireApprovalsLocked(now)
	delete(r.pending, fingerprint)
	if r.pinnedSender == fingerprint {
		r.pinnedSender = ""
	}
	if r.rejected == nil {
		r.rejected = make(map[string]time.Time)
	}
	if _, exists := r.rejected[fingerprint]; !exists && len(r.rejected) >= maxRejectedSenders {
		var oldest string
		for key, expires := range r.rejected {
			if oldest == "" || expires.Before(r.rejected[oldest]) || (expires.Equal(r.rejected[oldest]) && key < oldest) {
				oldest = key
			}
		}
		delete(r.rejected, oldest)
	}
	r.rejected[fingerprint] = now.Add(approvalLifetime)
	return nil
}

func senderLabel(req map[string]any) string {
	for _, key := range []string{"name", "deviceName"} {
		if label, ok := req[key].(string); ok {
			if cleaned := cleanSenderField(label); cleaned != "" {
				return cleaned
			}
		}
	}
	return ""
}

// senderDeviceID reads the sender's advertised identity. It is display data
// only; trust is decided by the pairing fingerprint.
func senderDeviceID(req map[string]any) string {
	for _, key := range []string{"deviceID", "macAddress"} {
		if value, ok := req[key].(string); ok {
			if cleaned := cleanSenderField(value); cleaned != "" {
				return cleaned
			}
		}
	}
	return ""
}

func senderModel(req map[string]any) string {
	if value, ok := req["model"].(string); ok {
		return cleanSenderField(value)
	}
	return ""
}

// cleanSenderField keeps a remote-supplied display string to one bounded line
// without control characters.
func cleanSenderField(label string) string {
	var runes []rune
	for _, r := range strings.TrimSpace(label) {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		runes = append(runes, r)
		if len(runes) == 64 {
			break
		}
	}
	return strings.TrimSpace(string(runes))
}
