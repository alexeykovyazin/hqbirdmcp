package main

import (
	"context"
	"database/sql"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/backupsvc"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/dbpool"
	"github.com/aleks/fbmcp/internal/facts"
	"github.com/aleks/fbmcp/internal/state"
)

// TestDiffLiveMCP (C.3): builds two divergent scratch databases via isql,
// registers them in-memory against the dev fb3 instance, and drives
// fb_diff_schema (two-db, snapshot save, snapshot-vs-now drift) and
// fb_diff_data (counts, samples, cap refusal) through the MCP path.
// Opt-in:
//
//	FBMCP_DEV_PW=… FBMCP_DIFF_LIVE=1 go test ./cmd/fbmcp -run DiffLive -v
func TestDiffLiveMCP(t *testing.T) {
	if os.Getenv("FBMCP_DIFF_LIVE") == "" {
		t.Skip("set FBMCP_DIFF_LIVE=1 to run")
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller info")
	}
	cfg, err := config.Load(filepath.Join(filepath.Dir(thisFile), "..", "..", "fbmcp.dev.yaml"))
	if err != nil {
		t.Skipf("dev config not loadable: %v", err)
	}
	var inst config.FBInstance
	for _, i := range cfg.Instances {
		if i.ID == "fb3" {
			inst = i
		}
	}
	if inst.ID == "" {
		t.Skip("fb3 instance not in dev config")
	}
	// Derive both scratch copies from spike3 through the Services API
	// (same engine => matching ODS; isql DSN parsing proved unreliable on
	// this host). Seed divergence via the driver.
	pw := pwFor(t)
	client := backupsvc.NewClient(inst, "SYSDBA", pw)
	fbk := `C:/HQbirdData/output/fbmcp-spike/work/fbmcp_diff_src.fbk`
	pathA := `C:/HQbirdData/output/fbmcp-spike/work/fbmcp_diff_a.fdb`
	pathB := `C:/HQbirdData/output/fbmcp-spike/work/fbmcp_diff_b.fdb`
	removeLocal := func(p string) {
		os.Remove(p)
		os.Remove(filepath.FromSlash(p))
	}
	removeLocal(fbk)
	removeLocal(pathA)
	removeLocal(pathB)
	if err := client.Backup(spike3Path(cfg), fbk, 0, func(string) {}); err != nil {
		t.Fatalf("backup spike3: %v", err)
	}
	t.Cleanup(func() {
		removeLocal(fbk)
		removeLocal(pathA)
		removeLocal(pathB)
	})
	if err := client.Restore(fbk, pathA, true, 0, func(string) {}); err != nil {
		t.Fatalf("restore A: %v", err)
	}
	if err := client.Restore(fbk, pathB, true, 0, func(string) {}); err != nil {
		t.Fatalf("restore B: %v", err)
	}

	seed := func(path string, stmts []string, tolerate bool) {
		t.Helper()
		db, err := sql.Open("firebirdsql", dbpool.DSN(inst.Addr, path, "SYSDBA", pw))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for _, s := range stmts {
			if _, err := db.ExecContext(ctx, s); err != nil && !tolerate {
				t.Fatalf("%s: %q: %v", filepath.Base(path), s, err)
			}
		}
	}
	drop := []string{"DROP TABLE DIF", "DROP TABLE EXTRA"}
	for _, p := range []string{pathA, pathB} {
		seed(p, drop, true) // tolerate leftovers from an aborted run
	}
	// A: reference. B: NM widened, EXTRA table added, row 2 altered,
	// row 3 removed, row 4 added.
	seed(pathA, []string{
		"CREATE TABLE DIF (ID INTEGER NOT NULL PRIMARY KEY, NM VARCHAR(30), AMT INTEGER)",
		"INSERT INTO DIF VALUES (1, 'x', 1)",
		"INSERT INTO DIF VALUES (2, 'y', 2)",
		"INSERT INTO DIF VALUES (3, 'z', 3)",
	}, false)
	seed(pathB, []string{
		"CREATE TABLE DIF (ID INTEGER NOT NULL PRIMARY KEY, NM VARCHAR(40), AMT INTEGER)",
		"CREATE TABLE EXTRA (ID INTEGER NOT NULL PRIMARY KEY)",
		"INSERT INTO DIF VALUES (1, 'x', 1)",
		"INSERT INTO DIF VALUES (2, 'y', 9)",
		"INSERT INTO DIF VALUES (4, 'w', 4)",
	}, false)

	// register the scratch dbs against fb3 in-memory
	cfg.Databases = append(cfg.Databases, config.Database{
		ID: "diff_a", Instance: "fb3", Path: pathA,
		ROUser: "SYSDBA", ROSecretEnv: "FBMCP_DEV_PW",
		AdminUser: "SYSDBA", AdminSecretEnv: "FBMCP_DEV_PW",
	}, config.Database{
		ID: "diff_b", Instance: "fb3", Path: pathB,
		ROUser: "SYSDBA", ROSecretEnv: "FBMCP_DEV_PW",
		AdminUser: "SYSDBA", AdminSecretEnv: "FBMCP_DEV_PW",
	})
	cfg.State.Dir = t.TempDir()

	// MCP harness (P2 tools carry the diff tools)
	st, err := state.Open(cfg.State.Dir)
	if err != nil {
		t.Fatal(err)
	}
	aud, err := audit.Open(cfg.State.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer aud.Close()
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
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "diff-live", Version: "0"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: cliConn, Writer: cliConn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	call := func(tool string, args map[string]any) string {
		t.Helper()
		cctx, ccancel := context.WithTimeout(ctx, 90*time.Second)
		defer ccancel()
		res, err := sess.CallTool(cctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		var b strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return b.String()
	}
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }

	// 1. two-db schema diff: NM type change, EXTRA only in B.
	out := call("fb_diff_schema", map[string]any{"db": "diff_a", "vs_db": "diff_b"})
	for _, want := range []string{"only in diff_b", "EXTRA", "would need ALTER on diff_b", "NM: VARCHAR(30)", "VARCHAR(40)"} {
		if !strings.Contains(norm(out), norm(want)) {
			t.Fatalf("schema diff missing %q:\n%.1200s", want, out)
		}
	}

	// 2. data diff: 1 only-in-A, 1 only-in-B, 1 differing.
	out = call("fb_diff_data", map[string]any{"db": "diff_a", "vs_db": "diff_b", "table": "DIF"})
	for _, want := range []string{"only in diff_a: 1", "only in diff_b: 1", "differing: 1", "ID=2", "ID=4"} {
		if !strings.Contains(norm(out), norm(want)) {
			t.Fatalf("data diff missing %q:\n%.1200s", want, out)
		}
	}

	// 3. cap refusal.
	out = call("fb_diff_data", map[string]any{"db": "diff_a", "vs_db": "diff_b", "table": "DIF", "row_cap": 2})
	if !strings.Contains(out, "refusing") {
		t.Fatalf("cap should refuse:\n%.600s", out)
	}

	// 4. snapshot save, then drift, then snapshot-vs-now.
	out = call("fb_diff_schema", map[string]any{"db": "diff_a", "save_snapshot": true})
	if !strings.Contains(out, "snapshot saved") {
		t.Fatalf("save_snapshot:\n%.600s", out)
	}
	seed(pathA, []string{"ALTER TABLE DIF ADD NEWCOL INTEGER"}, false)
	out = call("fb_diff_schema", map[string]any{"db": "diff_a"})
	if !strings.Contains(norm(out), norm("ADD column NEWCOL")) {
		t.Fatalf("snapshot-vs-now drift (NEWCOL) not reported:\n%.1200s", out)
	}
}

func spike3Path(cfg *config.Config) string {
	for _, d := range cfg.Databases {
		if d.ID == "spike3" {
			return d.Path
		}
	}
	return ""
}

func pwFor(t *testing.T) string {
	t.Helper()
	if pw := os.Getenv("FBMCP_DEV_PW"); pw != "" {
		return pw
	}
	t.Skip("FBMCP_DEV_PW not set")
	return ""
}
