//go:build windows

package trust

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func assertTrustProtectedDACL(t *testing.T, path string) {
	t.Helper()

	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Skipf("token user unavailable: %v", err)
	}
	userSID := user.User.Sid.String()

	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		if errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED) {
			t.Skipf("security descriptor API unavailable on filesystem: %v", err)
		}
		t.Fatalf("GetNamedSecurityInfo(%s): %v", path, err)
	}

	ctrl, _, err := sd.Control()
	if err != nil {
		t.Fatalf("sd.Control: %v", err)
	}
	if ctrl&windows.SE_DACL_PRESENT == 0 || ctrl&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("DACL not present/protected (control=%#x, sddl=%s)", ctrl, sd.String())
	}

	dacl, defaulted, err := sd.DACL()
	if err != nil {
		t.Fatalf("sd.DACL: %v", err)
	}
	if defaulted || dacl == nil {
		t.Fatalf("unexpected DACL state: defaulted=%v dacl=%v", defaulted, dacl)
	}

	wantACEs := []string{
		"(A;;FA;;;" + userSID + ")",
		"(A;;FA;;;SY)",
	}
	if userSID == "S-1-5-18" {
		wantACEs = []string{"(A;;FA;;;SY)"}
	}
	if int(dacl.AceCount) != len(wantACEs) {
		t.Fatalf("AceCount = %d, want %d (sddl=%s)", dacl.AceCount, len(wantACEs), sd.String())
	}

	sddl := sd.String()
	if !strings.HasPrefix(sddl, "D:P") {
		t.Fatalf("SDDL = %q, want protected prefix D:P", sddl)
	}
	for _, ace := range wantACEs {
		if !strings.Contains(sddl, ace) {
			t.Fatalf("SDDL %q missing expected ACE %s", sddl, ace)
		}
	}
}

func TestTrustStoreAppliesProtectedWindowsDACL(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	sender := Fingerprint([]byte("sender-1"))
	if err := s.TrustSender(sender); err != nil {
		t.Fatalf("TrustSender = %v", err)
	}

	path := filepath.Join(dir, FileName)
	assertTrustProtectedDACL(t, path)

	// Legacy world-accessible trust file is repaired on Open without rotating trust.
	permissiveSD, err := windows.SecurityDescriptorFromString("D:(A;;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	permissiveDACL, _, err := permissiveSD.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		permissiveDACL,
		nil,
	); err != nil {
		if errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED) {
			t.Skipf("SetNamedSecurityInfo unavailable: %v", err)
		}
		t.Fatalf("seed permissive DACL: %v", err)
	}

	reloaded, err := Open(dir, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatalf("Open existing: %v", err)
	}
	if !reloaded.IsTrustedSender(sender) {
		t.Fatal("Open lost existing trusted sender")
	}
	assertTrustProtectedDACL(t, path)
}
