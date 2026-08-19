//go:build windows

package main

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TaskDialogIndirect (comctl32.dll) is not wrapped by golang.org/x/sys/windows,
// so this binds it directly. It is available via plain GetProcAddress only
// once the process carries a manifest declaring a dependency on
// Microsoft.Windows.Common-Controls 6.0.0.0 (see tray.manifest, embedded via
// rsrc_windows_amd64.syso) — without it, comctl32.dll resolves to the legacy
// v5.82 copy, which doesn't export this symbol at all.
var (
	comctl32          = windows.NewLazySystemDLL("comctl32.dll")
	procTaskDialogInd = comctl32.NewProc("TaskDialogIndirect")
)

const (
	tdfAllowDialogCancellation = 0x0008
	tdfSizeToContent           = 0x01000000

	tdWarningIcon uintptr = 0xFFFF // MAKEINTRESOURCEW(-1), i.e. WORD(-1)

	idCancel = 2 // standard Windows control id returned on Esc/close
)

// TASKDIALOGCONFIG and TASKDIALOG_BUTTON are declared inside
// `#include <pshpack1.h> ... #include <poppack.h>` in CommCtrl.h — they are
// byte-packed with zero padding, unlike a normal Win32 struct. A Go struct
// with the same field order gets naturally-aligned padding inserted before
// every pointer field (176 bytes instead of the real 160), which
// TaskDialogIndirect rejects outright with E_INVALIDARG — confirmed by
// hand against the Windows SDK header
// (Windows Kits/10/Include/.../um/CommCtrl.h:7624-7770) after every other
// explanation was ruled out. So this is hand-marshaled into a raw byte
// buffer at the literal packed offsets instead of a Go struct.
const (
	tdCfgSize = 160 // total packed size, x64

	offCbSize                  = 0
	offHwndParent              = 4
	offHInstance               = 12
	offDwFlags                 = 20
	offDwCommonButtons         = 24
	offPszWindowTitle          = 28
	offPszMainIcon             = 36
	offPszMainInstruction      = 44
	offPszContent              = 52
	offCButtons                = 60
	offPButtons                = 64
	offNDefaultButton          = 72
	offCRadioButtons           = 76
	offPRadioButtons           = 80
	offNDefaultRadioButton     = 88
	offPszVerificationText     = 92
	offPszExpandedInformation  = 100
	offPszExpandedControlText  = 108
	offPszCollapsedControlText = 116
	offPszFooterIcon           = 124
	offPszFooter               = 132
	offPfCallback              = 140
	offLpCallbackData          = 148
	offCxWidth                 = 156

	tdButtonSize = 12 // int(4) + PCWSTR(8), packed
)

func putPtr(buf []byte, off int, p unsafe.Pointer) {
	binary.LittleEndian.PutUint64(buf[off:], uint64(uintptr(p)))
}

func putRaw(buf []byte, off int, v uintptr) {
	binary.LittleEndian.PutUint64(buf[off:], uint64(v))
}

func putU32(buf []byte, off int, v uint32) {
	binary.LittleEndian.PutUint32(buf[off:], v)
}

func putI32(buf []byte, off int, v int32) {
	binary.LittleEndian.PutUint32(buf[off:], uint32(v))
}

// approveDenyDialog shows a modal TaskDialog with exactly two custom
// buttons ("Approve"/"Deny") — no default Windows Yes/No/Cancel. Blocks
// until dismissed. Returns idCancel if closed without a button (Esc/X),
// or one of approveButtonID/denyButtonID.
func approveDenyDialog(title, mainInstruction, content string) (int32, error) {
	// LazyProc.Call panics if the procedure can't be resolved (e.g. no v6
	// comctl32 manifest bound) — Find surfaces that as a plain error instead,
	// so a resolution failure degrades to "dialog skipped" rather than
	// silently killing the whole tray process (it did exactly that once).
	if err := procTaskDialogInd.Find(); err != nil {
		return 0, fmt.Errorf("TaskDialogIndirect unavailable: %w", err)
	}

	titleW, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return 0, err
	}
	mainW, err := windows.UTF16PtrFromString(mainInstruction)
	if err != nil {
		return 0, err
	}
	contentW, err := windows.UTF16PtrFromString(content)
	if err != nil {
		return 0, err
	}
	approveText, err := windows.UTF16PtrFromString("Approve")
	if err != nil {
		return 0, err
	}
	denyText, err := windows.UTF16PtrFromString("Deny")
	if err != nil {
		return 0, err
	}

	buttons := make([]byte, tdButtonSize*2)
	putI32(buttons, 0*tdButtonSize+0, approveButtonID)
	putPtr(buttons, 0*tdButtonSize+4, unsafe.Pointer(approveText))
	putI32(buttons, 1*tdButtonSize+0, denyButtonID)
	putPtr(buttons, 1*tdButtonSize+4, unsafe.Pointer(denyText))

	cfg := make([]byte, tdCfgSize)
	putU32(cfg, offCbSize, tdCfgSize)
	putU32(cfg, offDwFlags, tdfAllowDialogCancellation|tdfSizeToContent)
	putPtr(cfg, offPszWindowTitle, unsafe.Pointer(titleW))
	putRaw(cfg, offPszMainIcon, tdWarningIcon)
	putPtr(cfg, offPszMainInstruction, unsafe.Pointer(mainW))
	putPtr(cfg, offPszContent, unsafe.Pointer(contentW))
	putU32(cfg, offCButtons, uint32(len(buttons)/tdButtonSize))
	putPtr(cfg, offPButtons, unsafe.Pointer(&buttons[0]))
	putI32(cfg, offNDefaultButton, denyButtonID) // Esc/Enter-safe default is the non-destructive choice

	// None of these buffers/pointers are referenced through a real Go
	// pointer type from cfg's perspective (everything is raw bytes), so the
	// GC does not see them as reachable from cfg — pin them explicitly
	// until after the call returns.
	defer runtime.KeepAlive(buttons)
	defer runtime.KeepAlive(titleW)
	defer runtime.KeepAlive(mainW)
	defer runtime.KeepAlive(contentW)
	defer runtime.KeepAlive(approveText)
	defer runtime.KeepAlive(denyText)

	var pressed int32
	hr, _, callErr := procTaskDialogInd.Call(
		uintptr(unsafe.Pointer(&cfg[0])),
		uintptr(unsafe.Pointer(&pressed)),
		0, // pnRadioButton
		0, // pfVerificationFlagChecked
	)
	if hr != 0 { // HRESULT != S_OK
		if callErr != nil && callErr != windows.ERROR_SUCCESS {
			return 0, fmt.Errorf("TaskDialogIndirect: hr=0x%x: %w", hr, callErr)
		}
		return 0, fmt.Errorf("TaskDialogIndirect: hr=0x%x", hr)
	}
	return pressed, nil
}
