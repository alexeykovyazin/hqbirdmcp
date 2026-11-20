package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/agentapi"
	"github.com/aleks/fbmcp/internal/config"
)

// dbaStub stands in for the /v1/dba routes of fbagent.
type dbaStub struct {
	mu     sync.Mutex
	checks map[string]int
	runs   map[string]int
	bodies []agentapi.DBARequest
	// staleClass is the class the check of op "stale" returns.
	staleClass string
	// staleRefusals is how many runs of op "stale" are refused first.
	staleRefusals int
}

func (s *dbaStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	write := func(status int, body string) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
	switch {
	case r.URL.Path == "/v1/dba/tools":
		write(200, `{"tools":[{"tool_id":"t1","tool":"gbak","action":"backup"}]}`)
	case r.URL.Path == "/v1/dba/ops":
		write(200, `{"ops":[{"op":"db_sweep","title":"Sweep","class":"normal"},{"op":"db_shutdown","title":"Shut down the database","class":"disruptive"}],"groups":[]}`)
	case len(parts) == 5 && parts[2] == "ops" && parts[4] == "check":
		op := parts[3]
		s.checks[op]++
		class, blocked, extra := "normal", false, ""
		switch op {
		case "db_shutdown":
			class, extra = "disruptive", `,"confirm_token":"tok-sd"`
		case "db_mend":
			class, extra = "destructive", `,"confirm_token":"tok-m","confirm_name":"a.fdb"`
		case "fb_restart":
			class, extra = "critical", `,"confirm_token":"tok-r"`
		case "blocked_op":
			blocked = true
		case "stale":
			class, extra = s.staleClass, `,"confirm_token":"tok-`+s.staleClass+`-`+strconv.Itoa(s.checks[op])+`"`
		case "refused_check":
			write(409, `{"error":{"code":"conflict","message":"the operations are off"},"reason":"disabled"}`)
			return
		}
		level := "ok"
		if blocked {
			level = "block"
		}
		write(200, `{"op":"`+op+`","class":"`+class+`","database":"/data/a.fdb","checks":[{"level":"`+level+`","code":"c1","text":"check line of `+op+`"}],"blocked":`+boolStr(blocked)+
			`,"commands":[{"phase":"main","binary":"gfix","command":"gfix -`+op+` -user SYSDBA -password *** /data/a.fdb"}]`+extra+`}`)
	case len(parts) == 5 && parts[2] == "ops" && parts[4] == "run":
		op := parts[3]
		s.runs[op]++
		var body agentapi.DBARequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.bodies = append(s.bodies, body)
		if op == "stale" && s.runs[op] <= s.staleRefusals {
			write(409, `{"error":{"code":"conflict","message":"the confirm token is stale"},"reason":"confirm_stale"}`)
			return
		}
		state := "done"
		if op == "db_shutdown" {
			state = "running"
		}
		write(202, `{"run_id":"r-`+op+`","run_url":"/v1/dba/runs/r-`+op+`","run":{"run_id":"r-`+op+`","op":"`+op+`","state":"`+state+`","step_count":1}}`)
	case r.URL.Path == "/v1/dba/runs/r-db_shutdown" && r.Method == http.MethodGet:
		write(200, `{"run_id":"r-db_shutdown","op":"db_shutdown","state":"running","step":"main","step_index":1,"step_count":1}`)
	case r.URL.Path == "/v1/dba/runs/r-db_shutdown" && r.Method == http.MethodDelete:
		write(200, `{"run_id":"r-db_shutdown","state":"cancelled"}`)
	case strings.HasSuffix(r.URL.Path, "/output"):
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-Output-Bytes", "40")
		write(200, "first line\nlast line\n")
	default:
		write(404, `{"error":{"code":"not_found","message":"no route `+r.URL.Path+`"}}`)
	}
}

// counts returns the checks and runs of op.
func (s *dbaStub) counts(op string) (checks, runs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checks[op], s.runs[op]
}

// lastBody is the body of the last run request.
func (s *dbaStub) lastBody() agentapi.DBARequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bodies) == 0 {
		return agentapi.DBARequest{}
	}
	return s.bodies[len(s.bodies)-1]
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func newDBAGT(t *testing.T) (*gatedTools, *mcp.Server, *dbaStub) {
	t.Helper()
	gt := newTestGT(t)
	stub := &dbaStub{checks: map[string]int{}, runs: map[string]int{}, staleClass: "disruptive"}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	t.Setenv("FBMCP_TEST_AGENT_TOKEN", "tok9")
	cfg := gt.live()
	cfg.Agents = []config.Agent{{ID: "ag1", BaseURL: srv.URL, TokenEnv: "FBMCP_TEST_AGENT_TOKEN"}}
	gt.cfg = config.NewHandle(cfg)
	oldW, oldP := dbaQuickWait, dbaPollEvery
	dbaQuickWait, dbaPollEvery = 50*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { dbaQuickWait, dbaPollEvery = oldW, oldP })
	server := mcp.NewServer(&mcp.Implementation{Name: "dba-test", Version: "0"}, nil)
	registerAgentTools(server, gt)
	return gt, server, stub
}

func TestDBAClassTier(t *testing.T) {
	cases := []struct {
		class   string
		tier    int
		refused bool
	}{
		{"read", 1, false}, {"normal", 1, false}, {"disruptive", 2, false},
		{"destructive", 3, false}, {"critical", 0, true}, {"", 0, true}, {"other", 0, true},
	}
	for _, c := range cases {
		tier, msg := dbaClassTier(c.class)
		if tier != c.tier || (msg != "") != c.refused {
			t.Fatalf("class %q: tier=%d msg=%q", c.class, tier, msg)
		}
	}
	if _, msg := dbaClassTier("critical"); !contains(msg, "fb_agent_restart") {
		t.Fatalf("critical message = %q", msg)
	}
}

func TestDBARunGatePaths(t *testing.T) {
	gt, _, stub := newDBAGT(t)
	ctx := context.Background()

	// normal: Tier 1, in-band token; the impact has the title, the path,
	// the check line and the exact command.
	msg, denied := gt.requestAgentDBARun(ctx, agentDBARunArg{Op: "db_sweep", Database: "/data/a.fdb"})
	if denied || !contains(msg, "In-band token") {
		t.Fatalf("normal: denied=%v msg=%q", denied, msg)
	}
	for _, want := range []string{"Sweep (db_sweep)", "/data/a.fdb", "[ok] check line of db_sweep", "gfix -db_sweep -user SYSDBA -password ***", "Dynamic tier: 1"} {
		if !contains(msg, want) {
			t.Fatalf("normal impact misses %q:\n%s", want, msg)
		}
	}
	if contains(msg, "tok-") {
		t.Fatalf("the confirm token is shown:\n%s", msg)
	}

	// disruptive without a window on db: denied, with the check.
	msg, denied = gt.requestAgentDBARun(ctx, agentDBARunArg{Op: "db_shutdown", Database: "/data/a.fdb", Db: "nonexistent-db"})
	if !denied || !contains(msg, "no open maintenance window") || !contains(msg, "check line of db_shutdown") {
		t.Fatalf("disruptive w/o window: denied=%v msg=%q", denied, msg)
	}
	// disruptive with the spike5 window: pending, out of band.
	msg, denied = gt.requestAgentDBARun(ctx, agentDBARunArg{Op: "db_shutdown", Database: "/data/a.fdb", Db: "spike5"})
	if denied || contains(msg, "In-band token") || !contains(msg, "out-of-band") || !contains(msg, "Dynamic tier: 2") {
		t.Fatalf("disruptive w/ window: denied=%v msg=%q", denied, msg)
	}

	// destructive: Tier 3, denied.
	msg, denied = gt.requestAgentDBARun(ctx, agentDBARunArg{Op: "db_mend", Database: "/data/a.fdb", Db: "spike5"})
	if !denied || !contains(msg, "Tier 3") || !contains(msg, "a.fdb") {
		t.Fatalf("destructive: denied=%v msg=%q", denied, msg)
	}

	// critical: refused before the gate.
	before := len(gt.st.Pending())
	msg, denied = gt.requestAgentDBARun(ctx, agentDBARunArg{Op: "fb_restart"})
	if !denied || !contains(msg, "REFUSED") || !contains(msg, "fb_agent_restart") {
		t.Fatalf("critical: denied=%v msg=%q", denied, msg)
	}
	// blocked check: no pending, no run.
	msg, denied = gt.requestAgentDBARun(ctx, agentDBARunArg{Op: "blocked_op", Database: "/data/a.fdb"})
	if !denied || !contains(msg, "BLOCKED") || !contains(msg, "[block] check line of blocked_op") {
		t.Fatalf("blocked: denied=%v msg=%q", denied, msg)
	}
	// refused check: the reason is shown.
	msg, denied = gt.requestAgentDBARun(ctx, agentDBARunArg{Op: "refused_check"})
	if !denied || !contains(msg, "reason disabled") {
		t.Fatalf("refused check: denied=%v msg=%q", denied, msg)
	}
	if after := len(gt.st.Pending()); after != before {
		t.Fatalf("refused requests made pendings: %d -> %d", before, after)
	}
	// op is required.
	if _, denied := gt.requestAgentDBARun(ctx, agentDBARunArg{}); !denied {
		t.Fatal("empty op accepted")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.checks["db_sweep"] == 0 {
		t.Fatal("the request path did not call the check")
	}
	total := 0
	for _, n := range stub.runs {
		total += n
	}
	if total != 0 {
		t.Fatalf("the request path ran operations: %v", stub.runs)
	}
}

func TestDBARunExecutor(t *testing.T) {
	gt, _, stub := newDBAGT(t)
	ctx := context.Background()
	prog := func(float64, string) {}
	exec := gt.execs[agentDBARunTool]

	// A fast run: the final state and the last lines of the output.
	out, err := exec(ctx, "", map[string]any{
		"op": "db_sweep", "database": "/data/a.fdb", "class": "normal", "reason": "test",
		"_grant_identity": "tester", "_grant_request_id": "req-1",
	}, prog)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !contains(out, "state done") || !contains(out, "last line") || !contains(out, "fb_agent_dba_output") {
		t.Fatalf("sweep out = %q", out)
	}
	b := stub.lastBody()
	if b.RequestedBy != "tester" || b.RequestID != "req-1" || b.Reason != "test" || b.Database != "/data/a.fdb" {
		t.Fatalf("run body = %+v", b)
	}

	// A long run: the run id and how to follow it.
	out, err = exec(ctx, "spike5", map[string]any{
		"op": "db_shutdown", "database": "/data/a.fdb", "class": "disruptive", "confirm_token": "tok-sd", "_grant_identity": "tester",
	}, prog)
	if err != nil || !contains(out, "Still running") || !contains(out, "fb_agent_dba_runs") || !contains(out, "r-db_shutdown") {
		t.Fatalf("shutdown: %v %q", err, out)
	}
	if b := stub.lastBody(); b.ConfirmToken != "tok-sd" {
		t.Fatalf("token not sent: %+v", b)
	}
}

func TestDBARunRetryOnce(t *testing.T) {
	ctx := context.Background()
	prog := func(float64, string) {}
	args := func() map[string]any {
		return map[string]any{"op": "stale", "database": "/data/a.fdb", "class": "disruptive", "confirm_token": "old", "_grant_identity": "tester",
			"commands": "gfix -stale -user SYSDBA -password *** /data/a.fdb"}
	}

	// One refusal: check again, run again with the new token.
	gt, _, stub := newDBAGT(t)
	stub.staleRefusals = 1
	out, err := gt.execs[agentDBARunTool](ctx, "spike5", args(), prog)
	if err != nil || !contains(out, "r-stale") {
		t.Fatalf("retry: %v %q", err, out)
	}
	if c, r := stub.counts("stale"); c != 1 || r != 2 {
		t.Fatalf("checks=%d runs=%d", c, r)
	}
	if b := stub.lastBody(); b.ConfirmToken != "tok-disruptive-1" {
		t.Fatalf("retry token = %q", b.ConfirmToken)
	}

	// Two refusals: one retry only, then the refusal.
	gt, _, stub = newDBAGT(t)
	stub.staleRefusals = 2
	if _, err := gt.execs[agentDBARunTool](ctx, "spike5", args(), prog); err == nil || !contains(err.Error(), "confirm_stale") {
		t.Fatalf("two refusals: %v", err)
	}
	if _, r := stub.counts("stale"); r != 2 {
		t.Fatalf("runs = %d, want 2", r)
	}

	// The class changed on the new check: no second run.
	gt, _, stub = newDBAGT(t)
	stub.staleRefusals = 1
	stub.staleClass = "destructive"
	if _, err := gt.execs[agentDBARunTool](ctx, "spike5", args(), prog); err == nil || !contains(err.Error(), "class changed") {
		t.Fatalf("class change: %v", err)
	}
	if _, r := stub.counts("stale"); r != 1 {
		t.Fatalf("runs = %d, want 1", r)
	}

	// The commands changed on the new check: the approval does not cover
	// them, so there is no second run.
	gt, _, stub = newDBAGT(t)
	stub.staleRefusals = 1
	other := args()
	other["commands"] = "gfix -stale -user SYSDBA -password *** /data/other.fdb"
	if _, err := gt.execs[agentDBARunTool](ctx, "spike5", other, prog); err == nil || !contains(err.Error(), "commands changed") {
		t.Fatalf("command change: %v", err)
	}
	if _, r := stub.counts("stale"); r != 1 {
		t.Fatalf("runs = %d, want 1", r)
	}
}

func TestDBACancelExecutor(t *testing.T) {
	gt, _, _ := newDBAGT(t)
	prog := func(float64, string) {}
	out, err := gt.execs[agentDBACancelTool](context.Background(), "", map[string]any{"run_id": "r-db_shutdown"}, prog)
	if err != nil || !contains(out, "cancelled") {
		t.Fatalf("cancel: %v %q", err, out)
	}
	if _, err := gt.execs[agentDBACancelTool](context.Background(), "", map[string]any{}, prog); err == nil {
		t.Fatal("cancel without run_id accepted")
	}
}

func TestCapDBAOutput(t *testing.T) {
	text := "aaaa\nbbbb\ncccc\n"
	if s, cut := capDBAOutput(text, false, 100); cut || s != text {
		t.Fatalf("no cut: %q %v", s, cut)
	}
	if s, cut := capDBAOutput(text, false, 8); !cut || s != "aaaa\n" {
		t.Fatalf("start: %q %v", s, cut)
	}
	if s, cut := capDBAOutput(text, true, 8); !cut || s != "cccc\n" {
		t.Fatalf("end: %q %v", s, cut)
	}
}

func TestDBAReadToolsOverMCP(t *testing.T) {
	_, server, _ := newDBAGT(t)
	srvConn, cliConn := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go server.Run(ctx, &mcp.IOTransport{Reader: srvConn, Writer: srvConn})
	defer cliConn.Close()
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "dba-test-client", Version: "0"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: cliConn, Writer: cliConn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	call := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		tc, _ := res.Content[0].(*mcp.TextContent)
		return tc.Text, res.IsError
	}
	if s, isErr := call("fb_agent_tools_running", map[string]any{}); isErr || !contains(s, "gbak") {
		t.Fatalf("tools_running: %v %q", isErr, s)
	}
	if s, isErr := call("fb_agent_dba_check", map[string]any{"op": "db_shutdown", "database": "/data/a.fdb"}); isErr || !contains(s, "Shut down the database (db_shutdown)") || !contains(s, "Tier 2") || contains(s, "tok-sd") {
		t.Fatalf("check: %v %q", isErr, s)
	}
	if s, isErr := call("fb_agent_dba_check", map[string]any{"op": "refused_check"}); !isErr || !contains(s, "reason disabled") {
		t.Fatalf("refused check: %v %q", isErr, s)
	}
	if s, isErr := call("fb_agent_dba_output", map[string]any{"run_id": "r-db_sweep", "tail": 5}); isErr || !contains(s, "last line") || !contains(s, "output file: 40 bytes") {
		t.Fatalf("output: %v %q", isErr, s)
	}
	if s, isErr := call("fb_agent_dba_database", map[string]any{"database": " "}); !isErr || !contains(s, "database is required") {
		t.Fatalf("database without path: %v %q", isErr, s)
	}
	if s, isErr := call("fb_agent_dba_run", map[string]any{"op": "fb_restart"}); !isErr || !contains(s, "fb_agent_restart") {
		t.Fatalf("critical over MCP: %v %q", isErr, s)
	}
}
