//go:build windows

package config

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/windows"
)

func assertProtectedDACLWithFlags(t *testing.T, path, aceFlags string) {
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
	if ctrl&windows.SE_DACL_PRESENT == 0 {
		t.Fatalf("DACL not present (control=%#x)", ctrl)
	}
	if ctrl&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("DACL is not protected from inheritance (control=%#x, sddl=%s)", ctrl, sd.String())
	}

	dacl, defaulted, err := sd.DACL()
	if err != nil {
		t.Fatalf("sd.DACL: %v", err)
	}
	if defaulted || dacl == nil {
		t.Fatalf("unexpected DACL state: defaulted=%v dacl=%v", defaulted, dacl)
	}

	// Windows renders some SIDs as SDDL aliases (for example the built-in
	// Administrator as "LA"), so compare against the rendering Windows itself
	// produces for the expected SID rather than the numeric string.
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
	if userSID == localSystemSID {
		wantACEs = []string{renderACE(fmt.Sprintf("(A;%s;FA;;;SY)", aceFlags))}
	}
	if int(dacl.AceCount) != len(wantACEs) {
		t.Fatalf("AceCount = %d, want %d (sddl=%s)", dacl.AceCount, len(wantACEs), sd.String())
	}

	sddl := sd.String()
	if !strings.HasPrefix(sddl, "D:P") {
		t.Fatalf("SDDL = %q, want protected DACL prefix D:P", sddl)
	}
	for _, ace := range wantACEs {
		if !strings.Contains(sddl, ace) {
			t.Fatalf("SDDL %q missing expected ACE %s", sddl, ace)
		}
	}
}

func assertProtectedUserAndSystemDACL(t *testing.T, path string) {
	t.Helper()
	assertProtectedDACLWithFlags(t, path, "")
}

func assertProtectedDirDACL(t *testing.T, dir string) {
	t.Helper()
	assertProtectedDACLWithFlags(t, dir, "OICI")
}

func assertInheritedOnlyUserAndSystemDACL(t *testing.T, filePath string) {
	t.Helper()

	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Skipf("token user unavailable: %v", err)
	}
	userSID := user.User.Sid.String()

	sd, err := windows.GetNamedSecurityInfo(filePath, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		if errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED) {
			t.Skipf("security descriptor API unavailable on filesystem: %v", err)
		}
		t.Fatalf("GetNamedSecurityInfo(%s): %v", filePath, err)
	}

	dacl, defaulted, err := sd.DACL()
	if err != nil {
		t.Fatalf("sd.DACL: %v", err)
	}
	if defaulted || dacl == nil {
		t.Fatalf("unexpected DACL state: defaulted=%v dacl=%v", defaulted, dacl)
	}

	renderInheritedACE := func(sid string) string {
		sd, err := windows.SecurityDescriptorFromString("D:(A;ID;FA;;;" + sid + ")")
		if err != nil {
			t.Fatalf("render inherited ACE for %s: %v", sid, err)
		}
		return strings.TrimPrefix(sd.String(), "D:")
	}
	wantACEs := []string{
		renderInheritedACE(userSID),
		renderInheritedACE("SY"),
	}
	if userSID == localSystemSID {
		wantACEs = []string{renderInheritedACE("SY")}
	}
	if int(dacl.AceCount) != len(wantACEs) {
		t.Fatalf("AceCount = %d, want %d (sddl=%s)", dacl.AceCount, len(wantACEs), sd.String())
	}
	sddl := sd.String()
	for _, ace := range wantACEs {
		if !strings.Contains(sddl, ace) {
			t.Fatalf("inherited SDDL %q missing expected ACE %s", sddl, ace)
		}
	}
}

func TestConfigSaveAndLoadApplyProtectedWindowsDACL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	path := filepath.Join(dir, FileName)

	// Fresh save creates dir and config.json with protected user + SYSTEM DACLs.
	if _, err := Load(dir, "鏡屏"); err != nil {
		t.Fatalf("Load fresh: %v", err)
	}
	assertProtectedDirDACL(t, dir)
	assertProtectedUserAndSystemDACL(t, path)

	// Newly created files inside dir inherit only user + SYSTEM ACEs before RestrictFileACL runs.
	tmp, err := os.CreateTemp(dir, "inherit-check-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	t.Cleanup(func() { _ = os.Remove(tmpName) })
	assertInheritedOnlyUserAndSystemDACL(t, tmpName)

	// Legacy file and permissive directory with world-readable ACEs are repaired on Load.
	seedPermissiveDirACL(t, dir)
	permissiveSD, err := windows.SecurityDescriptorFromString("D:(A;;FA;;;WD)")
	if err != nil {
		t.Fatalf("SecurityDescriptorFromString: %v", err)
	}
	permissiveDACL, _, err := permissiveSD.DACL()
	if err != nil {
		t.Fatalf("permissiveSD.DACL: %v", err)
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

	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, "鏡屏"); err != nil {
		t.Fatalf("Load existing legacy ACL: %v", err)
	}
	assertProtectedDirDACL(t, dir)
	assertProtectedUserAndSystemDACL(t, path)
}

func TestIsACLUnsupportedErrorAndVolumeFallback(t *testing.T) {
	for _, err := range []error{
		windows.ERROR_NOT_SUPPORTED,
		windows.ERROR_INVALID_FUNCTION,
		windows.ERROR_CALL_NOT_IMPLEMENTED,
		fmt.Errorf("wrapped: %w", windows.ERROR_NOT_SUPPORTED),
	} {
		if !isACLUnsupportedError(err) {
			t.Fatalf("isACLUnsupportedError(%v) = false, want true", err)
		}
	}
	for _, err := range []error{
		nil,
		windows.ERROR_ACCESS_DENIED,
		windows.ERROR_FILE_NOT_FOUND,
		errors.New("other error"),
	} {
		if isACLUnsupportedError(err) {
			t.Fatalf("isACLUnsupportedError(%v) = true, want false", err)
		}
	}

	origSet := setNamedSecurityInfo
	t.Cleanup(func() {
		setNamedSecurityInfo = origSet
		aclWarnOnce = sync.Once{}
	})

	var logs bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	// Unsupported filesystem errors log once and allow Load/Save to succeed.
	aclWarnOnce = sync.Once{}
	setNamedSecurityInfo = func(string, windows.SE_OBJECT_TYPE, windows.SECURITY_INFORMATION, *windows.SID, *windows.SID, *windows.ACL, *windows.ACL) error {
		return windows.ERROR_NOT_SUPPORTED
	}
	dir := t.TempDir()
	if _, err := Load(dir, "鏡屏"); err != nil {
		t.Fatalf("Load on unsupported ACL volume failed: %v", err)
	}
	if _, err := Load(dir, "鏡屏"); err != nil {
		t.Fatalf("second Load on unsupported ACL volume failed: %v", err)
	}
	if count := strings.Count(logs.String(), "does not support Windows ACLs"); count != 1 {
		t.Fatalf("expected 1 warning log, got %d (%q)", count, logs.String())
	}

	// Real permission failures (ERROR_ACCESS_DENIED) must still fail Save.
	setNamedSecurityInfo = func(string, windows.SE_OBJECT_TYPE, windows.SECURITY_INFORMATION, *windows.SID, *windows.SID, *windows.ACL, *windows.ACL) error {
		return windows.ERROR_ACCESS_DENIED
	}
	if _, err := Load(t.TempDir(), "鏡屏"); err == nil || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("expected ERROR_ACCESS_DENIED failure, got %v", err)
	}
}

func TestExtractProtectedDACLRejectsNilOrDefaulted(t *testing.T) {
	if _, err := extractProtectedDACL(nil); err == nil {
		t.Fatal("expected error for nil security descriptor")
	}
	// Missing DACL ("O:SY"), NULL DACL ("D:NO_ACCESS_CONTROL"), and empty DACL ("D:") must all be refused.
	for _, sddl := range []string{"O:SY", "D:NO_ACCESS_CONTROL", "D:"} {
		sd, err := windows.SecurityDescriptorFromString(sddl)
		if err != nil {
			t.Fatalf("SecurityDescriptorFromString(%q): %v", sddl, err)
		}
		if _, err := extractProtectedDACL(sd); err == nil {
			t.Fatalf("expected error for SDDL %q (must never install NULL/empty/missing DACL)", sddl)
		}
	}
	// A defaulted DACL must also be refused.
	validSD, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;SY)")
	if err != nil {
		t.Fatal(err)
	}
	validDACL, _, err := validSD.DACL()
	if err != nil {
		t.Fatal(err)
	}
	absSD, err := validSD.ToAbsolute()
	if err != nil {
		t.Fatal(err)
	}
	if err := absSD.SetDACL(validDACL, true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := extractProtectedDACL(absSD); err == nil {
		t.Fatal("expected error for defaulted DACL")
	}
}
