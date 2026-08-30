package main

import (
	"context"
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
	"github.com/aleks/fbmcp/internal/facts"
	"github.com/aleks/fbmcp/internal/state"
)

// TestGstatLiveMCP drives fb_gstat through the full MCP dispatch path
// (input schema, handler, records-mode table pre-check, gstat subprocess,
// audit) against the machine-local dev config. Opt-in, like the internal
// live smoke:
//
//	FBMCP_DEV_PW=… FBMCP_GSTAT_LIVE=1 go test ./cmd/fbmcp -run GstatLive -v
func TestGstatLiveMCP(t *testing.T) {
	if os.Getenv("FBMCP_GSTAT_LIVE") == "" {
		t.Skip("set FBMCP_GSTAT_LIVE=1 to run")
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
	server := mcp.NewServer(&mcp.Implementation{Name: "fbmcp-test", Version: "0"}, nil)
	registerP2Tools(server, handle, pools, facts.NewEngineFacts(handle, pools), aud, st)

	srvConn, cliConn := net.Pipe()
	go server.Run(context.Background(), &mcp.IOTransport{Reader: srvConn, Writer: srvConn})
	defer cliConn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "gstat-live", Version: "0"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: cliConn, Writer: cliConn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	call := func(args map[string]any) *mcp.CallToolResult {
		t.Helper()
		cctx, ccancel := context.WithTimeout(ctx, 90*time.Second)
		defer ccancel()
		res, err := sess.CallTool(cctx, &mcp.CallToolParams{Name: "fb_gstat", Arguments: args})
		if err != nil {
			t.Fatalf("fb_gstat %v: %v", args, err)
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

	// header mode on the stock-install db: no running server, no auth —
	// gstat reads the header page straight from the file.
	res := call(map[string]any{"db": "employee5std"})
	if out := body(res); res.IsError || !strings.Contains(out, "Database header page information:") {
		t.Fatalf("header mode on employee5std failed:\n%s", out)
	}

	// records mode restricted to three tables on the HQbird employee db.
	res = call(map[string]any{"db": "employee", "mode": "records", "tables": []string{"EMPLOYEE", "EMPLOYEE_PROJECT", "SALARY_HISTORY"}})
	out := body(res)
	if res.IsError {
		t.Fatalf("records mode failed:\n%s", out)
	}
	for _, want := range []string{"Analyzing database pages", "EMPLOYEE (", "EMPLOYEE_PROJECT (", "SALARY_HISTORY ("} {
		if !strings.Contains(out, want) {
			t.Fatalf("records output missing %q:\n%.400s", want, out)
		}
	}
	if strings.Contains(out, "SALES (") {
		t.Fatal("table filter leaked: SALES section present")
	}

	// records mode with a wrong-case name: the RDB$RELATIONS pre-check
	// must reject it with the uppercase suggestion (exact-case matching).
	res = call(map[string]any{"db": "employee", "mode": "records", "tables": []string{"employee"}})
	if out := body(res); !res.IsError || !strings.Contains(out, "did you mean: EMPLOYEE") {
		t.Fatalf("exact-case pre-check not triggered:\n%s", out)
	}
}
