//go:build windows

package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32         = windows.NewLazySystemDLL("user32.dll")
	procMessageBox = user32.NewProc("MessageBoxW")

	// held for process lifetime; the OS releases it on exit
	singleInstanceMutex windows.Handle
)

const mbIconError = 0x00000010

// msgBox shows a blocking error dialog — the GUI-visible channel for
// startup failures where stderr goes nowhere (Startup-shortcut launch).
func msgBox(title, text string) {
	t, err1 := windows.UTF16PtrFromString(title)
	x, err2 := windows.UTF16PtrFromString(text)
	if err1 != nil || err2 != nil {
		return
	}
	_, _, _ = procMessageBox.Call(0, uintptr(unsafe.Pointer(x)), uintptr(unsafe.Pointer(t)), mbIconError)
}

// acquireSingleInstance takes a session-local named mutex; false means
// another fbmcp-tray already holds it (WS2.6f).
func acquireSingleInstance() bool {
	name, err := windows.UTF16PtrFromString(`Local\fbmcp-tray-single-instance`)
	if err != nil {
		return true // fail open: a broken guard must not block the only confirmation surface
	}
	h, err := windows.CreateMutex(nil, false, name)
	if err == windows.ERROR_ALREADY_EXISTS {
		if h != 0 {
			windows.CloseHandle(h)
		}
		return false
	}
	singleInstanceMutex = h
	return true
}
