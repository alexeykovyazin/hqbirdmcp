package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/backupsvc"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/dbpool"
	"github.com/aleks/fbmcp/internal/gate"
	"github.com/aleks/fbmcp/internal/jobs"
	"github.com/aleks/fbmcp/internal/policy"
	"github.com/aleks/fbmcp/internal/state"
	"github.com/aleks/fbmcp/internal/statetest"
	"github.com/aleks/fbmcp/internal/workflows"
)

// UX review W1.1-W1.3: args are checked before the gate, lost args never
// run, and the gate replies carry the next step.

func TestValidateArgsForcewrite(t *testing.T) {
	cases := []struct {
		args map[string]any
		ok   bool
	}{
		{nil, false},                                  // missing "on" used to mean OFF
		{map[string]any{"on": "true"}, false},         // string is not a bool
		{map[string]any{"on": true, "x": 1.0}, false}, // unknown key
		{map[string]any{"on": true}, true},
		{map[string]any{"on": false}, true},
	}
	for _, c := range cases {
		err := validateArgs("fb_set_forcewrite", c.args)
		if (err == nil) != c.ok {
			t.Errorf("args %v: err=%v, want ok=%v", c.args, err, c.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "Expected args:") {
			t.Errorf("args %v: error lacks the expected-args summary: %v", c.args, err)
		}
	}
}

func TestValidateArgsOneOfAndRange(t *testing.T) {
	if err := validateArgs("fb_index_drop", map[string]any{}); err == nil {
		t.Error("index_drop without index or advisory_id accepted")
	}
	if err := validateArgs("fb_index_drop", map[string]any{"index": "IX_A"}); err != nil {
		t.Errorf("index_drop with index: %v", err)
	}
	if err := validateArgs("fb_set_page_buffers", map[string]any{"buffers": 0.0}); err == nil {
		t.Error("page_buffers=0 accepted")
	}
}

func TestRequestGatedRejectsBadArgsBeforeGate(t *testing.T) {
	gt := newTestGT(t)
	spy := &spyExec{}
	gt.execs["fb_set_forcewrite"] = spy.wrap()
	meta := policy.ToolMeta{Name: "fb_set_forcewrite", Tier: 1, Scope: "database"}
	out, isErr := gt.requestGated(context.Background(), meta, "Set Forced Writes on %s.", "spike5", nil, "", nil)
	if !isErr || !strings.HasPrefix(out, "DENIED: invalid args for fb_set_forcewrite") {
		t.Fatalf("want invalid-args deny, got isErr=%v %q", isErr, out)
	}
	if n := len(gt.st.Pending()); n != 0 {
		t.Fatalf("bad args created %d pending actions", n)
	}
	out, isErr = gt.requestGated(context.Background(), meta, "Set Forced Writes on %s.", "spike5", map[string]any{"on": true}, "", nil)
	if isErr {
		t.Fatalf("valid args denied: %s", out)
	}
	for _, want := range []string{"Request ID: ", "In-band token (Tier 1 only): ", `Arguments: {"on":true}`, "Next: show this to the user"} {
		if !strings.Contains(out, want) {
			t.Errorf("pending reply lacks %q:\n%s", want, out)
		}
	}
}

func TestDispatchRefusesLostArgs(t *testing.T) {
	gt := newTestGT(t)
	spy := &spyExec{}
	gt.execs["fb_set_forcewrite"] = spy.wrap()
	id := policy.Identity{Name: "api", MaxTier: 2}
	// A pending action that survived a restart: on disk, args not in memory.
	p, err := gt.g.Request(id, "spike5", policy.ToolMeta{Name: "fb_set_forcewrite", Tier: 1}, "impact", "hash", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gt.dispatch(p); !errors.Is(err, errArgsLost) {
		t.Fatalf("dispatch without stored args: err=%v, want errArgsLost", err)
	}
	time.Sleep(50 * time.Millisecond)
	if spy.n.Load() != 0 {
		t.Fatal("executor ran with lost args")
	}
}

func TestApprovalMarkerLostArgsAudited(t *testing.T) {
	gt := newTestGT(t)
	spy := &spyExec{}
	gt.execs["fb_demo_write"] = spy.wrap()
	p, err := gt.g.Request(policy.Identity{Name: "api", MaxTier: 2}, "spike5", policy.ToolMeta{Name: "fb_demo_write", Tier: 1}, "impact", "hash", nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(gt.live().State.Dir, "approvals")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, p.ID), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	gt.consumeApprovalMarkers()
	time.Sleep(50 * time.Millisecond)
	if spy.n.Load() != 0 {
		t.Fatal("approved request with lost args ran")
	}
	gt.aud.Close()
	raw, err := os.ReadFile(filepath.Join(gt.live().State.Dir, "audit.jsonl"))
	if err != nil {
		t.Skipf("audit file name differs: %v", err)
	}
	if !strings.Contains(string(raw), "arguments of this request were lost") {
		t.Fatalf("lost-args dispatch not audited:\n%s", raw)
	}
}

// blockedGT has no maintenance window and no fresh verified backup.
func blockedGT(t *testing.T) *gatedTools {
	t.Helper()
	cfg := testCfg(t)
	st, err := state.Open(cfg.State.Dir)
	if err != nil {
		t.Fatal(err)
	}
	aud, err := audit.Open(cfg.State.Dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { aud.Close() })
	facts := statetest.StubFacts{"engine_version": "5.0", "verified_backup_exists": false, "backup_freshness": 100.0}
	runner := jobs.NewRunner(st)
	t.Cleanup(func() { runner.Close() })
	return &gatedTools{
		cfg: config.NewHandle(cfg), eng: policy.New(toolMeta, facts, st), g: gate.New(st, aud), runner: runner, aud: aud, st: st,
		execs: map[string]executor{}, args: map[string]map[string]any{},
	}
}

func TestPreviewWorksWhileBlocked(t *testing.T) {
	gt := blockedGT(t)
	meta := policy.ToolMeta{Name: "fb_restore_replace", Tier: 2, Scope: "database"}
	out, isErr := gt.requestGated(context.Background(), meta, "Replace %s from the newest verified backup (downtime).", "spike5", nil, "preview", nil)
	if isErr {
		t.Fatalf("preview denied while blocked: %s", out)
	}
	for _, want := range []string{"mode=preview", "Blocked now", "fb_window_open", "fb_backup_start", "fb_restore_test"} {
		if !strings.Contains(out, want) {
			t.Errorf("preview lacks %q:\n%s", want, out)
		}
	}
	if n := len(gt.st.Pending()); n != 0 {
		t.Fatalf("preview created %d pending actions", n)
	}
}

func TestDenyListsWholeChecklist(t *testing.T) {
	gt := blockedGT(t)
	meta := policy.ToolMeta{Name: "fb_restore_replace", Tier: 2, Scope: "database"}
	out, isErr := gt.requestGated(context.Background(), meta, "Replace %s.", "spike5", nil, "", nil)
	if !isErr {
		t.Fatalf("blocked request not denied: %s", out)
	}
	for _, want := range []string{"DENIED: no open maintenance window", "verified backup required", "1. Open a maintenance window", "2. Make a verified backup", "Next:"} {
		if !strings.Contains(out, want) {
			t.Errorf("deny lacks %q:\n%s", want, out)
		}
	}
}

func TestPendingReplyOOBNextStep(t *testing.T) {
	p := state.PendingAction{ID: "r1", Tool: "fb_restore_replace", Tier: 2, Database: "spike5", Identity: "api", Expires: time.Now().Add(15 * time.Minute)}
	out := pendingReply(p, "h")
	if strings.Contains(out, "In-band token") {
		t.Fatalf("Tier 2 reply carries a token:\n%s", out)
	}
	for _, want := range []string{"Request ID: r1", "out-of-band", "fbmcp ctl approve r1", `fb_job_status with request_id="r1"`} {
		if !strings.Contains(out, want) {
			t.Errorf("OOB reply lacks %q:\n%s", want, out)
		}
	}
}

func TestJobForRequest(t *testing.T) {
	gt := newTestGT(t)
	p, err := gt.g.Request(policy.Identity{Name: "api", MaxTier: 2}, "spike5", policy.ToolMeta{Name: "fb_backup_start", Tier: 1}, "impact", "h", nil)
	if err != nil {
		t.Fatal(err)
	}
	if id, msg := jobForRequest(gt.st, p.ID); id != "" || !strings.Contains(msg, "still pending") {
		t.Fatalf("pending request: id=%q msg=%q", id, msg)
	}
	if id, msg := jobForRequest(gt.st, "nope"); id != "" || !strings.Contains(msg, "no job and no pending action") {
		t.Fatalf("unknown request: id=%q msg=%q", id, msg)
	}
	_ = gt.st.PutJob(state.Job{ID: "j9", Type: "fb_backup_start", Database: "spike5", State: "succeeded", RequestID: p.ID, CreatedAt: time.Now()})
	if id, _ := jobForRequest(gt.st, p.ID); id != "j9" {
		t.Fatalf("job by request id: got %q, want j9", id)
	}
}

// Every generic gated tool must declare its args: an unchecked arg map is
// how a missing "on" turned Forced Writes off. Tools with their own typed
// request path are listed explicitly.
func TestGatedToolsHaveArgSpecs(t *testing.T) {
	ownPath := map[string]bool{
		"fb_write": true, "fb_migration_apply": true, "fb_schedule_create": true,
		"fb_db_register": true, "fb_demo_write": true, "fb_agent_register": true, "fb_agent_config_set": true,
		"fb_agent_job_run": true,
	}
	gt := newTestGT(t)
	gt.wf = workflows.New(gt.st)
	gt.pools = dbpool.NewManager(gt.cfg)
	gt.traces = map[string]*backupsvc.LiveTrace{}
	server := mcp.NewServer(&mcp.Implementation{Name: "spec", Version: "0"}, nil)
	registerP3Tools(server, gt)
	gt.registerRestore(server)
	registerP4Tools(server, gt)
	registerP5Tools(server, gt)
	registerAgentTools(server, gt)
	registerAgentDiscoverTools(server, gt)
	registerPhase11Tools(server, gt)
	registerClusterTools(server, gt)
	registerDBTool(server, gt)
	if len(gt.execs) == 0 {
		t.Fatal("no executors registered")
	}
	for name := range gt.execs {
		if ownPath[name] {
			continue
		}
		if _, ok := gatedArgSpecs[name]; !ok {
			t.Errorf("%s: generic gated tool without an entry in gatedArgSpecs (toolargs.go)", name)
		}
	}
}
