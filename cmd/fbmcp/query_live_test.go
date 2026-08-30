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

	"github.com/modelcontextprotocol/go-sdk/mcp"

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
)

// TestQueryLiveMCP drives fb_query through the full MCP dispatch path against
// the machine-local dev config: SELECT with per-table stats and explained
// plan, a read-only EXECUTE PROCEDURE, denials, the fb_write fallback for a
// mutating procedure, and the query-log.jsonl telemetry. Opt-in, like the
// gstat live smoke:
//
//	FBMCP_DEV_PW=… FBMCP_QUERY_LIVE=1 go test ./cmd/fbmcp -run QueryLive -v
func TestQueryLiveMCP(t *testing.T) {
	if os.Getenv("FBMCP_QUERY_LIVE") == "" {
		t.Skip("set FBMCP_QUERY_LIVE=1 to run")
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
	gt := &gatedTools{cfg: handle, pools: pools, eng: eng, g: gate.New(st, aud),
		runner: jobs.NewRunner(st), aud: aud, qlog: ql, st: st,
		execSvc: &execpkg.Service{Pools: pools}, execs: map[string]executor{}, args: map[string]map[string]any{}}

	server := mcp.NewServer(&mcp.Implementation{Name: "fbmcp-test", Version: "0"}, nil)
	registerP4Tools(server, gt)

	srvConn, cliConn := net.Pipe()
	go server.Run(context.Background(), &mcp.IOTransport{Reader: srvConn, Writer: srvConn})
	defer cliConn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "query-live", Version: "0"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: cliConn, Writer: cliConn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	call := func(args map[string]any) *mcp.CallToolResult {
		t.Helper()
		cctx, ccancel := context.WithTimeout(ctx, 90*time.Second)
		defer ccancel()
		res, err := sess.CallTool(cctx, &mcp.CallToolParams{Name: "fb_query", Arguments: args})
		if err != nil {
			t.Fatalf("fb_query %v: %v", args, err)
		}
		return res
	}
	body := func(res *mcp.CallToolResult) string {
		t.Helper()
		var b strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return b.String()
	}

	t0 := time.Now()
	mark := func(s string) { t.Logf("[%6.1fs] %s", time.Since(t0).Seconds(), s) }
	mark("session connected")

	// A mutating procedure for the fallback case; created/dropped via the
	// admin pool so the test is self-contained.
	admin := func(sql string) {
		t.Helper()
		pool, err := pools.AdminPool(ctx, "employee")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, sql); err != nil {
			t.Fatalf("admin %q: %v", sql, err)
		}
		mark("admin ok: " + sql)
	}
	admin("CREATE TABLE FBMCP_QTEST_TBL (N INT)")
	admin("CREATE PROCEDURE FBMCP_QTEST_RW_PROC AS BEGIN INSERT INTO FBMCP_QTEST_TBL VALUES (1); END")
	defer func() {
		mark("cleanup start")
		admin("DROP PROCEDURE FBMCP_QTEST_RW_PROC")
		admin("DROP TABLE FBMCP_QTEST_TBL")
		mark("cleanup done")
	}()

	// 1. SELECT join: rows, plan, per-table stats, stats line (the join
	//    yields 92 rows — under the default cap of 100).
	mark("step1 start")
	res := call(map[string]any{"db": "employee",
		"sql": "SELECT EP.EMP_NO, E.FULL_NAME FROM EMPLOYEE_PROJECT EP JOIN EMPLOYEE E ON E.EMP_NO = EP.EMP_NO"})
	out := body(res)
	if res.IsError {
		t.Fatalf("SELECT failed:\n%s", out)
	}
	for _, want := range []string{"EMP_NO | FULL_NAME", "rows: 92\n", "plan:", "per-table:", "EMPLOYEE_PROJECT", "EMPLOYEE", "stats:", "elapsed:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("SELECT output missing %q:\n%.600s", want, out)
		}
	}

	// 1b. same query capped: max_rows must truncate with a note.
	res = call(map[string]any{"db": "employee", "max_rows": 5,
		"sql": "SELECT EP.EMP_NO, E.FULL_NAME FROM EMPLOYEE_PROJECT EP JOIN EMPLOYEE E ON E.EMP_NO = EP.EMP_NO"})
	if out := body(res); !strings.Contains(out, "rows: 5 (truncated") {
		t.Fatalf("max_rows cap not applied:\n%.300s", out)
	}

	// 2. read-only EXECUTE PROCEDURE (employee's DEPT_BUDGET sums a budget).
	mark("steps 1/1b done")
	res = call(map[string]any{"db": "employee", "sql": "EXECUTE PROCEDURE DEPT_BUDGET(100)"})
	if out := body(res); res.IsError || !strings.Contains(out, "rows: 1") {
		t.Fatalf("read procedure failed:\n%s", out)
	}

	// 3. mutation refused by the acceptance gate.
	mark("step2 done")
	res = call(map[string]any{"db": "employee", "sql": "INSERT INTO EMPLOYEE_PROJECT (EMP_NO, PROJ_ID) VALUES (999, 'XXX')"})
	if out := body(res); !res.IsError || !strings.Contains(out, "DENIED") || !strings.Contains(out, "fb_write") {
		t.Fatalf("mutation not denied:\n%s", out)
	}

	// 4. mutating procedure: engine refuses on the RO transaction, fb_query
	//    routes it into the fb_write gated flow (Tier 1 → in-band token).
	mark("step3 done")
	res = call(map[string]any{"db": "employee", "sql": "EXECUTE PROCEDURE FBMCP_QTEST_RW_PROC"})
	out = body(res)
	if res.IsError {
		t.Fatalf("fallback errored instead of routing to fb_write:\n%s", out)
	}
	for _, want := range []string{"read-only execution failed", "routed to fb_write gated flow", "In-band token"} {
		if !strings.Contains(out, want) {
			t.Fatalf("fallback output missing %q:\n%.600s", want, out)
		}
	}

	// 5. query-log.jsonl: one valid NDJSON line per call with the promised fields.
	mark("step4 done")
	lb, err := os.ReadFile(filepath.Join(tmp, "query-log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(lb)), "\n")
	if len(lines) != 5 {
		t.Fatalf("want 5 query-log lines, got %d", len(lines))
	}
	var okSel, okFallback, okDenied bool
	for _, l := range lines {
		var e map[string]any
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("query-log line not JSON: %v\n%s", err, l)
		}
		switch e["outcome"] {
		case "ok":
			if strings.Contains(e["query"].(string), "EMPLOYEE_PROJECT") {
				okSel = true
				if e["plan"] == nil || e["plan"].(string) == "" {
					t.Fatal("ok entry missing plan")
				}
				pts, _ := e["per_table_stats"].([]any)
				if len(pts) == 0 {
					t.Fatal("ok entry missing per_table_stats on FB5")
				}
				seen := map[string]bool{}
				for _, p := range pts {
					tbl := p.(map[string]any)["table"].(string)
					if seen[tbl] {
						t.Fatalf("per_table_stats duplicate: %s", tbl)
					}
					seen[tbl] = true
				}
				st, _ := e["stats"].(map[string]any)
				if st == nil || st["seq_reads"].(float64) <= 0 {
					t.Fatal("ok entry missing stats")
				}
				if e["engine"] != "5.0" {
					t.Fatalf("engine field = %v, want 5.0", e["engine"])
				}
			}
		case "fallback":
			okFallback = true
			if !strings.Contains(e["error"].(string), "read-only") {
				t.Fatalf("fallback entry error = %v", e["error"])
			}
		case "denied":
			okDenied = true
		}
	}
	if !okSel || !okFallback || !okDenied {
		t.Fatalf("query-log outcomes incomplete: sel=%v fallback=%v denied=%v", okSel, okFallback, okDenied)
	}
}
