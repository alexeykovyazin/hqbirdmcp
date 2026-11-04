package ctl

import (
	"os"
	"path/filepath"
	"testing"
)

// Lab 2026-11-04: approving an expired request id must fail (exit 1) and
// write no marker; listing an empty queue is still fine (exit 0).
func TestApproveUnknownIDFails(t *testing.T) {
	dir := t.TempDir()
	if rc := cmdApprove([]string{dir}); rc != 0 {
		t.Fatalf("list on an empty queue: rc=%d, want 0", rc)
	}
	if rc := cmdApprove([]string{dir, "8f132fa83adf6743a0855275"}); rc != 1 {
		t.Fatalf("approve unknown id: rc=%d, want 1", rc)
	}
	if _, err := os.Stat(filepath.Join(dir, "approvals", "8f132fa83adf6743a0855275")); !os.IsNotExist(err) {
		t.Fatalf("marker written for an unknown id (err=%v)", err)
	}
}
