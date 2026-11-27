package receiver

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Miku0139oao/MirrPane/backend/internal/trust"
)

type approvalClock struct{ now time.Time }

func approvalReceiver(t *testing.T) (*Receiver, *testMirror, *testAudio, *trust.Store, *approvalClock) {
	t.Helper()
	r, m, a := bindingReceiver(t)
	store, err := trust.Open(t.TempDir(), r.keys.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	clock := &approvalClock{now: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}
	r.now = func() time.Time { return clock.now }
	r.trust = store
	return r, m, a, store, clock
}

func requestApproval(t *testing.T, r *Receiver, remote string, identity ed25519.PrivateKey) string {
	t.Helper()
	finishVerificationAs(t, r, remote, identity)
	_, err := r.Setup(remote, streamRequest(110))
	wantProtocolError(t, err, 403, ErrUntrusted)
	return trust.Fingerprint(identity.Public().(ed25519.PublicKey))
}

func TestPendingApprovalsAreBoundedExpireAndOwnTheirSnapshot(t *testing.T) {
	r, m, a, store, clock := approvalReceiver(t)
	fingerprints := make([]string, 0, maxPendingSenders+1)
	for i := range maxPendingSenders {
		fingerprints = append(fingerprints, requestApproval(t, r, fmt.Sprintf("192.168.1.%d", 20+i), senderIdentity(t)))
		clock.now = clock.now.Add(time.Second)
	}
	// A retry updates lastSeen, but does not make the oldest request immortal.
	_, err := r.Setup("192.168.1.20", streamRequest(110))
	wantProtocolError(t, err, 403, ErrUntrusted)
	fingerprints = append(fingerprints, requestApproval(t, r, "192.168.1.99", senderIdentity(t)))
	if len(r.pending) != maxPendingSenders {
		t.Fatal("pending approval bound was exceeded")
	}
	if _, exists := r.pending[fingerprints[0]]; exists {
		t.Fatal("a retry prevented eviction of the oldest approval request")
	}
	snapshot := r.PendingSenders()
	if len(snapshot) != maxPendingSenders || snapshot[0].Fingerprint != fingerprints[1] {
		t.Fatal("pending snapshots are not oldest-first")
	}
	snapshot[0].Fingerprint, snapshot[0].Remote = "changed", "changed"
	if r.PendingSenders()[0].Fingerprint != fingerprints[1] {
		t.Fatal("snapshot mutation changed receiver state")
	}
	clock.now = r.pending[fingerprints[1]].firstSeen.Add(approvalLifetime)
	if err := r.ApproveSender(fingerprints[1]); !errors.Is(err, ErrNotPending) || store.IsTrustedSender(fingerprints[1]) {
		t.Fatal("expired pending request could be approved")
	}
	clock.now = clock.now.Add(approvalLifetime)
	if len(r.PendingSenders()) != 0 || len(r.pending) != 0 || m.prepares != 0 || a.configures != 0 {
		t.Fatal("expired approvals remained visible or configured media")
	}
}

func TestPendingMetadataRefreshDoesNotExtendExpiry(t *testing.T) {
	r, _, _, _, clock := approvalReceiver(t)
	identity := senderIdentity(t)
	fingerprint := requestApproval(t, r, "192.168.1.20", identity)
	firstSeen := clock.now
	clock.now = clock.now.Add(5 * time.Second)
	finishVerificationAs(t, r, "2001:db8::20", identity)
	_, err := r.Setup("2001:db8::20", map[string]any{"name": "  iPhone\n\u202e" + strings.Repeat("界", 100)})
	wantProtocolError(t, err, 403, ErrUntrusted)
	snapshot := r.PendingSenders()
	pending := r.pending[fingerprint]
	if len(snapshot) != 1 || snapshot[0].Remote != "2001:db8::20" || snapshot[0].AgeSeconds != 5 ||
		!pending.firstSeen.Equal(firstSeen) || !pending.lastSeen.Equal(clock.now) ||
		len([]rune(snapshot[0].Label)) != 64 || strings.ContainsAny(snapshot[0].Label, "\n\u202e") {
		t.Fatal("pending retry lost metadata, duplicated identity or exposed an unsafe label")
	}
	clock.now = firstSeen.Add(-time.Second)
	if r.PendingSenders()[0].AgeSeconds != 0 {
		t.Fatal("clock adjustment produced a negative UI age")
	}
	clock.now = firstSeen.Add(approvalLifetime)
	if len(r.PendingSenders()) != 0 {
		t.Fatal("retry extended the first-seen approval lifetime")
	}
}

func TestRejectionSuppressesRetriesAndIsBounded(t *testing.T) {
	r, m, a, store, clock := approvalReceiver(t)
	fingerprint := requestApproval(t, r, "192.168.1.20", senderIdentity(t))
	for range 2 {
		if err := r.RejectSender(fingerprint); err != nil {
			t.Fatal(err)
		}
	}
	_, err := r.Setup("192.168.1.20", streamRequest(110))
	wantProtocolError(t, err, 403, ErrUntrusted)
	if len(r.PendingSenders()) != 0 || store.IsTrustedSender(fingerprint) || m.prepares != 0 || a.configures != 0 {
		t.Fatal("rejected sender repopulated the approval UI or configured media")
	}
	if err := r.ApproveSender(fingerprint); !errors.Is(err, ErrNotPending) {
		t.Fatal("rejected identity could be approved without a fresh request")
	}
	clock.now = clock.now.Add(approvalLifetime)
	_, err = r.Setup("192.168.1.20", streamRequest(110))
	wantProtocolError(t, err, 403, ErrUntrusted)
	if len(r.PendingSenders()) != 1 {
		t.Fatal("rejection cooldown did not expire")
	}
	firstRejected := trust.Fingerprint([]byte{0})
	for i := range maxRejectedSenders + 1 {
		clock.now = clock.now.Add(time.Second)
		if err := r.RejectSender(trust.Fingerprint([]byte{byte(i)})); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.rejected) != maxRejectedSenders {
		t.Fatal("rejected fingerprint bound was exceeded")
	}
	if _, exists := r.rejected[firstRejected]; exists {
		t.Fatal("oldest rejected fingerprint was not evicted")
	}
}

func TestApprovalValidationAndResetClearAllDecisionState(t *testing.T) {
	r, m, a, store, _ := approvalReceiver(t)
	const owner = "192.168.1.20"
	approved := requestApproval(t, r, owner, senderIdentity(t))
	for _, invalid := range []string{"", "bad", "sha256:" + strings.Repeat("A", 64)} {
		if !errors.Is(r.ApproveSender(invalid), ErrInvalidFingerprint) || !errors.Is(r.RejectSender(invalid), ErrInvalidFingerprint) {
			t.Fatal("malformed fingerprint was accepted by a decision method")
		}
	}
	if err := r.ApproveSender(trust.Fingerprint([]byte("not pending"))); !errors.Is(err, ErrNotPending) {
		t.Fatal("arbitrary fingerprint could be approved")
	}
	if err := r.ApproveSender(approved); err != nil {
		t.Fatal(err)
	}
	if err := r.ApproveSender(approved); !errors.Is(err, ErrNotPending) {
		t.Fatal("consumed approval request remained pending")
	}
	if _, err := r.Setup(owner, streamKeys(t, r, owner, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Setup(owner, streamRequest(110, 96)); err != nil {
		t.Fatal(err)
	}
	rejected := requestApproval(t, r, "192.168.1.21", senderIdentity(t))
	if err := r.RejectSender(rejected); err != nil {
		t.Fatal(err)
	}
	requestApproval(t, r, "192.168.1.22", senderIdentity(t))
	if err := r.ClearTrustedSenders(); err != nil {
		t.Fatal(err)
	}
	if len(r.pending)+len(r.rejected)+len(r.sessions)+len(r.approvalSources) != 0 || len(r.PendingSenders()) != 0 || r.pinnedSender != "" ||
		store.IsTrustedSender(approved) || m.drops != 1 || a.clears != 1 || r.activeRemote != "" {
		t.Fatal("trust reset retained approved, pending, rejected or active media state")
	}
}

func TestPendingSourceChurnCannotRemoveAnotherOwnersRequest(t *testing.T) {
	r, m, a, store, _ := approvalReceiver(t)
	owner := requestApproval(t, r, "192.168.1.10", senderIdentity(t))
	if snapshot := r.PendingSenders(); len(snapshot) != 1 || snapshot[0].Fingerprint != owner {
		t.Fatal("owner request was not shown")
	}
	attackerFingerprints := make([]string, 0, 20)
	for range 20 {
		attackerFingerprints = append(attackerFingerprints, requestApproval(t, r, "192.168.1.20", senderIdentity(t)))
	}
	if len(r.pending) != 1+maxPendingPerSource || r.pinnedSender != owner {
		t.Fatal("one source churned the pending queue beyond its quota")
	}
	for i, fingerprint := range attackerFingerprints {
		_, exists := r.pending[fingerprint]
		if exists != (i < maxPendingPerSource) || store.IsTrustedSender(fingerprint) {
			t.Fatal("source churn created extra approval requests or granted trust")
		}
	}
	if err := r.ApproveSender(owner); err != nil || !store.IsTrustedSender(owner) || r.pinnedSender != "" {
		t.Fatalf("displayed owner request was no longer approvable: %v", err)
	}
	if m.prepares != 0 || a.configures != 0 {
		t.Fatal("approval churn changed media configuration")
	}
}

func TestDisplayedPendingRequestSurvivesFullQueueChurn(t *testing.T) {
	r, _, _, store, clock := approvalReceiver(t)
	owner := requestApproval(t, r, "192.168.1.10", senderIdentity(t))
	r.PendingSenders() // /api/state displays and pins this oldest request.
	for i := range maxPendingSenders + 12 {
		clock.now = clock.now.Add(time.Second)
		requestApproval(t, r, fmt.Sprintf("192.168.1.%d", 20+i), senderIdentity(t))
	}
	if len(r.pending) != maxPendingSenders || r.PendingSenders()[0].Fingerprint != owner {
		t.Fatal("multi-source queue churn evicted the displayed approval request")
	}
	if err := r.ApproveSender(owner); err != nil || !store.IsTrustedSender(owner) {
		t.Fatalf("pinned approval became stale due to churn: %v", err)
	}
}

func TestPendingSourceQuotaGroupsAddressesAndSurvivesApproval(t *testing.T) {
	for _, tc := range []struct{ name, first, second, third, other string }{
		{"IPv4 and mapped IPv4", "192.168.1.20", "::ffff:192.168.1.20", "192.168.1.20", "192.168.1.21"},
		{"IPv6 /64", "2001:db8:1:2::1", "2001:db8:1:2::2", "2001:db8:1:2::3", "2001:db8:1:3::1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, _, _, clock := approvalReceiver(t)
			first := requestApproval(t, r, tc.first, senderIdentity(t))
			requestApproval(t, r, tc.second, senderIdentity(t))
			third := requestApproval(t, r, tc.third, senderIdentity(t))
			if len(r.pending) != maxPendingPerSource {
				t.Fatal("source addresses bypassed the per-TTL quota")
			}
			if _, exists := r.pending[third]; exists {
				t.Fatal("third fingerprint from a source was admitted")
			}
			if err := r.ApproveSender(first); err != nil {
				t.Fatal(err)
			}
			_, err := r.Setup(tc.third, streamRequest(110))
			wantProtocolError(t, err, 403, ErrUntrusted)
			if _, exists := r.pending[third]; exists {
				t.Fatal("approval/removal reopened the source churn budget")
			}
			other := requestApproval(t, r, tc.other, senderIdentity(t))
			if _, exists := r.pending[other]; !exists {
				t.Fatal("different source inherited another source's quota")
			}
			clock.now = clock.now.Add(approvalLifetime)
			_, err = r.Setup(tc.third, streamRequest(110))
			wantProtocolError(t, err, 403, ErrUntrusted)
			if len(r.pending) != 1 {
				t.Fatal("source quota did not expire after its rolling TTL")
			}
			if _, exists := r.pending[third]; !exists {
				t.Fatal("new request remained blocked after source quota expiry")
			}
		})
	}
}

func TestPinnedRequestExpiresOrCanBeRejectedByOwner(t *testing.T) {
	r, _, _, _, clock := approvalReceiver(t)
	fingerprint := requestApproval(t, r, "192.168.1.20", senderIdentity(t))
	r.PendingSenders()
	if r.pinnedSender != fingerprint {
		t.Fatal("snapshot did not pin the displayed request")
	}
	if err := r.RejectSender(fingerprint); err != nil || r.pinnedSender != "" {
		t.Fatal("owner rejection did not release the displayed pin")
	}
	next := requestApproval(t, r, "192.168.1.21", senderIdentity(t))
	r.PendingSenders()
	clock.now = clock.now.Add(approvalLifetime)
	if err := r.ApproveSender(next); !errors.Is(err, ErrNotPending) || r.pinnedSender != "" || len(r.PendingSenders()) != 0 {
		t.Fatal("pin outlived its approval's expiry")
	}
}

func TestPendingSourceMetadataIsBoundedAndInvalidSourcesFailClosed(t *testing.T) {
	r, _, _, _, clock := approvalReceiver(t)
	r.mu.Lock()
	for i := range maxApprovalSources {
		r.recordPendingLocked(trust.Fingerprint([]byte(fmt.Sprint(i))), fmt.Sprintf("10.0.%d.1", i), "")
	}
	r.recordPendingLocked(trust.Fingerprint([]byte("over capacity")), "10.1.0.1", "")
	r.recordPendingLocked(trust.Fingerprint([]byte("invalid source")), "not-an-ip", "")
	if len(r.approvalSources) != maxApprovalSources || len(r.pending) > maxPendingSenders {
		t.Error("source metadata or approval queue exceeded its bound")
	}
	r.mu.Unlock()
	clock.now = clock.now.Add(approvalLifetime)
	r.PendingSenders()
	if len(r.approvalSources) != 0 {
		t.Fatal("expired source metadata was retained")
	}
}

func TestConcurrentApprovalAndResetCannotRetainStaleTrust(t *testing.T) {
	r, _, _, store, _ := approvalReceiver(t)
	fingerprint := requestApproval(t, r, "192.168.1.20", senderIdentity(t))
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := r.ApproveSender(fingerprint); err != nil && !errors.Is(err, ErrNotPending) {
			t.Error(err)
		}
	})
	wg.Go(func() {
		if err := r.ClearTrustedSenders(); err != nil {
			t.Error(err)
		}
	})
	for range 8 {
		wg.Go(func() { _ = r.PendingSenders() })
	}
	wg.Wait()
	if store.IsTrustedSender(fingerprint) || len(r.PendingSenders()) != 0 {
		t.Fatal("approval/reset race retained a stale request or trust")
	}
}

// A flood from many sources must not prevent a new (owner) source from asking
// for approval: the oldest source budget is evicted instead.
func TestManySourcesDoNotBlockNewSource(t *testing.T) {
	r, _, _, _, clock := approvalReceiver(t)
	for i := 0; i < maxApprovalSources; i++ {
		remote := fmt.Sprintf("10.%d.%d.1", (i/256)%256, i%256)
		for j := 0; j < maxPendingPerSource; j++ {
			requestApproval(t, r, remote, senderIdentity(t))
		}
		clock.now = clock.now.Add(time.Millisecond)
	}
	identity := senderIdentity(t)
	fingerprint := requestApproval(t, r, "192.168.50.10", identity)
	for _, pending := range r.PendingSenders() {
		if pending.Fingerprint == fingerprint {
			return
		}
	}
	t.Fatal("a new source was refused after a multi-source flood")
}
