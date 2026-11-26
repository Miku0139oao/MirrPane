//go:build !windows

package airplayd

// watchParent is a no-op where the UI does not run the sidecar as a child.
func watchParent(pid int) error { return nil }

// hasConsole reports whether stderr is a terminal.
func hasConsole() bool { return true }
