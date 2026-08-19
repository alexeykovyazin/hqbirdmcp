//go:build windows

package main

import "testing"

// TestManualDialogSmoke is not part of CI — it pops a real, visible
// TaskDialog and blocks until a human clicks a button, to verify end-to-end
// that the popup a real Tier-2 pending action would trigger actually
// renders (the earlier bug crashed before ever reaching this point).
// Run explicitly: go test ./cmd/fbmcp-tray/... -run TestManualDialogSmoke -v
func TestManualDialogSmoke(t *testing.T) {
	pressed, err := approveDenyDialog(
		"fbmcp-tray — manual smoke test",
		"This is a live test popup, not a real database action",
		"Click either button. Nothing in fbmcp is affected either way — this call never touches the gate, state store, or approvals/denials markers.",
	)
	if err != nil {
		t.Fatalf("dialog error: %v", err)
	}
	switch pressed {
	case approveButtonID:
		t.Log("you clicked APPROVE (button id 1001) — popup + custom buttons work")
	case denyButtonID:
		t.Log("you clicked DENY (button id 1002) — popup + custom buttons work")
	default:
		t.Logf("dialog dismissed without a button (id=%d) — popup rendered but wasn't answered", pressed)
	}
}
