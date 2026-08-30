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
	"github.com/aleks/fbmcp/internal/trends"
)

// TestTrendsLiveMCP (C.4): runs two real sampler ticks against spike3 and
// then drives fb_trends through the full MCP dispatch path — samples must
// appear with a real size, and the tool must say it is still collecting
// history (2 < 3 samples). Opt-in:
//
//	FBMCP_DEV_PW=… FBMCP_TRENDS_LIVE=1 go test ./cmd/fbmcp -run TrendsLive -v
func TestTrendsLiveMCP(t *testing.T) {
	if os.Getenv("FBMCP_TRENDS_LIVE") == "" {
		t.Skip("set FBMCP_TRENDS_LIVE=1 to run")
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller info")
	}
	cfg, err := config.Load(filepath.Join(filepath.Dir(thisFile), "..", "..", "fbmcp.dev.yaml"))
	if err != nil {
		t.Skipf("dev config not loadable: %v", err)
	}
	// dedicated trends dir (never pollute the real state dir)
	cfg.State.Dir = t.TempDir()

	handle := config.NewHandle(cfg)
	pools := dbpool.NewManager(handle)
	defer pools.Close()
	st, err := state.Open(cfg.State.Dir)
	if err != nil {
		t.Fatal(err)
	}
	aud, err := audit.Open(cfg.State.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer aud.Close()
	engFacts := facts.NewEngineFacts(handle, pools)

	server := mcp.NewServer(&mcp.Implementation{Name: "fbmcp-test", Version: "0"}, nil)
	registerP2Tools(server, handle, pools, engFacts, aud, st)

	srvConn, cliConn := net.Pipe()
	go server.Run(context.Background(), &mcp.IOTransport{Reader: srvConn, Writer: srvConn})
	defer cliConn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "trends-live", Version: "0"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: cliConn, Writer: cliConn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	call := func(args map[string]any) string {
		t.Helper()
		cctx, ccancel := context.WithTimeout(ctx, 60*time.Second)
		defer ccancel()
		res, err := sess.CallTool(cctx, &mcp.CallToolParams{Name: "fb_trends", Arguments: args})
		if err != nil {
			t.Fatalf("fb_trends: %v", err)
		}
		var b strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return b.String()
	}

	// 0. no samples yet
	out := call(map[string]any{"db": "spike3"})
	if !strings.Contains(out, "no trend samples") {
		t.Fatalf("pre-tick render:\n%.600s", out)
	}

	// 1. two real ticks, ~1.1 s apart (interval is bypassed by TickOnce)
	sampler := &trends.Sampler{
		Dir:    cfg.State.Dir,
		List:   func() []trends.DBRef { return trendsDBRefs(handle) },
		Sample: newTrendsSampleFn(pools),
	}
	// employee5std has no running server — its skip is the correct behavior
	if ok, failed := sampler.TickOnce(ctx); ok < 1 || failed < 1 {
		t.Fatalf("tick1: ok=%d failed=%d (databases=%d)", ok, failed, len(cfg.Databases))
	}
	time.Sleep(1100 * time.Millisecond)
	if ok, failed := sampler.TickOnce(ctx); ok < 1 {
		t.Fatalf("tick2: ok=%d failed=%d", ok, failed)
	}
	sp, err := trends.Read(cfg.State.Dir, "spike3", 0)
	if err != nil || len(sp) != 2 {
		t.Fatalf("spike3 samples = %d err %v, want 2", len(sp), err)
	}
	if sp[1].SizeBytes == 0 || sp[1].Attachments < 1 || sp[1].OATGap < 0 {
		t.Fatalf("spike3 sample looks wrong: %+v", sp[1])
	}

	// 2. fb_trends shows both samples with a real size, no projection yet
	out = call(map[string]any{"db": "spike3", "hours": 1, "threshold_gb": 100})
	for _, want := range []string{"samples: 2", "size now:", "collecting history"} {
		if !strings.Contains(out, want) {
			t.Fatalf("render missing %q:\n%.900s", want, out)
		}
	}
}
