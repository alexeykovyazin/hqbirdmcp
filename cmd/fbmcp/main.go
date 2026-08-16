// cmd/fbmcp — the fbmcp MCP server (Phase 1: kernel + demo tools).
//
// Usage: fbmcp -config fbmcp.yaml   (stdio transport, local mode)
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/dbpool"
	"github.com/aleks/fbmcp/internal/gate"
	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/jobs"
	"github.com/aleks/fbmcp/internal/policy"
	"github.com/aleks/fbmcp/internal/state"
)

// demo tool set (Phase 1). The real Tier-0..2 surface arrives in Phases 2–4;
// metadata generation from firebird_dba_tasks_table_v3.md lands with P1.5's
// CI diff (plan §8) — entries here are explicitly demo-only.
var toolMeta = []policy.ToolMeta{
	{Name: "fb_ping", Tier: 0, Scope: "database"},
	{Name: "fb_db_list", Tier: 0, Scope: "database"},
	{Name: "fb_db_health", Tier: 0, Scope: "database"},
	{Name: "fb_job_status", Tier: 0, Scope: "database"},
	{Name: "fb_confirm", Tier: 0, Scope: "database"}, // gate entry point, not a mutation
	{Name: "fb_cancel", Tier: 0, Scope: "database"},
	{Name: "fb_demo_write", Tier: 1, Scope: "database", RetrySafe: true},
}

func main() {
	cfgPath := flag.String("config", "fbmcp.yaml", "path to fbmcp configuration")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fbmcp: %v\n", err)
		os.Exit(1)
	}
	aud, err := audit.Open(cfg.State.Dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fbmcp: audit: %v\n", err)
		os.Exit(1)
	}
	defer aud.Close()
	st, err := state.Open(cfg.State.Dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fbmcp: state: %v\n", err)
		os.Exit(1)
	}
	eng := policy.New(toolMeta, state.StubFacts{}, st) // stub facts until P2.1/P3.1
	g := gate.New(st, aud)
	pools := dbpool.NewManager(cfg)
	defer pools.Close()
	runner := jobs.NewRunner(st)
	defer runner.Close()
	localID := identity.Local(2, nil) // local ceiling: Tier 2 (Tier 3 disabled regardless)

	server := mcp.NewServer(&mcp.Implementation{Name: "fbmcp", Version: "0.1.0"}, nil)

	type noArgs struct{}
	type dbArg struct {
		Db string `json:"db" jsonschema:"registry id of the database"`
	}

	mcp.AddTool(server, &mcp.Tool{Name: "fb_ping", Description: "liveness probe"}, func(ctx context.Context, req *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
		return text("pong"), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{Name: "fb_db_list", Description: "Tier 0: list registered databases with health"}, func(ctx context.Context, req *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
		var b strings.Builder
		for _, db := range cfg.Databases {
			s := "online"
			if err := pools.Health(ctx, db.ID); err != nil {
				s = "offline"
			}
			fmt.Fprintf(&b, "- %s [%s]\n", db.ID, s)
		}
		return text(b.String()), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{Name: "fb_db_health", Description: "Tier 0: probe a registered database"}, func(ctx context.Context, req *mcp.CallToolRequest, a dbArg) (*mcp.CallToolResult, any, error) {
		if err := pools.Health(ctx, a.Db); err != nil {
			aud.Log(audit.Entry{Identity: "local", Database: a.Db, Tool: "fb_db_health", Tier: 0, Decision: "error", Detail: map[string]interface{}{"error": err.Error()}})
			return text("offline: " + err.Error()), nil, nil
		}
		aud.Log(audit.Entry{Identity: "local", Database: a.Db, Tool: "fb_db_health", Tier: 0, Decision: "allow"})
		return text("online"), nil, nil
	})

	// fb_demo_write — the M1 gated-tool demo: policy → pending action →
	// fb_confirm → job manager. The "mutation" is a no-op report.
	mcp.AddTool(server, &mcp.Tool{Name: "fb_demo_write", Description: "DEMO Tier-1 gated tool: policy → impact statement → confirm → job (no real mutation)"}, func(ctx context.Context, req *mcp.CallToolRequest, a dbArg) (*mcp.CallToolResult, any, error) {
		d := eng.Evaluate(localID, a.Db, "fb_demo_write")
		if d.Outcome == "deny" {
			aud.Log(audit.Entry{Identity: localID.Name, Database: a.Db, Tool: "fb_demo_write", Tier: d.Meta.Tier, Decision: "denied", Detail: map[string]interface{}{"reason": d.Reason}})
			return text("DENIED: " + d.Reason), nil, nil
		}
		argHash := hashOf(a.Db)
		p, err := g.Request(localID, a.Db, d.Meta,
			fmt.Sprintf("Demo write on database %s (no-op function; demonstrates the gate).", a.Db),
			argHash, d.FailedPreconditions)
		if err != nil {
			return text("gate error: " + err.Error()), nil, nil
		}
		tok := gate.IssueToken(p.ID, argHash)
		var b strings.Builder
		b.WriteString(gate.ImpactStatement(p))
		fmt.Fprintf(&b, "In-band token (Tier 1 only): %s\n", tok)
		return text(b.String()), nil, nil
	})

	type confirmArg struct {
		RequestID string `json:"request_id"`
		Token     string `json:"token,omitempty" jsonschema:"in-band token from the pending action (Tier 1 only)"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_confirm", Description: "confirm a pending action (in-band: Tier 1 only; Tier >= 2 requires the out-of-band surface)"}, func(ctx context.Context, req *mcp.CallToolRequest, a confirmArg) (*mcp.CallToolResult, any, error) {
		p, err := g.Confirm(a.RequestID, localID.Name, gate.ChannelInBand, a.Token)
		if err != nil {
			return text("confirmation rejected: " + err.Error()), nil, nil
		}
		id, err := runner.Submit("demo_write", p.Database, p.Identity, p.ID, func(ctx context.Context, prog func(float64, string)) (string, error) {
			prog(0.5, "demo work")
			return "demo mutation 'executed' (no-op)", nil
		})
		if err != nil {
			return text("submit failed: " + err.Error()), nil, nil
		}
		return text(fmt.Sprintf("confirmed; job %s submitted — check fb_job_status", id)), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{Name: "fb_cancel", Description: "cancel a pending action or a job"}, func(ctx context.Context, req *mcp.CallToolRequest, a confirmArg) (*mcp.CallToolResult, any, error) {
		if _, ok, _ := st.TakePending(a.RequestID); ok {
			return text("pending action cancelled"), nil, nil
		}
		if err := runner.Cancel(a.RequestID); err != nil {
			return text("cancel failed: " + err.Error()), nil, nil
		}
		return text("cancellation requested"), nil, nil
	})

	type jobArg struct {
		JobID string `json:"job_id"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_job_status", Description: "Tier 0: job status"}, func(ctx context.Context, req *mcp.CallToolRequest, a jobArg) (*mcp.CallToolResult, any, error) {
		j, ok := runner.Status(a.JobID)
		if !ok {
			return text("unknown job"), nil, nil
		}
		return text(fmt.Sprintf("%s: %s (%.0f%%) %s", j.ID, j.State, j.Progress*100, j.Message)), nil, nil
	})

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("fbmcp: server: %v", err)
	}
}

func hashOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}
