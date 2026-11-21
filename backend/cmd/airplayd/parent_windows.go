//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// hasConsole reports whether stderr is a real console a human can read.
//
// The UI starts this process with piped stdio. Nothing drains that pipe, so once
// its buffer fills, every write blocks — and because the write happens while the
// logger holds its mutex, the whole process stops: no accepts, no replies, the
// sender's handshake hangs mid-flight. So logging only goes to the console when
// there is an actual console.
func hasConsole() bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Stderr, &mode) == nil
}

// watchParent exits this process when the one that started it goes away.
//
// The UI starts the sidecar as a child process. Windows does not reap children,
// so a window that is force-closed would otherwise leave a receiver holding the
// AirPlay port and still accepting connections — with no window to show for it.
func watchParent(pid int) {
	if pid <= 0 {
		return
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return
	}
	go func() {
		defer windows.CloseHandle(handle)
		if _, err := windows.WaitForSingleObject(handle, windows.INFINITE); err != nil {
			return
		}
		note("parent process exited; stopping")
		os.Exit(0)
	}()
}
