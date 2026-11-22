package trust

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestExplicitApprovalsPersistAndUnknownSendersRemainUntrusted(t *testing.T) {
	s, dir := testStore(t)
	a, b := Fingerprint([]byte("sender A")), Fingerprint([]byte("sender B"))
	if s.IsTrustedSender(a) || s.IsTrustedSender(b) {
		t.Fatal("empty store implicitly trusted a sender")
	}
	if err := s.TrustSender(a); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := Open(dir, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.IsTrustedSender(a) || reloaded.IsTrustedSender(b) {
		t.Fatal("restart did not retain the exact allowlist")
	}
	if err := reloaded.TrustSender(a); err != nil {
		t.Fatal(err)
	}
	if reloaded.IsTrustedSender(b) {
		t.Fatal("unknown sender was implicitly approved")
	}
	after, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("unknown sender modified the persisted allowlist")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, FileName))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("trust file permissions: %v, %v", info, err)
		}
	}
	if err := reloaded.TrustSender(b); err != nil {
		t.Fatal(err)
	}
	if !reloaded.IsTrustedSender(b) {
		t.Fatal("explicit owner enrollment failed")
	}
}

func TestClearTrustAndReceiverIdentityReset(t *testing.T) {
	s, dir := testStore(t)
	old := Fingerprint([]byte("old sender"))
	if err := s.TrustSender(old); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearTrustedSenders(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Open(dir, bytes.Repeat([]byte{1}, 32))
	if err != nil || reloaded.IsTrustedSender(old) {
		t.Fatalf("clear did not survive restart: %v", err)
	}
	if err := reloaded.TrustSender(old); err != nil {
		t.Fatal(err)
	}
	newIdentity := bytes.Repeat([]byte{2}, 32)
	reset, err := Open(dir, newIdentity)
	if err != nil || reset.IsTrustedSender(old) {
		t.Fatalf("receiver identity reset retained old trust: %v", err)
	}
	reset, err = Open(dir, newIdentity)
	if err != nil || reset.IsTrustedSender(old) {
		t.Fatalf("identity reset was not persisted: %v", err)
	}
}

func TestInvalidTrustFilesFailClosed(t *testing.T) {
	for _, body := range []string{"{", "null", "{}", `{"receiver":"bad"}`,
		`{"receiver":"` + Fingerprint([]byte("receiver")) + `","trustedSenders":["bad"]}`} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, bytes.Repeat([]byte{1}, 32)); err == nil {
			t.Fatalf("invalid trust file reopened enrollment: %q", body)
		}
	}
	s, _ := testStore(t)
	if err := s.TrustSender("invalid"); err == nil || s.IsTrustedSender("invalid") {
		t.Fatal("malformed fingerprint was enrolled")
	}
}

func TestSaveFailureDoesNotChangeTrustAndCleansTemporaryFile(t *testing.T) {
	s, dir := testStore(t)
	a, b := Fingerprint([]byte("sender A")), Fingerprint([]byte("sender B"))
	path := s.path
	blocked := filepath.Join(dir, "directory")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	s.path = blocked // A rename cannot replace a directory with a file.
	if err := s.TrustSender(a); err == nil || s.IsTrustedSender(a) {
		t.Fatal("failed first enrollment changed the policy")
	}
	s.path = path
	if err := s.TrustSender(b); err != nil {
		t.Fatal(err)
	}
	s.path = blocked
	if err := s.ClearTrustedSenders(); err == nil || !s.IsTrustedSender(b) {
		t.Fatal("failed clear changed the policy")
	}
	if err := s.TrustSender(a); err == nil || s.IsTrustedSender(a) {
		t.Fatal("failed explicit enrollment changed the policy")
	}
	temps, err := filepath.Glob(filepath.Join(dir, ".trusted-senders-*"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("failed save left temporary files: %v, %v", temps, err)
	}
}

func TestConcurrentExplicitApprovalsPersistEveryApprovedIdentity(t *testing.T) {
	s, dir := testStore(t)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			if err := s.TrustSender(Fingerprint([]byte{byte(i)})); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	reloaded, err := Open(dir, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 32 {
		if !reloaded.IsTrustedSender(Fingerprint([]byte{byte(i)})) {
			t.Fatalf("concurrent approval %d was lost", i)
		}
	}
}
