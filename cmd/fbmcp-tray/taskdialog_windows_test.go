//go:build windows

package main

import "testing"

// TestTaskDialogIndirectResolves catches the exact failure mode that took
// down the whole tray process in production: comctl32.dll loaded without a
// v6 manifest doesn't export TaskDialogIndirect at all, so LazyProc.Find
// fails. This never opens a window — it only proves the manifest embedded
// via rsrc_windows_amd64.syso actually binds v6 comctl32 for this binary.
func TestTaskDialogIndirectResolves(t *testing.T) {
	if err := procTaskDialogInd.Find(); err != nil {
		t.Fatalf("TaskDialogIndirect not found (missing v6 comctl32 manifest?): %v", err)
	}
}
