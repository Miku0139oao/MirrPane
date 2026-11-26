//go:build windows

package main

import (
	"fmt"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestAcquireInstanceMutexRejectsSecondHolder(t *testing.T) {
	name := fmt.Sprintf(`Local\MirrPane.Test.%d.%d`, windows.GetCurrentProcessId(), time.Now().UnixNano())

	first, ok := acquireInstanceMutex(name, false)
	if !ok || first == 0 {
		t.Fatalf("first acquire = (%v, %v), want a handle", first, ok)
	}
	defer windows.CloseHandle(first)

	if second, ok := acquireInstanceMutex(name, false); ok || second != 0 {
		windows.CloseHandle(second)
		t.Fatalf("second acquire = (%v, %v), want rejected", second, ok)
	}

	other := name + ".other"
	third, ok := acquireInstanceMutex(other, false)
	if !ok || third == 0 {
		t.Fatalf("different name acquire = (%v, %v), want a handle", third, ok)
	}
	windows.CloseHandle(third)

	// Releasing the first holder lets a later launch in.
	if err := windows.CloseHandle(first); err != nil {
		t.Fatalf("close first: %v", err)
	}
	first = 0
	again, ok := acquireInstanceMutex(name, false)
	if !ok || again == 0 {
		t.Fatalf("acquire after release = (%v, %v), want a handle", again, ok)
	}
	windows.CloseHandle(again)
}
