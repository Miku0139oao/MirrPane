//go:build windows

package trust

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func assertTrustProtectedDACL(t *testing.T, path, aceFlags string) {
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

	// Windows may render the SID as an SDDL alias (for example "LA" for the
	// built-in Administrator), so compare the rendering Windows produces.
	renderACE := func(ace string) string {
		sd, err := windows.SecurityDescriptorFromString("D:P" + ace)
		if err != nil {
			t.Fatalf("render ACE %s: %v", ace, err)
		}
		return strings.TrimPrefix(sd.String(), "D:P")
	}
	wantACEs := []string{
		renderACE(fmt.Sprintf("(A;%s;FA;;;%s)", aceFlags, userSID)),
		renderACE(fmt.Sprintf("(A;%s;FA;;;SY)", aceFlags)),
	}
	if userSID == "S-1-5-18" {
		wantACEs = []string{renderACE(fmt.Sprintf("(A;%s;FA;;;SY)", aceFlags))}
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

func seedPermissiveACL(t *testing.T, path, sddl string) {
	t.Helper()
	permissiveSD, err := windows.SecurityDescriptorFromString(sddl)
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
		t.Fatalf("seed permissive DACL on %s: %v", path, err)
	}
}

func TestTrustStoreAppliesProtectedWindowsDACL(t *testing.T) {
	dir := t.TempDir()
	seedPermissiveACL(t, dir, "D:(A;OICI;FA;;;WD)")

	// Open repairs an existing permissive directory even before trusted_senders.json exists.
	s, err := Open(dir, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	assertTrustProtectedDACL(t, dir, "OICI")

	// TrustSender creates trusted_senders.json and protects both a newly permissive dir and the file.
	subDir := filepath.Join(t.TempDir(), "data")
	s2, err := Open(subDir, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	sender := Fingerprint([]byte("sender-1"))
	if err := s2.TrustSender(sender); err != nil {
		t.Fatalf("TrustSender = %v", err)
	}
	assertTrustProtectedDACL(t, subDir, "OICI")
	assertTrustProtectedDACL(t, filepath.Join(subDir, FileName), "")

	// Also verify TrustSender repairs a permissive directory when saving.
	if err := s.TrustSender(sender); err != nil {
		t.Fatalf("TrustSender = %v", err)
	}
	path := filepath.Join(dir, FileName)
	assertTrustProtectedDACL(t, dir, "OICI")
	assertTrustProtectedDACL(t, path, "")

	// Newly created temp files in dir inherit only user + SYSTEM ACEs before RestrictFileACL runs.
	tmp, err := os.CreateTemp(dir, "inherit-check-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	t.Cleanup(func() { _ = os.Remove(tmpName) })
	sd, err := windows.GetNamedSecurityInfo(tmpName, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%s): %v", tmpName, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Skipf("token user unavailable: %v", err)
	}
	wantCount := 2
	if user.User.Sid.String() == "S-1-5-18" {
		wantCount = 1
	}
	if int(dacl.AceCount) != wantCount {
		t.Fatalf("inherited AceCount = %d, want %d (sddl=%s)", dacl.AceCount, wantCount, sd.String())
	}

	// Legacy world-accessible trust file and directory are repaired on Open without rotating trust.
	seedPermissiveACL(t, dir, "D:(A;OICI;FA;;;WD)")
	seedPermissiveACL(t, path, "D:(A;;FA;;;WD)")

	reloaded, err := Open(dir, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatalf("Open existing: %v", err)
	}
	if !reloaded.IsTrustedSender(sender) {
		t.Fatal("Open lost existing trusted sender")
	}
	assertTrustProtectedDACL(t, dir, "OICI")
	assertTrustProtectedDACL(t, path, "")
}
