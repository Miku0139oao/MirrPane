//go:build windows

package config

import (
	"errors"
	"fmt"
	"log"
	"sync"

	"golang.org/x/sys/windows"
)

const localSystemSID = "S-1-5-18"

var (
	aclWarnOnce          sync.Once
	setNamedSecurityInfo = windows.SetNamedSecurityInfo
)

// RestrictFileACL replaces path's DACL with a protected DACL granting full
// access only to the current process user and SYSTEM, blocking inherited read
// permissions from parent directories. On filesystems that do not support ACLs
// (FAT32, exFAT, some network shares), it logs a warning once and returns nil
// so portable usage continues with 0600 mode.
func RestrictFileACL(path string) error {
	return restrictACL(path, "")
}

// RestrictDirACL replaces dir's DACL with a protected, inheritable (OICI) DACL
// granting full access only to the current process user and SYSTEM so files
// created inside dir inherit owner-only permissions immediately. On
// filesystems that do not support ACLs (FAT32, exFAT, some network shares), it
// logs a warning once and returns nil.
func RestrictDirACL(dir string) error {
	return restrictACL(dir, "OICI")
}

func restrictACL(path, aceFlags string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("token user: %w", err)
	}
	userSID := user.User.Sid.String()
	sddl := fmt.Sprintf("D:P(A;%s;FA;;;%s)(A;%s;FA;;;SY)", aceFlags, userSID, aceFlags)
	if userSID == localSystemSID {
		sddl = fmt.Sprintf("D:P(A;%s;FA;;;SY)", aceFlags)
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return fmt.Errorf("parse SDDL: %w", err)
	}
	dacl, err := extractProtectedDACL(sd)
	if err != nil {
		return err
	}
	err = setNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	)
	return classifyACLError(err)
}

func extractProtectedDACL(sd *windows.SECURITY_DESCRIPTOR) (*windows.ACL, error) {
	if sd == nil {
		return nil, errors.New("extract DACL: nil security descriptor")
	}
	dacl, defaulted, err := sd.DACL()
	if err != nil {
		return nil, fmt.Errorf("extract DACL: %w", err)
	}
	if dacl == nil || defaulted || dacl.AceCount == 0 {
		return nil, fmt.Errorf("extract DACL: invalid protected DACL (nil=%v, defaulted=%v)", dacl == nil, defaulted)
	}
	return dacl, nil
}

func classifyACLError(err error) error {
	if err == nil {
		return nil
	}
	if isACLUnsupportedError(err) {
		aclWarnOnce.Do(func() {
			log.Printf("config: filesystem does not support Windows ACLs (%v); continuing without DACL protection", err)
		})
		return nil
	}
	return fmt.Errorf("SetNamedSecurityInfo: %w", err)
}

func isACLUnsupportedError(err error) bool {
	return errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
		errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED)
}
