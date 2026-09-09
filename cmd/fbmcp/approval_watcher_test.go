package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aleks/fbmcp/internal/policy"
	"github.com/aleks/fbmcp/internal/state"
)

// P2.9 (test_plan): the OOB approval/denial marker paths (the goroutine in
// startApprovalWatcher only polls these every 2s — the unit tests call the
// consume functions directly).

func pendingForWatcher(t *testing.T, gt *gatedTools, tool string) state.PendingAction {
	t.Helper()
	id := policy.Identity{Name: "api", MaxTier: 2}
	p, err := gt.g.Request(id, "spike5", policy.ToolMeta{Name: tool, Tier: 1}, "impact", "hash", nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestApprovalMarkerConsumesAndDispatches(t *testing.T) {
	gt := newTestGT(t)
	spy := &spyExec{}
	gt.execs["fb_demo_write"] = spy.wrap()
	p := pendingForWatcher(t, gt, "fb_demo_write")

	approve := filepath.Join(gt.live().State.Dir, "approvals")
	if err := os.MkdirAll(approve, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(approve, p.ID), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if n := gt.consumeApprovalMarkers(); n != 1 {
		t.Fatalf("consumeApprovalMarkers=%d, want 1", n)
	}
	if len(gt.st.Pending()) != 0 {
		t.Fatal("pending action still present after approval")
	}
	if _, err := os.Stat(filepath.Join(approve, p.ID)); !os.IsNotExist(err) {
		t.Fatal("approval marker not removed")
	}
}

func TestApprovalMarkerUnknownIDIsDropped(t *testing.T) {
	gt := newTestGT(t)
	approve := filepath.Join(gt.live().State.Dir, "approvals")
	if err := os.MkdirAll(approve, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(approve, "no-such-request")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if n := gt.consumeApprovalMarkers(); n != 0 {
		t.Fatalf("consumed=%d for unknown id, want 0", n)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("unknown marker not dropped")
	}
	if len(gt.st.Pending()) != 0 {
		t.Fatal("phantom pending appeared")
	}
}

func TestDenialMarkerRemovesPendingWithoutDispatch(t *testing.T) {
	gt := newTestGT(t)
	calls := 0
	gt.execs["fb_demo_write"] = func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
		calls++
		return "", nil
	}
	p := pendingForWatcher(t, gt, "fb_demo_write")

	deny := filepath.Join(gt.live().State.Dir, "denials")
	if err := os.MkdirAll(deny, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deny, p.ID), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if n := gt.consumeDenialMarkers(); n != 1 {
		t.Fatalf("consumeDenialMarkers=%d, want 1", n)
	}
	if len(gt.st.Pending()) != 0 {
		t.Fatal("denied pending action still present")
	}
	if calls != 0 {
		t.Fatal("denial must never dispatch")
	}
}

// NOTE: no test drives startApprovalWatcher's poll loop itself — the loop is
// a plain 2s ticker with no shutdown WaitGroup, so a goroutine can still be
// mid-dispatch when the test's runner.Close() cleanup runs (observed as
// "send on closed channel" on CI). Covering the loop would require a
// production WaitGroup; the consume functions above carry the logic.
