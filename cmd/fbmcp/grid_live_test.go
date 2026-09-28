package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/dbpool"
	execpkg "github.com/aleks/fbmcp/internal/executor"
	"github.com/aleks/fbmcp/internal/facts"
	"github.com/aleks/fbmcp/internal/gate"
	"github.com/aleks/fbmcp/internal/jobs"
	"github.com/aleks/fbmcp/internal/policy"
	"github.com/aleks/fbmcp/internal/qlog"
	"github.com/aleks/fbmcp/internal/state"
	"github.com/aleks/fbmcp/internal/uigrid"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestGridLiveMCP drives the phase10 surfaces against the machine-local dev
// config: fb_query stores a grid payload and returns result_id + _meta.ui;
// the resource read renders the HTML grid; fb_query_check compile-validates
// (valid draft ok, typo'd draft sql-object-missing, bind-param note).
// Opt-in, like the query live smoke:
//
//	FBMCP_GRID_LIVE=1 go test ./cmd/fbmcp -run GridLive -v
func TestGridLiveMCP(t *testing.T) {
	if os.Getenv("FBMCP_GRID_LIVE") == "" {
		t.Skip("set FBMCP_GRID_LIVE=1 to run")
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller info")
	}
	cfg, err := config.Load(filepath.Join(filepath.Dir(thisFile), "..", "..", "fbmcp.dev.yaml"))
	if err != nil {
		t.Skipf("dev config not loadable: %v", err)
	}
	tmp := t.TempDir()
	aud, err := audit.Open(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer aud.Close()
	ql, err := qlog.Open(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer ql.Close()
	st, err := state.Open(tmp)
	if err != nil {
		t.Fatal(err)
	}
	handle := config.NewHandle(cfg)
	pools := dbpool.NewManager(handle)
	defer pools.Close()
	engFacts := facts.NewEngineFacts(handle, pools)
	eng := policy.New(toolMeta, state.CompositeFacts{engFacts}, st)
	gridCfg := cfg.Grid.OrDefault()
	gt := &gatedTools{cfg: handle, pools: pools, eng: eng, g: gate.New(st, aud),
		runner: jobs.NewRunner(st), aud: aud, qlog: ql, st: st,
		execSvc: &execpkg.Service{Pools: pools}, execs: map[string]executor{}, args: map[string]map[string]any{},
		grid: uigrid.New(uigrid.RegistryOpts{
			Enabled: gridCfg.Enabled != nil && *gridCfg.Enabled,
			TTL:     time.Duration(gridCfg.TTLMinutes) * time.Minute,
			MaxLive: gridCfg.MaxLive,
		})}

	server := mcp.NewServer(&mcp.Implementation{Name: "fbmcp-grid-test", Version: "0"}, nil)
	registerP4Tools(server, gt)
	uigrid.RegisterResource(server, gt.grid, aud)

	srvConn, cliConn := net.Pipe()
	go server.Run(context.Background(), &mcp.IOTransport{Reader: srvConn, Writer: srvConn})
	defer cliConn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "grid-live", Version: "0"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: cliConn, Writer: cliConn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// 1. fb_query → result_id + _meta.ui + a fetchable text/html resource
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "fb_query", Arguments: map[string]any{
		"db": "employee", "sql": "SELECT EMP_NO, FULL_NAME FROM EMPLOYEE ROWS 1 TO 5",
	}})
	if err != nil {
		t.Fatalf("fb_query: %v", err)
	}
	if res.IsError {
		t.Fatalf("fb_query errored: %+v", res.Content)
	}
	str, _ := res.StructuredContent.(map[string]any)
	if str == nil {
		t.Fatalf("no structuredContent: %+v", res)
	}
	gid, _ := str["result_id"].(string)
	if gid == "" {
		t.Fatalf("no result_id in structuredContent: %+v", str["result_id"])
	}
	colsJSON, _ := jsonMarshalCols(str["columns"])
	if !strings.Contains(colsJSON, `"type"`) {
		t.Fatalf("columns lack types: %s", colsJSON)
	}
	rr, err := sess.ReadResource(ctx, &mcp.ReadResourceParams{URI: uigrid.URI(gid)})
	if err != nil {
		t.Fatalf("grid resource read: %v", err)
	}
	page := rr.Contents[0].Text
	for _, want := range []string{"Content-Security-Policy", "employee:", "__GRID_DATA__", "FULL_NAME"} {
		if !strings.Contains(page, want) {
			t.Fatalf("grid page missing %q", want)
		}
	}

	// 2. fb_query_check — a valid draft compiles, never executes
	res, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: "fb_query_check", Arguments: map[string]any{
		"db": "employee", "sql": "SELECT EMP_NO FROM EMPLOYEE WHERE EMP_NO > 0",
	}})
	if err != nil || res.IsError {
		t.Fatalf("check valid draft: err=%v res=%+v", err, res)
	}
	if body := textOf(res); !strings.Contains(body, "kind: select") || !strings.Contains(body, "never executed") {
		t.Fatalf("check body: %q", body)
	}

	// 3. typo'd draft → sql-object-missing envelope in one response
	res, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: "fb_query_check", Arguments: map[string]any{
		"db": "employee", "sql": "SELECT NOSUCHCOL FROM EMPLOYEE",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("typo draft must error")
	}
	if str, _ := res.StructuredContent.(map[string]any); str == nil || str["code"] != "sql-object-missing" {
		t.Fatalf("typo draft code = %+v, want sql-object-missing", str)
	}
	if body := textOf(res); !strings.Contains(body, "compile error") {
		t.Fatalf("typo draft text: %q", body)
	}

	// 4. bind params: classifyRead does NOT refuse '?' (fb_query would hit
	// the engine and fail at execute); the check tool reports the count so
	// the builder loop catches it before execution. The engine may prepare
	// the draft (→ bind_params note) or refuse it (→ sql-compile).
	res, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: "fb_query_check", Arguments: map[string]any{
		"db": "employee", "sql": "SELECT EMP_NO FROM EMPLOYEE WHERE EMP_NO = ?",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		str, _ := res.StructuredContent.(map[string]any)
		if str == nil || str["code"] != "sql-compile" {
			t.Fatalf("bind-param draft refused with unexpected envelope: %+v", str)
		}
	} else {
		str, _ := res.StructuredContent.(map[string]any)
		if str == nil || str["bind_params"] != float64(1) {
			t.Fatalf("bind_params = %+v, want 1", str["bind_params"])
		}
		if !strings.Contains(textOf(res), "inline literal values") {
			t.Fatalf("bind-param note missing: %q", textOf(res))
		}
	}

	// 5. compile-only guard (§7 "Check is compile-only", §11 D4): a
	// selectable procedure whose body logs a row must NOT run under
	// fb_query_check (prepare ≠ execute) — log rows stay 0 — while the
	// same procedure really does log when executed on the writable admin
	// pool, proving the harness can tell the difference. (fb_query on it
	// is refused by the RO transaction by design — the mutating body trips
	// the fuse and routes to fb_write — so the contrast runs out-of-band.)
	admin, err := pools.AdminPool(ctx, "employee")
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE TABLE PH10_GRIDCHK_LOG (N INTEGER)"); err != nil {
		t.Fatalf("setup log table: %v", err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE PROCEDURE PH10_GRIDCHK_SIDE_EFFECT RETURNS (N INTEGER) AS BEGIN INSERT INTO PH10_GRIDCHK_LOG VALUES (1); N = 1; SUSPEND; END`); err != nil {
		t.Fatalf("setup procedure: %v", err)
	}
	// a mid-test failure must still clean the dev DB even after this
	// test's own pools.Close() defer has run — hence a self-contained
	// manager; the explicit call below makes the happy path deterministic.
	dropObjects := func() {
		m2 := dbpool.NewManager(handle)
		defer m2.Close()
		cctx, ccancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer ccancel()
		if a, err := m2.AdminPool(cctx, "employee"); err == nil {
			_, _ = a.ExecContext(cctx, "DROP PROCEDURE PH10_GRIDCHK_SIDE_EFFECT")
			_, _ = a.ExecContext(cctx, "DROP TABLE PH10_GRIDCHK_LOG")
		}
	}
	t.Cleanup(dropObjects)
	pools.CloseRead("employee") // drain the RO attachment: nothing must pin the DROP
	logRows := func() int {
		var n int
		if err := admin.QueryRowContext(ctx, "SELECT COUNT(*) FROM PH10_GRIDCHK_LOG").Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	res, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: "fb_query_check", Arguments: map[string]any{
		"db": "employee", "sql": "SELECT N FROM PH10_GRIDCHK_SIDE_EFFECT"}})
	if err != nil || res.IsError {
		t.Fatalf("check selectable proc: err=%v res=%+v", err, res)
	}
	if n := logRows(); n != 0 {
		t.Fatalf("procedure body ran under prepare: log rows = %d, want 0", n)
	}
	if _, err := admin.ExecContext(ctx, "EXECUTE PROCEDURE PH10_GRIDCHK_SIDE_EFFECT"); err != nil {
		t.Fatalf("contrast exec on admin pool: %v", err)
	}
	if n := logRows(); n != 1 {
		t.Fatalf("contrast run: log rows = %d, want 1", n)
	}
	dropObjects()
}

func textOf(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func jsonMarshalCols(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
