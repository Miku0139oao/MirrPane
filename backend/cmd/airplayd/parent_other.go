//go:build !windows

package main

// watchParent is a no-op where the UI does not run the sidecar as a child.
func watchParent(pid int) {}

// hasConsole reports whether stderr is a terminal.
func hasConsole() bool { return true }
