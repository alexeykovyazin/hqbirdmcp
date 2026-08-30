package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/dbpool"
	"github.com/aleks/fbmcp/internal/facts"
	"github.com/aleks/fbmcp/internal/state"
)

// TestAdviceLiveMCP drives fb_index_advice through the full MCP dispatch
// path against the machine-local dev config: a natural scan gets a proposed
// index, applying it (admin pool, standing in for a confirmed fb_write)
// suppresses the advice via the covering-index check, and recheck_of reports
// the scan resolved. Opt-in:
//
//	FBMCP_DEV_PW=… FBMCP_ADVICE_LIVE=1 go test ./cmd/fbmcp -run AdviceLive -v
func TestAdviceLiveMCP(t *testing.T) {
	if os.Getenv("FBMCP_ADVICE_LIVE") == "" {
		t.Skip("set FBMCP_ADVICE_LIVE=1 to run")
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
	st, err := state.Open(tmp)
	if err != nil {
		t.Fatal(err)
	}
	handle := config.NewHandle(cfg)
	pools := dbpool.NewManager(handle)
	defer pools.Close()
	engFacts := facts.NewEngineFacts(handle, pools)

	server := mcp.NewServer(&mcp.Implementation{Name: "fbmcp-test", Version: "0"}, nil)
	registerP2Tools(server, handle, pools, engFacts, aud, st)

	srvConn, cliConn := net.Pipe()
	go server.Run(context.Background(), &mcp.IOTransport{Reader: srvConn, Writer: srvConn})
	defer cliConn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "advice-live", Version: "0"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: cliConn, Writer: cliConn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	call := func(args map[string]any) *mcp.CallToolResult {
		t.Helper()
		cctx, ccancel := context.WithTimeout(ctx, 90*time.Second)
		defer ccancel()
		res, err := sess.CallTool(cctx, &mcp.CallToolParams{Name: "fb_index_advice", Arguments: args})
		if err != nil {
			t.Fatalf("fb_index_advice %v: %v", args, err)
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

	admin := func(sql string) {
		t.Helper()
		pool, err := pools.AdminPool(ctx, "employee")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, sql); err != nil {
			t.Fatalf("admin %q: %v", sql, err)
		}
	}

	// self-cleaning: tolerate leftovers from an aborted previous run
	if pool, err := pools.AdminPool(ctx, "employee"); err == nil {
		_, _ = pool.ExecContext(ctx, "DROP TABLE FBMCP_ADVICE_TBL")
	}
	admin("CREATE TABLE FBMCP_ADVICE_TBL (ID INT PRIMARY KEY, FLTR INT)")
	admin(`INSERT INTO FBMCP_ADVICE_TBL (ID, FLTR)
		WITH RECURSIVE SEQ(N) AS (SELECT 1 FROM RDB$DATABASE UNION ALL SELECT N + 1 FROM SEQ WHERE N < 50)
		SELECT N, MOD(N, 5) FROM SEQ`)
	defer admin("DROP TABLE FBMCP_ADVICE_TBL")

	query := "SELECT * FROM FBMCP_ADVICE_TBL WHERE FLTR = 3"

	// 1. natural scan → proposed index DDL, advisory id recorded.
	res := call(map[string]any{"db": "employee", "query": query})
	out := body(res)
	if res.IsError {
		t.Fatalf("advice failed:\n%s", out)
	}
	for _, want := range []string{"PLAN", "NATURAL", "CREATE INDEX", "ON FBMCP_ADVICE_TBL (FLTR)", "estimate only", "apply via fb_write"} {
		if !strings.Contains(out, want) {
			t.Fatalf("advice output missing %q:\n%.900s", want, out)
		}
	}
	advRe := regexp.MustCompile(`ADVISORY id=(adv\d+)`)
	m := advRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no advisory id in output:\n%.900s", out)
	}
	advID := m[1]

	// 2. non-SELECT is refused.
	res = call(map[string]any{"db": "employee", "query": "UPDATE FBMCP_ADVICE_TBL SET FLTR = 4 WHERE FLTR = 3"})
	out = body(res)
	if !res.IsError || !strings.Contains(out, "exactly one plain SELECT") {
		t.Fatalf("non-SELECT should be refused:\n%.400s", out)
	}

	// 3. apply the index (admin pool stands in for a confirmed fb_write) —
	//    the covering-index check must now suppress the advice.
	admin("CREATE INDEX FBMCP_ADVICE_IX ON FBMCP_ADVICE_TBL (FLTR)")
	res = call(map[string]any{"db": "employee", "query": query})
	out = body(res)
	if res.IsError {
		t.Fatalf("post-apply advice failed:\n%s", out)
	}
	if strings.Contains(out, "CREATE INDEX IDX_ADVICE") {
		t.Fatalf("advice persisted after applying an equivalent index:\n%.900s", out)
	}

	// 4. recheck: the recorded natural scan must be reported resolved.
	res = call(map[string]any{"db": "employee", "recheck_of": advID})
	out = body(res)
	if res.IsError {
		t.Fatalf("recheck failed:\n%s", out)
	}
	if !strings.Contains(out, "resolved: FBMCP_ADVICE_TBL no longer scanned naturally") {
		t.Fatalf("recheck did not report resolution:\n%.900s", out)
	}
}
