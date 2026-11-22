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

func assertProtectedUserAndSystemDACL(t *testing.T, path string) {
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

	wantACEs := []string{
		"(A;;FA;;;" + userSID + ")",
		"(A;;FA;;;SY)",
	}
	if userSID == localSystemSID {
		wantACEs = []string{"(A;;FA;;;SY)"}
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

func TestConfigSaveAndLoadApplyProtectedWindowsDACL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)

	// Fresh save creates config.json with protected user + SYSTEM DACL.
	if _, err := Load(dir, "鏡屏"); err != nil {
		t.Fatalf("Load fresh: %v", err)
	}
	assertProtectedUserAndSystemDACL(t, path)

	// Legacy file with world-readable ACE is repaired on Load.
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
