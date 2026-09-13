package main

// Phase 9L — live-lab suite for the fbagent bridge (phase9_live_test_plan.md).
// Drives every bridge tool through a real MCP session against the fbagent
// deployed on this lab (default http://127.0.0.1:13055, instance
// fb-master-3055). Opt-in and CI-safe:
//
//	FBMCP_AGENT_LIVE=1 \
//	FBMCP_AGENT_TOKEN=<agent local_api.token> \
//	go test ./cmd/fbmcp -run AgentLive -v
//
// Extended / engine tiers add FBMCP_AGENT_LIVE_EXTENDED=1 and
// FBMCP_AGENT_LIVE_ENGINE=yes-restart-fb5 (the latter RESTARTS the FB5
// service the soak kernel attaches to — pause fbmcp-soak-week first; see
// the Tier C pre-flight below). This file is the only live suite that
// MUTATES agent state (one reload, one job run per pass);
// internal/agentapi's live suite is GET-only, so parallel
// `go test ./...` packages stay safe.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/agentapi"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/gate"
)

const (
	agentLiveLab     = "labfb1"
	agentLiveBadTok  = "badtok"
	agentLiveSpikeDB = "spike5" // window/audit key (testCfg registers it)
)

type agentLiveEnv struct{ base, inst, tokEnv string }

func agentLiveGate(t *testing.T) agentLiveEnv {
	t.Helper()
	if os.Getenv("FBMCP_AGENT_LIVE") != "1" {
		t.Skip("set FBMCP_AGENT_LIVE=1 to run against the lab agent")
	}
	env := agentLiveEnv{
		base:   envOr("FBMCP_AGENT_LIVE_URL", "http://127.0.0.1:13055"),
		inst:   envOr("FBMCP_AGENT_LIVE_INSTANCE", "fb-master-3055"),
		tokEnv: envOr("FBMCP_AGENT_LIVE_TOKEN_ENV", "FBMCP_AGENT_TOKEN"),
	}
	if os.Getenv(env.tokEnv) == "" {
		t.Logf("token env %s unset — relying on keyring fallback (fbmcpctl secret set %s)", env.tokEnv, env.tokEnv)
	}
	return env
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// agentLiveSetup builds the in-process MCP server over a newTestGT whose
// config carries the lab agent (+ a wrong-token entry for auth tests) and
// connects a session. No config file is touched — everything comes from env.
func agentLiveSetup(t *testing.T) (*gatedTools, *mcp.ClientSession, agentLiveEnv) {
	t.Helper()
	env := agentLiveGate(t)
	gt := newTestGT(t) // testCfg: spike5 registered + maintenance window open
	t.Setenv("FBMCP_LIVE_BADTOK", "definitely-wrong")
	cfg := gt.live()
	cfg.Agents = []config.Agent{
		{ID: agentLiveLab, BaseURL: env.base, TokenEnv: env.tokEnv, InstanceID: env.inst, TimeoutSeconds: 130},
		{ID: agentLiveBadTok, BaseURL: env.base, TokenEnv: "FBMCP_LIVE_BADTOK", InstanceID: env.inst, TimeoutSeconds: 130},
	}
	gt.cfg = config.NewHandle(cfg)

	server := mcp.NewServer(&mcp.Implementation{Name: "agent-live-test", Version: "0"}, nil)
	registerAgentTools(server, gt)
	srvConn, cliConn := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	go server.Run(ctx, &mcp.IOTransport{Reader: srvConn, Writer: srvConn})
	t.Cleanup(func() { cliConn.Close() })
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "agent-live-client", Version: "0"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: cliConn, Writer: cliConn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })

	// Pre-flight: one cheap authenticated call so a bad token/URL fails
	// once, here, with a hint — not scattered across 20 tests.
	cl, err := agentapi.New(env.base, os.Getenv(env.tokEnv))
	if err != nil {
		t.Fatal(err)
	}
	pctx, pcancel := context.WithTimeout(ctx, 15*time.Second)
	defer pcancel()
	if _, err := cl.GetInstances(pctx, env.inst); err != nil {
		if strings.Contains(err.Error(), "fbagent auth") {
			t.Fatalf("pre-flight: %v — set %s (env or fbmcpctl secret set %s)", err, env.tokEnv, env.tokEnv)
		}
		t.Fatalf("pre-flight: %v — is the agent up on %s?", err, env.base)
	}
	return gt, sess, env
}

// --- call helpers ---

func liveCall(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, string, map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: transport: %v", name, err)
	}
	text := ""
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			text = tc.Text
		}
	}
	sc, _ := res.StructuredContent.(map[string]any)
	return res, text, sc
}

func liveOK(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, text, _ := liveCall(t, sess, name, args)
	if res.IsError {
		t.Fatalf("%s %+v errored: %s", name, args, text)
	}
	return text
}

// liveWantErr asserts the errdoc envelope code on an IsError result.
func liveWantErr(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any, code string) {
	t.Helper()
	res, text, sc := liveCall(t, sess, name, args)
	if !res.IsError {
		t.Fatalf("%s %+v: want %s error, got success: %s", name, args, code, text)
	}
	if sc == nil || sc["code"] != code {
		t.Fatalf("%s %+v: want structured code %q, got %+v (text %q)", name, args, code, sc, text)
	}
}

var (
	reInBandToken = regexp.MustCompile(`In-band token \(Tier 1 only\):\s*(\S+)`)
	reRequestID   = regexp.MustCompile(`Request ID:\s*(\S+)`)
)

// liveParsePending extracts the in-band token and request id from a
// pending statement (ImpactStatement + token line).
func liveParsePending(t *testing.T, pending string) (token, reqID string) {
	t.Helper()
	m := reInBandToken.FindStringSubmatch(pending)
	if m == nil {
		t.Fatalf("no in-band token in pending text:\n%s", pending)
	}
	id := reRequestID.FindStringSubmatch(pending)
	if id == nil {
		t.Fatalf("no Request ID in pending text:\n%s", pending)
	}
	return m[1], id[1]
}

// liveConfirm drives the Tier-1 confirm the way the production fb_confirm
// tool does: g.Confirm with the in-band token, then dispatch, then block
// on the job store until terminal. (fb_confirm itself is registered inline
// in production main.go — outside this in-process server — and its MCP
// wiring is already unit-covered; the gate/dispatch/executor/poll chain
// exercised here is the real production path.) Returns the job result
// message + whether it failed.
func liveConfirm(t *testing.T, gt *gatedTools, pending string) (string, bool) {
	t.Helper()
	token, reqID := liveParsePending(t, pending)
	var as string
	for _, p := range gt.st.Pending() {
		if p.ID == reqID {
			as = p.Identity
		}
	}
	if as == "" {
		t.Fatalf("pending %s not found", reqID)
	}
	p, err := gt.g.Confirm(reqID, as, gate.ChannelInBand, token)
	if err != nil {
		return err.Error(), true
	}
	jobID, err := gt.dispatch(p)
	if err != nil {
		return err.Error(), true
	}
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		j, ok := gt.runner.Status(jobID)
		if ok {
			switch j.State {
			case "succeeded":
				return j.Message, false
			case "failed", "cancelled", "interrupted":
				return fmt.Sprintf("job %s: %s: %s", j.ID, j.State, j.Message), true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "job did not finish within 3m", true
}

// liveSpikeKey derives the sanitized database-folder key for the spike DB
// from the live /v1/databases artifact and verifies it via a listing
// (the agent serves no arbitrary server-scoped artifact read — probed 404 —
// so nothing here relies on one).
func liveSpikeKey(t *testing.T, sess *mcp.ClientSession) string {
	t.Helper()
	text := liveOK(t, sess, "fb_agent_databases", map[string]any{"agent": agentLiveLab})
	var doc struct {
		Databases []struct {
			Path string `json:"path"`
		} `json:"databases"`
	}
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("databases artifact not JSON: %v\n%s", err, text)
	}
	var key string
	for _, d := range doc.Databases {
		if strings.Contains(strings.ToUpper(d.Path), "SPIKE_FB5.0.FDB") {
			key = strings.NewReplacer("\\", "_", "/", "_", ":", "_", ".", "_").Replace(strings.ToUpper(d.Path))
			break
		}
	}
	if key == "" {
		t.Skip("spike database not in the agent's active-databases artifact")
	}
	// verify the derived key actually resolves (dash preserved, dots are
	// not — fall back to the historically observed variants)
	candidates := append([]string{key},
		"C_HQBIRDDATA_OUTPUT_FBMCP-SPIKE_SPIKE_FB5_0_FDB",
		"C_HQBIRDDATA_OUTPUT_FBMCP_SPIKE_SPIKE_FB5_0_FDB")
	for _, cand := range candidates {
		list := liveOK(t, sess, "fb_agent_artifact_list", map[string]any{"agent": agentLiveLab, "scope": cand, "glob": "*"})
		var ldoc struct {
			Count int `json:"count"`
		}
		if json.Unmarshal([]byte(list), &ldoc) == nil && ldoc.Count > 0 {
			return cand
		}
	}
	t.Skip("no resolvable db folder key for the spike database")
	return ""
}

// --- Tier A (default suite) ---

func TestAgentLiveInstances(t *testing.T) {
	_, sess, env := agentLiveSetup(t)
	text := liveOK(t, sess, "fb_agent_instances", map[string]any{"agent": agentLiveLab})
	if !strings.Contains(text, env.inst) || !strings.Contains(text, "FirebirdServerHQBirdInstanceFB5") {
		t.Fatalf("instances missing %s / service:\n%s", env.inst, text)
	}
	detail := liveOK(t, sess, "fb_agent_instances", map[string]any{"agent": agentLiveLab, "instance_id": env.inst})
	if !strings.Contains(detail, `"port": 3055`) {
		t.Fatalf("instance detail missing port:\n%s", detail)
	}
}

func TestAgentLiveStatus(t *testing.T) {
	_, sess, _ := agentLiveSetup(t)
	full := liveOK(t, sess, "fb_agent_status", map[string]any{"agent": agentLiveLab})
	var doc map[string]any
	if err := json.Unmarshal([]byte(full), &doc); err != nil {
		t.Fatalf("status not JSON: %v", err)
	}
	health := liveOK(t, sess, "fb_agent_status", map[string]any{"agent": agentLiveLab, "view": "health"})
	if !strings.Contains(health, "severity") {
		t.Fatalf("health view missing severity:\n%s", health)
	}
	backups := liveOK(t, sess, "fb_agent_status", map[string]any{"agent": agentLiveLab, "section": "backups"})
	var b map[string]any
	if err := json.Unmarshal([]byte(backups), &b); err != nil {
		t.Fatalf("backups section not JSON: %v", err)
	}
	// section and view are mutually exclusive (client-side)
	res, text, _ := liveCall(t, sess, "fb_agent_status", map[string]any{"agent": agentLiveLab, "section": "backups", "view": "health"})
	if !res.IsError || !strings.Contains(text, "pass section or view") {
		t.Fatalf("section+view together: want client-side refusal, got IsError=%v %s", res.IsError, text)
	}
}

func TestAgentLiveDatabases(t *testing.T) {
	_, sess, _ := agentLiveSetup(t)
	text := liveOK(t, sess, "fb_agent_databases", map[string]any{"agent": agentLiveLab})
	if !strings.Contains(text, "SPIKE_FB5.0.FDB") {
		t.Fatalf("spike missing from active databases:\n%s", text)
	}
	var doc struct {
		DatabaseCount int `json:"database_count"`
	}
	if err := json.Unmarshal([]byte(text), &doc); err != nil || doc.DatabaseCount < 1 {
		t.Fatalf("database_count: %v %+v", err, doc)
	}
}

func TestAgentLiveArtifactListing(t *testing.T) {
	_, sess, _ := agentLiveSetup(t)
	text := liveOK(t, sess, "fb_agent_artifact_list", map[string]any{"agent": agentLiveLab, "scope": "server", "glob": "*"})
	var doc struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(text), &doc); err != nil || doc.Count < 10 {
		t.Fatalf("server listing: %v count=%+v\n%s", err, doc, text)
	}
}

func TestAgentLiveArtifactGet(t *testing.T) {
	_, sess, _ := agentLiveSetup(t)
	text := liveOK(t, sess, "fb_agent_artifact_get", map[string]any{"agent": agentLiveLab, "scope": "server", "path": "backups/status"})
	var doc struct {
		SchemaVersion int `json:"schema_version"`
		Databases     []any
	}
	if err := json.Unmarshal([]byte(text), &doc); err != nil || doc.SchemaVersion != 1 || len(doc.Databases) == 0 {
		t.Fatalf("backups/status: %v %+v", err, doc)
	}
	// lwm/summary?date=today — skip when the collector has no summary yet
	date := time.Now().Format("2006-01-02")
	res, _, sc := liveCall(t, sess, "fb_agent_artifact_get", map[string]any{"agent": agentLiveLab, "scope": "server", "path": "lwm/summary", "date": date})
	if res.IsError {
		if sc == nil || sc["code"] != "agent-not-found" {
			t.Fatalf("lwm/summary error is not not-found: %+v", sc)
		}
		t.Logf("no lwm summary for %s — skipped", date)
	}
}

func TestAgentLiveArtifactGstatTail(t *testing.T) {
	_, sess, _ := agentLiveSetup(t)
	key := liveSpikeKey(t, sess)
	text := liveOK(t, sess, "fb_agent_artifact_get", map[string]any{"agent": agentLiveLab, "scope": key, "path": "gstat-full/latest", "tail": 40})
	var doc struct {
		File string `json:"file"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(text), &doc); err != nil || doc.File == "" || doc.Body == "" {
		t.Fatalf("gstat-full/latest: %v %+v", err, doc)
	}
	if lines := strings.Count(doc.Body, "\n") + 1; lines > 41 {
		t.Fatalf("tail=40 returned %d lines", lines)
	}
	t.Logf("gstat via %s: %s", key, doc.File)
}

func TestAgentLiveArtifactMetricsConditional(t *testing.T) {
	_, sess, _ := agentLiveSetup(t)
	key := liveSpikeKey(t, sess)
	list := liveOK(t, sess, "fb_agent_artifact_list", map[string]any{"agent": agentLiveLab, "scope": key, "glob": "DatabaseMetrics/*"})
	var ldoc struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal([]byte(list), &ldoc)
	res, _, sc := liveCall(t, sess, "fb_agent_artifact_get", map[string]any{"agent": agentLiveLab, "scope": key, "path": "metrics/latest"})
	if ldoc.Count == 0 {
		if !res.IsError || sc == nil || sc["code"] != "agent-not-found" {
			t.Fatalf("empty collector: want agent-not-found, got IsError=%v sc=%+v", res.IsError, sc)
		}
		t.Log("metrics collector still empty — pinned negative holds")
		return
	}
	if res.IsError {
		t.Fatalf("collector has %d files but metrics/latest errored: %+v", ldoc.Count, sc)
	}
}

func TestAgentLiveArtifactBadInputs(t *testing.T) {
	_, sess, _ := agentLiveSetup(t)
	liveWantErr(t, sess, "fb_agent_artifact_list", map[string]any{"agent": agentLiveLab, "scope": "a/b", "glob": "*"}, "agent-remote-denied")
	liveWantErr(t, sess, "fb_agent_artifact_get", map[string]any{"agent": agentLiveLab, "scope": "server", "path": "../../etc/passwd"}, "agent-remote-denied")
	liveWantErr(t, sess, "fb_agent_artifact_get", map[string]any{"agent": agentLiveLab, "scope": "C__x", "path": "backups/status", "tail": 5}, "agent-remote-denied")
}

func TestAgentLiveJobStatus(t *testing.T) {
	_, sess, _ := agentLiveSetup(t)
	text := liveOK(t, sess, "fb_agent_job_status", map[string]any{"agent": agentLiveLab, "job": "sweep"})
	if !strings.Contains(text, "status_url") {
		t.Fatalf("sweep status missing wrapper fields:\n%s", text)
	}
	res, _, sc := liveCall(t, sess, "fb_agent_job_status", map[string]any{"agent": agentLiveLab, "job": "nbackup"})
	if res.IsError {
		if sc == nil || sc["code"] != "agent-not-found" {
			t.Fatalf("nbackup status: want not-found, got %+v", sc)
		}
		t.Log("no nbackup status document on this lab — skipped")
	}
}

func TestAgentLiveDBAccess(t *testing.T) {
	_, sess, _ := agentLiveSetup(t)
	text := liveOK(t, sess, "fb_agent_dbaccess", map[string]any{"agent": agentLiveLab})
	var inv struct {
		Mode     string `json:"mode"`
		ConfPath string `json:"conf_path"`
	}
	if err := json.Unmarshal([]byte(text), &inv); err != nil || inv.Mode == "" || inv.ConfPath == "" {
		t.Fatalf("dbaccess inventory: %v %+v", err, inv)
	}
	// mode full: every path is allowed with reason ok_full (probed); the
	// alias path does NOT surface matched_alias in full mode.
	for _, p := range []string{
		`C:\HQbird\Firebird50\examples\empbuild\employee.fdb`,
		`C:\some\where\x.fdb`,
	} {
		chk := liveOK(t, sess, "fb_agent_dbaccess_check", map[string]any{"agent": agentLiveLab, "path": p})
		var c struct {
			Allowed bool   `json:"allowed"`
			Reason  string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(chk), &c); err != nil || !c.Allowed || c.Reason != "ok_full" {
			t.Fatalf("check %q: %v %+v", p, err, c)
		}
	}
}

func TestAgentLiveUpdateStatus(t *testing.T) {
	_, sess, _ := agentLiveSetup(t)
	// The sidecar is absent today (404, documented when-absent) but its
	// existence is the agent's choice, not ours — accept either outcome and
	// assert the shape of whichever came back.
	res, text, sc := liveCall(t, sess, "fb_agent_update_status", map[string]any{"agent": agentLiveLab})
	if res.IsError {
		if sc == nil || sc["code"] != "agent-not-found" {
			t.Fatalf("update status: want not-found while absent, got %+v", sc)
		}
		t.Log("no update state sidecar (update disabled) — pinned negative holds")
		return
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("update sidecar present but unparsable: %v", err)
	}
	t.Log("update sidecar appeared — positive shape asserted")
}

func TestAgentLiveOperationUnknown(t *testing.T) {
	_, sess, _ := agentLiveSetup(t)
	liveWantErr(t, sess, "fb_agent_operation", map[string]any{"agent": agentLiveLab, "operation_id": "phase9L-nonexistent-op"}, "agent-not-found")
}

func TestAgentLiveJobRunRoundTrip(t *testing.T) {
	gt, sess, _ := agentLiveSetup(t)
	pending := liveOK(t, sess, "fb_agent_job_run", map[string]any{
		"agent": agentLiveLab, "db": agentLiveSpikeDB,
		"job": "agent_config_snapshot", "reason": "phase9L live suite",
	})
	result, isErr := liveConfirm(t, gt, pending)
	if isErr || !strings.Contains(result, "succeeded") || !strings.Contains(result, "agent_config_snapshot") {
		t.Fatalf("job run round trip (isErr=%v):\n%s", isErr, result)
	}
}

func TestAgentLiveConfigReload(t *testing.T) {
	gt, sess, _ := agentLiveSetup(t)
	pending := liveOK(t, sess, "fb_agent_config_reload", map[string]any{
		"db": agentLiveSpikeDB, "args": map[string]any{"agent": agentLiveLab},
	})
	result, isErr := liveConfirm(t, gt, pending)
	if isErr || !strings.Contains(result, `"reloaded": true`) {
		t.Fatalf("config reload (isErr=%v):\n%s", isErr, result)
	}
}

func TestAgentLivePprofZero(t *testing.T) {
	gt, sess, _ := agentLiveSetup(t)
	pending := liveOK(t, sess, "fb_agent_pprof", map[string]any{
		"db": agentLiveSpikeDB, "args": map[string]any{"agent": agentLiveLab, "cpu_seconds": 0},
	})
	result, isErr := liveConfirm(t, gt, pending)
	if isErr {
		if strings.Contains(result, "agent-unavailable") {
			t.Skip("agent refused the capture (concurrent/unwired): " + result)
		}
		t.Fatalf("pprof confirm errored: %s", result)
	}
	if !strings.Contains(result, "goroutine") {
		t.Fatalf("pprof result missing goroutine path:\n%s", result)
	}
}

func TestAgentLiveAuthFailure(t *testing.T) {
	_, sess, _ := agentLiveSetup(t)
	liveWantErr(t, sess, "fb_agent_instances", map[string]any{"agent": agentLiveBadTok}, "agent-auth")
}

func TestAgentLiveEngineDenyWithoutWindow(t *testing.T) {
	_, sess, _ := agentLiveSetup(t)
	// registerToolEx returns denials as plain text (house style) — assert
	// on content; the deny happens client-side before any agent call.
	text := liveOK(t, sess, "fb_agent_restart", map[string]any{
		"db": "nonexistent-db", "args": map[string]any{"agent": agentLiveLab, "reason": "phase9L"},
	})
	if !strings.Contains(text, "no open maintenance window") {
		t.Fatalf("want window deny, got:\n%s", text)
	}
}

func TestAgentLiveEnginePendingThenCancel(t *testing.T) {
	gt, sess, _ := agentLiveSetup(t) // newTestGT opened a window for spike5
	pending := liveOK(t, sess, "fb_agent_restart", map[string]any{
		"db": agentLiveSpikeDB, "args": map[string]any{"agent": agentLiveLab, "reason": "phase9L pending test"},
	})
	if !strings.Contains(pending, "out-of-band") || strings.Contains(pending, "In-band token") {
		t.Fatalf("Tier-2 pending must be OOB-only:\n%s", pending)
	}
	id := reRequestID.FindStringSubmatch(pending)
	if id == nil {
		t.Fatalf("no Request ID in pending:\n%s", pending)
	}
	reqID := id[1]
	// fb_cancel is registered inline in production main.go; exercise the
	// exact same store path it uses (TakePending).
	if _, ok, _ := gt.st.TakePending(reqID); !ok {
		t.Fatalf("pending %s not found for cancel", reqID)
	}
	for _, p := range gt.st.Pending() {
		if p.ID == reqID {
			t.Fatal("pending still present after TakePending")
		}
	}
	t.Logf("pending %s cancelled cleanly (no execution, agent untouched)", reqID)
}

// --- Tier B (extended) ---

func TestAgentLiveExtendedPprofCPU(t *testing.T) {
	if os.Getenv("FBMCP_AGENT_LIVE_EXTENDED") != "1" {
		t.Skip("set FBMCP_AGENT_LIVE_EXTENDED=1")
	}
	gt, sess, _ := agentLiveSetup(t)
	pending := liveOK(t, sess, "fb_agent_pprof", map[string]any{
		"db": agentLiveSpikeDB, "args": map[string]any{"agent": agentLiveLab, "cpu_seconds": 2},
	})
	result, isErr := liveConfirm(t, gt, pending)
	if isErr || !strings.Contains(result, "cpu") {
		t.Fatalf("extended pprof result (isErr=%v):\n%s", isErr, result)
	}
}

// --- Tier C (engine; RESTARTS/STOPS THE FB5 SERVICE — coordinate first) ---

// agentLiveEngineGate gates the service-affecting tier: explicit ack env,
// plus the health pre-flight (update lock held → the agent blocks engine
// ops; skip instead of firing a restart into a locked agent). Pausing
// `fbmcp-soak-week` for the window is the operator's responsibility
// (phase9_live_test_plan.md §4c).
func agentLiveEngineGate(t *testing.T, sess *mcp.ClientSession) {
	t.Helper()
	if os.Getenv("FBMCP_AGENT_LIVE_ENGINE") != "yes-restart-fb5" {
		t.Skip("set FBMCP_AGENT_LIVE_ENGINE=yes-restart-fb5 (restarts the FB5 service — coordinate with the soak first)")
	}
	health := liveOK(t, sess, "fb_agent_status", map[string]any{"agent": agentLiveLab, "view": "health"})
	var h struct {
		Update struct {
			ApplyInProgress bool `json:"apply_in_progress"`
		} `json:"update"`
	}
	if err := json.Unmarshal([]byte(health), &h); err == nil && h.Update.ApplyInProgress {
		t.Skip("agent reports update.apply_in_progress — engine ops are locked")
	}
}

// liveEngineOp requests a Tier-2 engine op, approves it out-of-band via
// the C19 approval-marker mechanism (the same channel fbmcpctl/tray use),
// consumes the marker and waits for the executor's job to finish.
func liveEngineOp(t *testing.T, gt *gatedTools, sess *mcp.ClientSession, action, reason string) string {
	t.Helper()
	pending := liveOK(t, sess, "fb_agent_"+action, map[string]any{
		"db": agentLiveSpikeDB, "args": map[string]any{"agent": agentLiveLab, "reason": reason},
	})
	_, reqID := liveParsePending(t, pending)
	markerDir := filepath.Join(gt.live().State.Dir, "approvals")
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(markerDir, reqID), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		gt.consumeApprovalMarkers()
		for _, j := range gt.st.Jobs() {
			if j.RequestID == reqID {
				switch j.State {
				case "succeeded":
					return j.Message
				case "failed", "cancelled", "interrupted":
					t.Fatalf("%s job %s: state=%s msg=%s", action, j.ID, j.State, j.Message)
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("%s job did not finish within 5m", action)
	return ""
}

func TestAgentLiveEngineRestart(t *testing.T) {
	gt, sess, env := agentLiveSetup(t)
	agentLiveEngineGate(t, sess)
	liveEngineOp(t, gt, sess, "restart", "phase9L Tier C engine drill")
	after := liveOK(t, sess, "fb_agent_instances", map[string]any{"agent": agentLiveLab, "instance_id": env.inst})
	if !strings.Contains(after, `"state": "running"`) {
		t.Fatalf("instance not running after restart:\n%s", after)
	}
}

func TestAgentLiveEngineStopStart(t *testing.T) {
	gt, sess, env := agentLiveSetup(t)
	agentLiveEngineGate(t, sess)
	liveEngineOp(t, gt, sess, "stop", "phase9L Tier C stop leg")
	stopped := liveOK(t, sess, "fb_agent_instances", map[string]any{"agent": agentLiveLab, "instance_id": env.inst})
	if !strings.Contains(stopped, `"state": "stopped"`) {
		t.Fatalf("instance not stopped:\n%s", stopped)
	}
	// recovery safety net: if the start leg fails, bring the service back
	// up directly (test-side recovery, outside the gate — documented).
	t.Cleanup(func() {
		cl, err := agentapi.New(env.base, os.Getenv(env.tokEnv))
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		resp, err := cl.EngineOp(ctx, env.inst, "start", agentapi.EngineOpRequest{RequestedBy: "phase9L-cleanup"})
		if err != nil {
			return
		}
		pctx, pcancel := context.WithTimeout(ctx, 3*time.Minute)
		defer pcancel()
		_, _ = cl.PollOperation(pctx, resp.OperationID, 2*time.Second, nil)
	})
	liveEngineOp(t, gt, sess, "start", "phase9L Tier C start leg")
	running := liveOK(t, sess, "fb_agent_instances", map[string]any{"agent": agentLiveLab, "instance_id": env.inst})
	if !strings.Contains(running, `"state": "running"`) {
		t.Fatalf("instance not running after start:\n%s", running)
	}
}
