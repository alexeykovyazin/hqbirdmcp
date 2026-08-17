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
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/backupsvc"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/dbpool"
	"github.com/aleks/fbmcp/internal/facts"
	"github.com/aleks/fbmcp/internal/gate"
	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/instlock"
	"github.com/aleks/fbmcp/internal/jobs"
	"github.com/aleks/fbmcp/internal/policy"
	"github.com/aleks/fbmcp/internal/state"
)

// demo tool set (Phase 1). The real Tier-0..2 surface arrives in Phases 2–4;
// metadata generation from firebird_dba_tasks_table_v3.md lands with P1.5's
// CI diff (plan §8) — entries here are explicitly demo-only.
var registerExtra func(server *mcp.Server, cfg *config.Config, pools *dbpool.Manager, engFacts *facts.EngineFacts, aud *audit.Logger)

var toolMeta = []policy.ToolMeta{
	{Name: "fb_ping", Tier: 0, Scope: "database"},
	{Name: "fb_db_list", Tier: 0, Scope: "database"},
	{Name: "fb_db_health", Tier: 0, Scope: "database"},
	{Name: "fb_job_status", Tier: 0, Scope: "database"},
	{Name: "fb_confirm", Tier: 0, Scope: "database"}, // gate entry point, not a mutation
	{Name: "fb_cancel", Tier: 0, Scope: "database"},
	{Name: "fb_demo_write", Tier: 1, Scope: "database", RetrySafe: true},
	{Name: "fb_info", Tier: 0, Scope: "database", MinFB: "2.5"},
	{Name: "fb_sessions", Tier: 0, Scope: "database", MinFB: "2.5"},
	{Name: "fb_transactions", Tier: 0, Scope: "database", MinFB: "2.5"},
	{Name: "fb_analyze_query", Tier: 0, Scope: "database", MinFB: "2.5"},
	{Name: "fb_index_stats", Tier: 0, Scope: "database", MinFB: "2.5"},
	{Name: "fb_schema_list", Tier: 0, Scope: "database", MinFB: "2.5"},
	{Name: "fb_describe", Tier: 0, Scope: "database", MinFB: "2.5"},
	{Name: "fb_activity_sample", Tier: 0, Scope: "database", MinFB: "2.5"},
	{Name: "fb_backup_start", Tier: 1, Scope: "database"},
	{Name: "fb_restore_test", Tier: 1, Scope: "database"},
	{Name: "fb_validate", Tier: 1, Scope: "database"},
	{Name: "fb_sweep", Tier: 1, Scope: "database"},
	{Name: "fb_set_forcewrite", Tier: 1, Scope: "database"},
	{Name: "fb_set_readonly", Tier: 1, Scope: "database"},
	{Name: "fb_service_status", Tier: 0, Scope: "instance"},
	{Name: "fb_restore_replace", Tier: 2, Scope: "database", Preconditions: []policy.Precondition{
		{Name: "verified_backup_exists", Op: "true", Why: "verified backup required"},
		{Name: "backup_freshness", Op: "lt", Value: 24.0, Why: "backup < 24h"},
	}},
}

func main() {
	cfgPath := flag.String("config", "fbmcp.yaml", "path to fbmcp configuration")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fbmcp: %v\n", err)
		os.Exit(1)
	}
	instLock, err := instlock.Acquire(cfg.State.Dir) // D8: fail fast on second instance
	if err != nil {
		fmt.Fprintf(os.Stderr, "fbmcp: %v\n", err)
		os.Exit(1)
	}
	defer instLock.Release()
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

	g := gate.New(st, aud)
	pools := dbpool.NewManager(cfg)
	defer pools.Close()
	runner := jobs.NewRunner(st)
	defer runner.Close()
	engFacts := facts.NewEngineFacts(cfg, pools) // first real facts provider (P2.1)
	allFacts := state.CompositeFacts{engFacts, backupsvc.NewCatalog(st)}
	eng := policy.New(toolMeta, allFacts, st)
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
	gt := &gatedTools{cfg: cfg, pools: pools, eng: eng, g: g, runner: runner, aud: aud, st: st,
		execs: map[string]executor{}, args: map[string]map[string]any{}}
	registerP3Tools(server, gt)
	gt.registerRestore(server)
	gt.startApprovalWatcher(context.Background())
	// fb_confirm → gate → dispatcher → job manager.
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
		if p.Tool == "fb_demo_write" { // legacy demo path
			id, err := runner.Submit("demo_write", p.Database, p.Identity, p.ID, func(ctx context.Context, prog func(float64, string)) (string, error) {
				prog(0.5, "demo work")
				return "demo mutation 'executed' (no-op)", nil
			})
			if err != nil {
				return text("submit failed: " + err.Error()), nil, nil
			}
			return text(fmt.Sprintf("confirmed; job %s submitted — check fb_job_status", id)), nil, nil
		}
		id, err := gt.dispatch(p)
		if err != nil {
			return text("dispatch failed: " + err.Error()), nil, nil
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

	// fb_info — P2.1: capability probe; the facts also feed version gating.
	mcp.AddTool(server, &mcp.Tool{Name: "fb_info", Description: "Tier 0: engine version, ODS, dialect, page size, RO/ForceWrite state"}, func(ctx context.Context, req *mcp.CallToolRequest, a dbArg) (*mcp.CallToolResult, any, error) {
		snap, err := engFacts.Snapshot(ctx, a.Db)
		if err != nil {
			return text("error: " + err.Error()), nil, nil
		}
		var b strings.Builder
		for _, k := range []string{"engine_version_full", "engine_version", "ods", "sql_dialect", "page_size", "read_only", "forced_writes", "sweep_interval"} {
			if v, ok := snap[k]; ok {
				fmt.Fprintf(&b, "%s: %v\n", k, v)
			}
		}
		aud.Log(audit.Entry{Identity: "local", Database: a.Db, Tool: "fb_info", Tier: 0, Decision: "allow"})
		return text(b.String()), nil, nil
	})

	// fb_sessions — P2.2: MON$ATTACHMENTS (+running statement per attachment).
	mcp.AddTool(server, &mcp.Tool{Name: "fb_sessions", Description: "Tier 0: list attachments (user, remote address, state, timestamp) with running statements"}, func(ctx context.Context, req *mcp.CallToolRequest, a dbArg) (*mcp.CallToolResult, any, error) {
		tx, err := pools.ReadOnly(ctx, a.Db) // engine-enforced read-only
		if err != nil {
			return text("error: " + err.Error()), nil, nil
		}
		defer tx.Rollback()
		rows, err := tx.QueryContext(ctx, `SELECT MON$USER, COALESCE(MON$REMOTE_ADDRESS,''), COALESCE(MON$STATE,''), MON$TIMESTAMP
			FROM MON$ATTACHMENTS ORDER BY MON$TIMESTAMP`)
		if err != nil {
			return text("error: " + err.Error()), nil, nil
		}
		defer rows.Close()
		var b strings.Builder
		n := 0
		for rows.Next() {
			var user, addr, st string
			var ts time.Time
			if err := rows.Scan(&user, &addr, &st, &ts); err != nil {
				return text("error: " + err.Error()), nil, nil
			}
			fmt.Fprintf(&b, "- %s from %s state=%s since %s\n", user, addr, st, ts.UTC().Format(time.RFC3339))
			n++
			if n >= 100 { // row cap (§2 ADR-014 discipline)
				b.WriteString("... (capped at 100 rows)\n")
				break
			}
		}
		aud.Log(audit.Entry{Identity: "local", Database: a.Db, Tool: "fb_sessions", Tier: 0, Decision: "allow"})
		return text(b.String()), nil, nil
	})

	// fb_transactions — P2.3: MGA health: OIT/OAT/OST/Next from MON$DATABASE.
	mcp.AddTool(server, &mcp.Tool{Name: "fb_transactions", Description: "Tier 0: transaction health — OIT/OAT/OST/Next and gap sizes"}, func(ctx context.Context, req *mcp.CallToolRequest, a dbArg) (*mcp.CallToolResult, any, error) {
		tx, err := pools.ReadOnly(ctx, a.Db)
		if err != nil {
			return text("error: " + err.Error()), nil, nil
		}
		defer tx.Rollback()
		var oit, oat, ost, next int64
		if err := tx.QueryRowContext(ctx, `SELECT MON$OLDEST_TRANSACTION, MON$OLDEST_ACTIVE, MON$OLDEST_SNAPSHOT, MON$NEXT_TRANSACTION FROM MON$DATABASE`).
			Scan(&oit, &oat, &ost, &next); err != nil {
			return text("error: " + err.Error()), nil, nil
		}
		out := fmt.Sprintf("oldest_transaction: %d\noldest_active: %d\noldest_snapshot: %d\nnext: %d\nnext-oit gap: %d\n", oit, oat, ost, next, next-oit)
		aud.Log(audit.Entry{Identity: "local", Database: a.Db, Tool: "fb_transactions", Tier: 0, Decision: "allow"})
		return text(out), nil, nil
	})

	registerExtra(server, cfg, pools, engFacts, aud)
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

func init() { registerExtra = registerP2Tools }
