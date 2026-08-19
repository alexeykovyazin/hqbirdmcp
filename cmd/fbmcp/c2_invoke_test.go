package main

import (
	"context"
	"strings"
	"testing"

	"github.com/aleks/fbmcp/internal/policy"
)

// C2: enumerate toolMeta Tier ≥ 1 and invoke the real request path without
// confirmation. Spy executors must not run (no catalog/job/file side effect).
func TestC2InvokeWithoutConfirm(t *testing.T) {
	gt := newTestGT(t)
	spy := &spyExec{}
	ctx := context.Background()
	n := 0
	for _, m := range toolMeta {
		if m.Tier < 1 {
			continue
		}
		gt.execs[m.Name] = spy.wrap()
		out := gt.requestGated(ctx, m, "impact %s", "spike5", nil, "", nil)
		n++
		if strings.Contains(out, "spy-ran") {
			t.Errorf("%s: executor result leaked into request path: %s", m.Name, out)
		}
	}
	if n == 0 {
		t.Fatal("no gated tools in toolMeta")
	}
	if spy.n.Load() != 0 {
		t.Fatalf("C2 FAIL: executor ran %d times without confirmation", spy.n.Load())
	}
	if len(gt.st.Pending()) == 0 {
		t.Fatal("expected pending actions after gated invoke")
	}
}

func TestC2UnknownDBNoExec(t *testing.T) {
	gt := newTestGT(t)
	spy := &spyExec{}
	m := policy.ToolMeta{Name: "fb_backup_start", Tier: 1, Scope: "database"}
	gt.execs[m.Name] = spy.wrap()
	out := gt.requestGated(context.Background(), m, "x %s", "nope", nil, "", nil)
	if !strings.HasPrefix(out, "DENIED:") {
		t.Fatalf("want DENIED, got %s", out)
	}
	if spy.n.Load() != 0 {
		t.Fatal("executor ran for unknown db")
	}
}
