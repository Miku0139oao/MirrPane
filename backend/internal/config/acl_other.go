//go:build !windows

package config

// RestrictFileACL applies platform-specific owner-only access controls to path.
// On non-Windows platforms, file permissions are enforced via POSIX 0600 mode.
func RestrictFileACL(string) error { return nil }
