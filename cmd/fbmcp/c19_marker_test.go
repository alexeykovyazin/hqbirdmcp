package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/policy"
)

// C19: unknown marker dropped; consumed marker cannot double-dispatch;
// rewriting the same id after consume does not dispatch again.
func TestC19UnknownMarkerDropped(t *testing.T) {
	gt := newTestGT(t)
	dir := filepath.Join(gt.live().State.Dir, "approvals")
	_ = os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "not-a-request"), []byte("x\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if n := gt.consumeApprovalMarkers(); n != 0 {
		t.Fatalf("unknown dispatched %d", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "not-a-request")); !os.IsNotExist(err) {
		t.Fatal("unknown marker not dropped")
	}
}

func TestC19ConsumeOnce(t *testing.T) {
	gt := newTestGT(t)
	spy := &spyExec{}
	meta := policy.ToolMeta{Name: "fb_backup_start", Tier: 1, Scope: "database"}
	gt.execs[meta.Name] = spy.wrap()
	out := gt.requestGated(t.Context(), meta, "backup %s", "spike5", nil, "", nil)
	if len(gt.st.Pending()) != 1 {
		t.Fatalf("pending=%d out=%s", len(gt.st.Pending()), out)
	}
	id := gt.st.Pending()[0].ID
	dir := filepath.Join(gt.live().State.Dir, "approvals")
	_ = os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, id), []byte("approved-by-operator\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if n := gt.consumeApprovalMarkers(); n != 1 {
		t.Fatalf("first consume n=%d", n)
	}
	deadline := time.Now().Add(2 * time.Second)
	for spy.n.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if spy.n.Load() != 1 {
		t.Fatalf("expected one dispatch, got %d", spy.n.Load())
	}
	if err := os.WriteFile(filepath.Join(dir, id), []byte("approved-again\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if n := gt.consumeApprovalMarkers(); n != 0 {
		t.Fatalf("replay dispatched n=%d", n)
	}
	time.Sleep(50 * time.Millisecond)
	if spy.n.Load() != 1 {
		t.Fatalf("double dispatch: %d", spy.n.Load())
	}
}

// Deny: symmetric with C19 approve — unknown marker dropped; denying a
// pending action removes it immediately (no dispatch, no TTL wait); a
// repeat marker for an already-resolved id is a no-op.
func TestDenyUnknownMarkerDropped(t *testing.T) {
	gt := newTestGT(t)
	dir := filepath.Join(gt.live().State.Dir, "denials")
	_ = os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "not-a-request"), []byte("x\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if n := gt.consumeDenialMarkers(); n != 0 {
		t.Fatalf("unknown denied %d", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "not-a-request")); !os.IsNotExist(err) {
		t.Fatal("unknown marker not dropped")
	}
}

func TestDenyConsumeOnce(t *testing.T) {
	gt := newTestGT(t)
	spy := &spyExec{}
	meta := policy.ToolMeta{Name: "fb_backup_start", Tier: 1, Scope: "database"}
	gt.execs[meta.Name] = spy.wrap()
	out := gt.requestGated(t.Context(), meta, "backup %s", "spike5", nil, "", nil)
	if len(gt.st.Pending()) != 1 {
		t.Fatalf("pending=%d out=%s", len(gt.st.Pending()), out)
	}
	id := gt.st.Pending()[0].ID
	dir := filepath.Join(gt.live().State.Dir, "denials")
	_ = os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, id), []byte("denied-by-operator\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if n := gt.consumeDenialMarkers(); n != 1 {
		t.Fatalf("first deny n=%d", n)
	}
	if len(gt.st.Pending()) != 0 {
		t.Fatalf("pending action survived denial: %+v", gt.st.Pending())
	}
	time.Sleep(50 * time.Millisecond)
	if spy.n.Load() != 0 {
		t.Fatalf("denied action dispatched: %d", spy.n.Load())
	}
	if err := os.WriteFile(filepath.Join(dir, id), []byte("denied-again\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if n := gt.consumeDenialMarkers(); n != 0 {
		t.Fatalf("replay denied n=%d", n)
	}
}
