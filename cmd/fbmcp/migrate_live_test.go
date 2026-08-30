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
	execpkg "github.com/aleks/fbmcp/internal/executor"
	"github.com/aleks/fbmcp/internal/facts"
	"github.com/aleks/fbmcp/internal/gate"
	"github.com/aleks/fbmcp/internal/jobs"
	"github.com/aleks/fbmcp/internal/migrate"
	"github.com/aleks/fbmcp/internal/policy"
	"github.com/aleks/fbmcp/internal/qlog"
	"github.com/aleks/fbmcp/internal/state"
)

// TestMigrateLiveMCP drives the C.1 surface end-to-end on spike3 through the
// full MCP dispatch path with a temp migrations directory (same content as
// examples/migrations): status → plan → apply pending → tamper → manifest
// re-validation failure → clean apply → verify tables + history →
// rollback_plan rendering. Opt-in:
//
//	FBMCP_DEV_PW=… FBMCP_MIGRATE_LIVE=1 go test ./cmd/fbmcp -run MigrateLive -v
func TestMigrateLiveMCP(t *testing.T) {
	if os.Getenv("FBMCP_MIGRATE_LIVE") == "" {
		t.Skip("set FBMCP_MIGRATE_LIVE=1 to run")
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller info")
	}
	cfg, err := config.Load(filepath.Join(filepath.Dir(thisFile), "..", "..", "fbmcp.dev.yaml"))
	if err != nil {
		t.Skipf("dev config not loadable: %v", err)
	}
	migDir := t.TempDir()
	const file001 = `CREATE TABLE FBMCP_MIG_LIVE_A (ID INTEGER NOT NULL PRIMARY KEY, NAME VARCHAR(80) NOT NULL);
INSERT INTO FBMCP_MIG_LIVE_A (ID, NAME) VALUES (1, 'live one');
-- @down
DROP TABLE FBMCP_MIG_LIVE_A;
`
	const file002 = `CREATE TABLE FBMCP_MIG_LIVE_B (ID INTEGER NOT NULL PRIMARY KEY, A_ID INTEGER NOT NULL REFERENCES FBMCP_MIG_LIVE_A (ID));
-- @down
DROP TABLE FBMCP_MIG_LIVE_B;
`
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(migDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("001_live_a.sql", file001)
	write("002_live_b.sql", file002)
	for i := range cfg.Databases {
		if cfg.Databases[i].ID == "spike3" {
			cfg.Databases[i].MigrationsDir = filepath.ToSlash(migDir)
		}
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
	runner := jobs.NewRunner(st)
	gt := &gatedTools{cfg: handle, pools: pools, eng: eng, g: gate.New(st, aud),
		runner: runner, aud: aud, qlog: ql, st: st,
		execSvc: &execpkg.Service{Pools: pools}, execs: map[string]executor{}, args: map[string]map[string]any{}}

	server := mcp.NewServer(&mcp.Implementation{Name: "fbmcp-test", Version: "0"}, nil)
	registerP4Tools(server, gt)

	srvConn, cliConn := net.Pipe()
	go server.Run(context.Background(), &mcp.IOTransport{Reader: srvConn, Writer: srvConn})
	defer cliConn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "migrate-live", Version: "0"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: cliConn, Writer: cliConn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	call := func(tool string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		cctx, ccancel := context.WithTimeout(ctx, 90*time.Second)
		defer ccancel()
		res, err := sess.CallTool(cctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatalf("%s %v: %v", tool, args, err)
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
		pool, err := pools.AdminPool(ctx, "spike3")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, sql); err != nil {
			t.Fatalf("admin %q: %v", sql, err)
		}
	}
	// self-cleaning: tolerate leftovers from an aborted previous run
	if pool, err := pools.AdminPool(ctx, "spike3"); err == nil {
		_, _ = pool.ExecContext(ctx, "DROP TABLE FBMCP_MIG_LIVE_B")
		_, _ = pool.ExecContext(ctx, "DROP TABLE FBMCP_MIG_LIVE_A")
		_, _ = pool.ExecContext(ctx, "DROP TABLE "+migrate.Table)
	}
	defer func() { // best-effort cleanup, never masks the test result
		_ = admin
		if pool, err := pools.AdminPool(ctx, "spike3"); err == nil {
			_, _ = pool.ExecContext(ctx, "DROP TABLE FBMCP_MIG_LIVE_B")
			_, _ = pool.ExecContext(ctx, "DROP TABLE FBMCP_MIG_LIVE_A")
			_, _ = pool.ExecContext(ctx, "DROP TABLE "+migrate.Table)
		}
	}()

	// confirmAndRun confirms a pending in-band and waits for the job.
	confirmAndRun := func(out string) state.Job {
		t.Helper()
		idRe := regexp.MustCompile(`Request ID: (\S+)`)
		m := idRe.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("no request id in:\n%.900s", out)
		}
		reqID := m[1]
		tokRe := regexp.MustCompile(`In-band token \(Tier 1 only\): (\S+)`)
		tm := tokRe.FindStringSubmatch(out)
		if tm == nil {
			t.Fatalf("no in-band token (batch not Tier 1?):\n%.900s", out)
		}
		var p state.PendingAction
		for _, cand := range st.Pending() {
			if cand.ID == reqID {
				p = cand
			}
		}
		if p.ID == "" {
			t.Fatalf("pending %s not found", reqID)
		}
		if _, err := gt.g.Confirm(reqID, p.Identity, "in-band-token", tm[1]); err != nil {
			t.Fatalf("confirm: %v", err)
		}
		jobID, err := gt.dispatch(p)
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		deadline := time.Now().Add(60 * time.Second)
		for {
			j, ok := st.Job(jobID)
			if !ok {
				t.Fatalf("job %s not found", jobID)
			}
			if j.State == "succeeded" || j.State == "failed" || j.State == "cancelled" || j.State == "interrupted" {
				return j
			}
			if time.Now().After(deadline) {
				t.Fatalf("job %s stuck in %s", jobID, j.State)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	// 1. status: uninitialized, both pending.
	res := call("fb_migration_status", map[string]any{"db": "spike3"})
	out := body(res)
	if res.IsError || !strings.Contains(out, "not initialized") || !strings.Contains(out, "pending") {
		t.Fatalf("status:\n%.900s", out)
	}

	// 2. plan: batch tier 1, two files, per-statement lines.
	res = call("fb_migration_plan", map[string]any{"db": "spike3"})
	out = body(res)
	if res.IsError || !strings.Contains(out, "batch tier: 1") || !strings.Contains(out, "001_live_a.sql") || !strings.Contains(out, "002_live_b.sql") || !strings.Contains(out, "checksum") {
		t.Fatalf("plan:\n%.1200s", out)
	}

	// 3. apply → pending; 4. tamper with 002 before confirming.
	res = call("fb_migration_apply", map[string]any{"db": "spike3"})
	out = body(res)
	if res.IsError || !strings.Contains(out, "In-band token") {
		t.Fatalf("apply request:\n%.1200s", out)
	}
	write("002_live_b.sql", file002+"-- tampered\n")
	j := confirmAndRun(out)
	if j.State != "failed" || !strings.Contains(j.Message, "migrations changed after confirmation") {
		t.Fatalf("tampered batch should fail re-validation, got job %s: %s", j.State, j.Message)
	}

	// 5. restore, re-request, confirm, run.
	write("002_live_b.sql", file002)
	res = call("fb_migration_apply", map[string]any{"db": "spike3"})
	out = body(res)
	if res.IsError {
		t.Fatalf("re-apply:\n%.1200s", out)
	}
	j = confirmAndRun(out)
	if j.State != "succeeded" {
		t.Fatalf("apply job failed: %s", j.Message)
	}

	// 6. verify: tables + rows + history.
	tx, err := pools.ReadOnly(ctx, "spike3")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM FBMCP_MIG_LIVE_A").Scan(&n); err != nil || n != 1 {
		t.Fatalf("seed row missing (n=%d err=%v)", n, err)
	}
	if _, err := tx.QueryContext(ctx, "SELECT 1 FROM FBMCP_MIG_LIVE_B WHERE 1=0"); err != nil {
		t.Fatalf("table B missing: %v", err)
	}
	rows, err := tx.QueryContext(ctx, "SELECT VERSION FROM "+migrate.Table+" ORDER BY VERSION")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var vers []int
	for rows.Next() {
		var v int
		_ = rows.Scan(&v)
		vers = append(vers, v)
	}
	rows.Close()
	tx.Rollback()
	if len(vers) != 2 || vers[0] != 1 || vers[1] != 2 {
		t.Fatalf("history versions = %v, want [1 2]", vers)
	}

	// 7. re-apply: nothing pending.
	res = call("fb_migration_apply", map[string]any{"db": "spike3"})
	out = body(res)
	if !res.IsError || !strings.Contains(out, "nothing to apply") {
		t.Fatalf("idempotent apply should refuse:\n%.600s", out)
	}

	// 8. rollback_plan: default is ONE step down (v2 → v1): only v2's down.
	res = call("fb_migration_rollback_plan", map[string]any{"db": "spike3"})
	out = body(res)
	if res.IsError || !strings.Contains(out, "DROP TABLE FBMCP_MIG_LIVE_B") || strings.Contains(out, "DROP TABLE FBMCP_MIG_LIVE_A") {
		t.Fatalf("one-step rollback plan:\n%.1200s", out)
	}

	// 9. explicit to_version=0 renders the full chain.
	res = call("fb_migration_rollback_plan", map[string]any{"db": "spike3", "to_version": 0})
	out = body(res)
	if res.IsError || !strings.Contains(out, "DROP TABLE FBMCP_MIG_LIVE_B") || !strings.Contains(out, "DROP TABLE FBMCP_MIG_LIVE_A") || !strings.Contains(out, "fb_write") {
		t.Fatalf("full rollback plan:\n%.1200s", out)
	}
}
