//go:build windows

package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// instanceMutexName is per logon session: two Windows users may each run one
// receiver, but a second launch in the same session focuses the first.
const instanceMutexName = `Local\MirrPane.UI`

const windowTitle = "鏡屏 MirrPane"

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	procFindWindowW         = user32.NewProc("FindWindowW")
	procShowWindow          = user32.NewProc("ShowWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
)

// acquireSingleInstance takes the per-session instance mutex and keeps it for
// the process lifetime. When another instance already owns it, that instance's
// window is restored and focused and ok reports false so this launch exits.
func acquireSingleInstance() (func(), bool) {
	handle, ok := acquireInstanceMutex(instanceMutexName, true)
	if !ok {
		return func() {}, false
	}
	return func() {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
	}, true
}

// acquireInstanceMutex creates the named mutex. focusExisting restores the
// existing window when the mutex is already held; tests pass false.
func acquireInstanceMutex(name string, focusExisting bool) (windows.Handle, bool) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, true
	}
	handle, err := windows.CreateMutex(nil, false, namePtr)
	if err == nil {
		return handle, true
	}
	if handle != 0 && focusExisting {
		focusExistingWindow()
	}
	if handle != 0 {
		_ = windows.CloseHandle(handle)
	}
	if err == windows.ERROR_ALREADY_EXISTS {
		return 0, false
	}
	// Any other mutex failure must not keep the receiver from running.
	return 0, true
}

// focusExistingWindow brings the already-running instance to the foreground.
func focusExistingWindow() {
	title, err := windows.UTF16PtrFromString(windowTitle)
	if err != nil {
		return
	}
	hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(title)))
	if hwnd == 0 {
		return
	}
	// SW_RESTORE un-minimises; SetForegroundWindow may be refused while
	// another process owns the foreground, which is harmless: the window is
	// at least visible and in the taskbar.
	procShowWindow.Call(hwnd, windows.SW_RESTORE)
	procSetForegroundWindow.Call(hwnd)
}
