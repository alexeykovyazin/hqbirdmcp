package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/config"
)

// agentStub is a minimal fbagent Local API stand-in.
type agentStub struct {
	mu       atomic.Int64 // poll counter per test use
	restarts atomic.Int64
	seenAuth atomic.Value // string
}

func (s *agentStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.seenAuth.Store(r.Header.Get("Authorization"))
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/jobs/sweep/run":
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"operation_id":"p9-1","job":"sweep","status_url":"/v1/jobs/sweep/status","warnings":[]}`))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/instances/fb1/restart":
		s.restarts.Add(1)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"operation_id":"p9-2"}`))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/instances/fb1/firebird-update/apply":
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"operation_id":"p9-3","job":"firebird_update"}`))
	case r.URL.Path == "/v1/operations/p9-1":
		_, _ = w.Write([]byte(`{"operation_id":"p9-1","kind":"job_enqueue","state":"succeeded"}`))
	case r.URL.Path == "/v1/operations/p9-2":
		if s.mu.Add(1) < 2 {
			_, _ = w.Write([]byte(`{"operation_id":"p9-2","kind":"restart","state":"queued"}`))
			return
		}
		_, _ = w.Write([]byte(`{"operation_id":"p9-2","kind":"restart","state":"succeeded"}`))
	case r.URL.Path == "/v1/operations/p9-3":
		_, _ = w.Write([]byte(`{"operation_id":"p9-3","kind":"job_inline","state":"succeeded","result":{"restart_operation_id":"p9-4"}}`))
	case r.URL.Path == "/v1/operations/p9-4":
		_, _ = w.Write([]byte(`{"operation_id":"p9-4","kind":"restart","state":"succeeded"}`))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/instances/fb1":
		_, _ = w.Write([]byte(`{"instance_id":"fb1","service_name":"FirebirdServerDefaultInstance","state":"running","uptime_sec":3600,"port":3050}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no route ` + r.URL.Path + `"}}`))
	}
}

// newAgentGT builds a gatedTools with one configured agent pointing at an
// httptest stub, plus a server with the bridge registered.
func newAgentGT(t *testing.T, mutate func(a *config.Agent)) (*gatedTools, *mcp.Server) {
	t.Helper()
	gt := newTestGT(t)
	srv := httptest.NewServer(&agentStub{})
	t.Cleanup(srv.Close)
	t.Setenv("FBMCP_TEST_AGENT_TOKEN", "tok9")
	cfg := gt.live()
	cfg.Agents = []config.Agent{{ID: "ag1", BaseURL: srv.URL, TokenEnv: "FBMCP_TEST_AGENT_TOKEN"}}
	if mutate != nil {
		mutate(&cfg.Agents[0])
	}
	gt.cfg = config.NewHandle(cfg)
	server := mcp.NewServer(&mcp.Implementation{Name: "agent-test", Version: "0"}, nil)
	registerAgentTools(server, gt)
	return gt, server
}

func TestAgentCfgResolution(t *testing.T) {
	gt := newTestGT(t) // no agents configured
	if _, err := gt.agentCfg(""); err == nil {
		t.Fatal("no-agent case accepted")
	}
	gt, _ = newAgentGT(t, nil)
	if a, err := gt.agentCfg(""); err != nil || a.ID != "ag1" {
		t.Fatalf("single-agent default: %v %+v", err, a)
	}
	if _, err := gt.agentCfg("nope"); err == nil {
		t.Fatal("unknown agent id accepted")
	}
	// two agents: an explicit selector is required
	gt2, _ := newAgentGT(t, func(a *config.Agent) {})
	live := gt2.live()
	live.Agents = append(live.Agents, config.Agent{ID: "ag2", BaseURL: live.Agents[0].BaseURL, TokenEnv: "FBMCP_TEST_AGENT_TOKEN"})
	if _, err := gt2.agentCfg(""); err == nil {
		t.Fatal("multi-agent default accepted")
	}
	if a, err := gt2.agentCfg("ag2"); err != nil || a.ID != "ag2" {
		t.Fatalf("explicit selector: %v %+v", err, a)
	}
}

func TestAgentJobRunGatePaths(t *testing.T) {
	gt, _ := newAgentGT(t, nil)
	ctx := context.Background()

	// Tier-1 light job: pending with in-band token.
	msg, denied := gt.requestAgentJobRun(ctx, agentJobRunArg{Job: "sweep", JobID: "nightly"})
	if denied || !contains(msg, "In-band token") {
		t.Fatalf("light job: denied=%v msg=%q", denied, msg)
	}
	// Tier-2 hot job without a window on db: denied.
	msg, denied = gt.requestAgentJobRun(ctx, agentJobRunArg{Job: "restore", Db: "nonexistent-db"})
	if !denied || !contains(msg, "no open maintenance window") {
		t.Fatalf("hot job w/o window: denied=%v msg=%q", denied, msg)
	}
	// Tier-2 hot job with the spike5 window (opened by newTestGT): pending.
	msg, denied = gt.requestAgentJobRun(ctx, agentJobRunArg{Job: "restore", Db: "spike5"})
	if denied || !contains(msg, "restore") {
		t.Fatalf("hot job w/ window: denied=%v msg=%q", denied, msg)
	}
	// sql_script without a script: refused before the gate.
	msg, denied = gt.requestAgentJobRun(ctx, agentJobRunArg{Job: "sql_script"})
	if !denied || !contains(msg, "scriptPath") {
		t.Fatalf("sql_script validation: denied=%v msg=%q", denied, msg)
	}
	// preview: informational only, no pending created.
	before := len(gt.st.Pending())
	msg, denied = gt.requestAgentJobRun(ctx, agentJobRunArg{Job: "sweep", Mode: "preview"})
	if denied || !contains(msg, "mode=preview") {
		t.Fatalf("preview: denied=%v msg=%q", denied, msg)
	}
	if after := len(gt.st.Pending()); after != before {
		t.Fatalf("preview created pendings: %d → %d", before, after)
	}
}

func TestAgentJobRunTier2Override(t *testing.T) {
	gt, _ := newAgentGT(t, func(a *config.Agent) { a.Tier2Jobs = []string{"perf_test"} })
	msg, denied := gt.requestAgentJobRun(context.Background(), agentJobRunArg{Job: "perf_test", Db: "spike5"})
	if denied || !contains(msg, "hot-list") {
		t.Fatalf("tier2_jobs override not applied: denied=%v msg=%q", denied, msg)
	}
}

func TestAgentExecutors(t *testing.T) {
	gt, _ := newAgentGT(t, nil)
	ctx := context.Background()
	prog := func(float64, string) {}

	out, err := gt.execs["fb_agent_job_run"](ctx, "", map[string]any{
		"job": "sweep", "job_id": "nightly", "_grant_identity": "tester",
	}, prog)
	if err != nil {
		t.Fatalf("job_run executor: %v", err)
	}
	if !contains(out, "succeeded") || !contains(out, "enqueued") {
		t.Fatalf("job_run out = %q", out)
	}

	out, err = gt.execs["fb_agent_restart"](ctx, "spike5", map[string]any{
		"reason": "test", "_grant_identity": "tester",
	}, prog)
	if err != nil {
		t.Fatalf("restart executor: %v", err)
	}
	if !contains(out, "restart") || !contains(out, "succeeded") {
		t.Fatalf("restart out = %q", out)
	}

	out, err = gt.execs["fb_agent_update_apply"](ctx, "spike5", map[string]any{
		"_grant_identity": "tester",
	}, prog)
	if err != nil {
		t.Fatalf("update_apply executor: %v", err)
	}
	if !contains(out, "p9-4") || !contains(out, "nested restart") {
		t.Fatalf("update_apply out = %q", out)
	}
}

func TestAgentReadToolOverMCP(t *testing.T) {
	_, server := newAgentGT(t, nil)
	srvConn, cliConn := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go server.Run(ctx, &mcp.IOTransport{Reader: srvConn, Writer: srvConn})
	defer cliConn.Close()
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "agent-test-client", Version: "0"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: cliConn, Writer: cliConn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "fb_agent_instances", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("instances call errored: %+v", res.Content)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok || !contains(tc.Text, "running") {
		t.Fatalf("instances text = %+v", res.Content)
	}

	// Unknown agent id → structured error.
	res, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: "fb_agent_instances", Arguments: map[string]any{"agent": "nope"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("unknown agent accepted")
	}
}

func TestAgentArgCoercion(t *testing.T) {
	if got := agentStrSlice([]any{"a", "b"}); len(got) != 2 || got[0] != "a" {
		t.Fatalf("agentStrSlice = %v", got)
	}
	if got := agentStrSlice([]string{"x"}); len(got) != 1 {
		t.Fatalf("agentStrSlice native = %v", got)
	}
	if n, ok := agentInt(float64(7)); !ok || n != 7 {
		t.Fatalf("agentInt = %d %v", n, ok)
	}
	b, _ := json.Marshal(map[string]any{"agent": "z"})
	var base agentArg
	if err := json.Unmarshal(b, &base); err != nil || base.Agent != "z" {
		t.Fatalf("agentArg round-trip: %v %+v", err, base)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
