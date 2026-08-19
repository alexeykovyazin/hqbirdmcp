//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/aleks/fbmcp/internal/gate"
	"github.com/aleks/fbmcp/internal/state"
)

// Custom TaskDialog button ids — well clear of the standard IDOK..IDCONTINUE
// range (1-11) so they can't be confused with a default button.
const (
	approveButtonID int32 = 1001
	denyButtonID    int32 = 1002
)

// dialogWorker shows one Approve/Deny dialog at a time (TaskDialogIndirect
// blocks until dismissed, which is what naturally serializes them — two
// pending actions never stack as overlapping windows). Approve/Deny write
// the same out-of-band marker files the fbmcp-approve / fbmcpctl approve
// CLIs already write; the running server's approval/denial watchers
// (cmd/fbmcp/p3tools.go) pick them up within 2s.
func dialogWorker(stateDir string, queue <-chan state.PendingAction) {
	for p := range queue {
		showOneDialog(stateDir, p)
	}
}

// showOneDialog isolates one TaskDialogIndirect call behind a recover: a
// failure showing one action's dialog must never take down the whole tray
// process (it did exactly that once — an unrecovered panic in this
// goroutine is fatal to the entire program — silently leaving every later
// pending action with no operator-visible confirmation at all).
func showOneDialog(stateDir string, p state.PendingAction) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintln(os.Stderr, "fbmcp-tray: dialog panic (action left pending):", r)
		}
	}()

	title := "fbmcp — confirm action"
	main := fmt.Sprintf("%s on %s (Tier %d)", p.Tool, p.Database, p.Tier)
	content := gate.ImpactStatement(p)

	pressed, err := approveDenyDialog(title, main, content)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fbmcp-tray: dialog:", err)
		return
	}
	switch pressed {
	case approveButtonID:
		writeMarker(stateDir, "approvals", p.ID, "approved-by-operator\n")
	case denyButtonID:
		writeMarker(stateDir, "denials", p.ID, "denied-by-operator\n")
	default:
		// Esc/close (idCancel) — leave the action pending; it will be
		// re-queued only if it's still pending on a later poll tick
		// is not attempted here to avoid nagging. Operator can act via
		// the CLI, or it expires on its own 15-minute TTL.
	}
}

func writeMarker(stateDir, subdir, id, body string) {
	dir := filepath.Join(stateDir, subdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "fbmcp-tray: marker dir:", err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, id), []byte(body), 0o640); err != nil {
		fmt.Fprintln(os.Stderr, "fbmcp-tray: write marker:", err)
	}
}
