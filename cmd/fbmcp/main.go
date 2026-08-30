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
	"net"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/attach"
	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/backupsvc"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/dbpool"
	"github.com/aleks/fbmcp/internal/errdoc"
	execpkg "github.com/aleks/fbmcp/internal/executor"
	"github.com/aleks/fbmcp/internal/facts"
	"github.com/aleks/fbmcp/internal/gate"
	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/instlock"
	"github.com/aleks/fbmcp/internal/jobs"
	"github.com/aleks/fbmcp/internal/notify"
	"github.com/aleks/fbmcp/internal/policy"
	"github.com/aleks/fbmcp/internal/qlog"
	"github.com/aleks/fbmcp/internal/reload"
	"github.com/aleks/fbmcp/internal/schedule"
	"github.com/aleks/fbmcp/internal/state"
	"github.com/aleks/fbmcp/internal/transport"
	"github.com/aleks/fbmcp/internal/workflows"
)

// demo tool set (Phase 1). The real Tier-0..2 surface arrives in Phases 2–4;
// metadata generation from firebird_dba_tasks_table_v3.md lands with P1.5's
// CI diff (plan §8) — entries here are explicitly demo-only.
var registerExtra func(server *mcp.Server, cfg *config.Handle, pools *dbpool.Manager, engFacts *facts.EngineFacts, aud *audit.Logger, st *state.Store)

var toolMeta = []policy.ToolMeta{
	{Name: "fb_ping", Tier: 0, Scope: "database"},
	{Name: "fb_db_list", Tier: 0, Scope: "database"},
	{Name: "fb_db_health", Tier: 0, Scope: "database"},
	{Name: "fb_job_status", Tier: 0, Scope: "database"},
	{Name: "fb_confirm", Tier: 0, Scope: "database"}, // gate entry point, not a mutation
	{Name: "fb_cancel", Tier: 0, Scope: "database"},
	{Name: "fb_demo_write", Tier: 1, Scope: "database", RetrySafe: true},
	{Name: "fb_info", Tier: 0, Scope: "database", MinFB: "2.5"},
	{Name: "fb_connected_dbs", Tier: 0, Scope: "instance", MinFB: "2.5"},
	{Name: "fb_db_register", Tier: 2, Scope: "instance"},
	{Name: "fb_config_reload", Tier: 0, Scope: "instance"},
	{Name: "fb_sessions", Tier: 0, Scope: "database", MinFB: "2.5"},
	{Name: "fb_transactions", Tier: 0, Scope: "database", MinFB: "2.5"},
	{Name: "fb_analyze_query", Tier: 0, Scope: "database", MinFB: "2.5"},
	{Name: "fb_index_advice", Tier: 0, Scope: "database", MinFB: "2.5"}, // C.2: plan analysis → proposed CREATE INDEX DDL (apply via fb_write)
	{Name: "fb_index_stats", Tier: 0, Scope: "database", MinFB: "2.5"},
	{Name: "fb_gstat", Tier: 0, Scope: "database"}, // ADR-003 gstat route; no MinFB — utility route, works without a server
	{Name: "fb_schema_list", Tier: 0, Scope: "database", MinFB: "2.5"},
	{Name: "fb_describe", Tier: 0, Scope: "database", MinFB: "2.5"},
	{Name: "fb_diff_schema", Tier: 0, Scope: "database", MinFB: "2.5"}, // C.3: two dbs or snapshot-vs-now
	{Name: "fb_diff_data", Tier: 0, Scope: "database", MinFB: "2.5"},   // C.3: bounded key-based data diff
	{Name: "fb_activity_sample", Tier: 0, Scope: "database", MinFB: "2.5"},
	{Name: "fb_trends", Tier: 0, Scope: "database", MinFB: "2.5"}, // C.4: sampler history → capacity projections
	{Name: "fb_lwmonitoring", Tier: 0, Scope: "instance"},
	{Name: "fb_backup_start", Tier: 1, Scope: "database"},
	{Name: "fb_restore_test", Tier: 1, Scope: "database"},
	{Name: "fb_validate", Tier: 1, Scope: "database"},
	{Name: "fb_sweep", Tier: 1, Scope: "database"},
	{Name: "fb_set_forcewrite", Tier: 1, Scope: "database"},
	{Name: "fb_set_readonly", Tier: 1, Scope: "database"},
	{Name: "fb_service_status", Tier: 0, Scope: "instance"},
	{Name: "fb_write", Tier: 1, Scope: "database"},                                 // dynamic tier: classified per request
	{Name: "fb_migration_status", Tier: 0, Scope: "database", MinFB: "2.5"},        // C.1: dir vs history
	{Name: "fb_migration_plan", Tier: 0, Scope: "database", MinFB: "2.5"},          // C.1: dry-run classification
	{Name: "fb_migration_apply", Tier: 1, Scope: "database"},                       // C.1: ADR-030 batch gate; dynamic tier per batch
	{Name: "fb_migration_rollback_plan", Tier: 0, Scope: "database", MinFB: "2.5"}, // C.1: renders recorded down sections
	{Name: "fb_query", Tier: 0, Scope: "database", MinFB: "2.5"},                   // read-only tx; fallback into fb_write's gated flow for refused EXECUTE PROCEDURE
	{Name: "fb_index_rebuild", Tier: 1, Scope: "database"},
	{Name: "fb_index_drop", Tier: 1, Scope: "database"},
	{Name: "fb_session_kill", Tier: 1, Scope: "database"},
	{Name: "fb_user_create", Tier: 1, Scope: "database"},
	{Name: "fb_user_drop", Tier: 1, Scope: "database"},
	{Name: "fb_role_create", Tier: 1, Scope: "database"},
	{Name: "fb_grant", Tier: 1, Scope: "database"},
	{Name: "fb_revoke", Tier: 1, Scope: "database"},
	{Name: "fb_comment_set", Tier: 1, Scope: "database"},
	{Name: "fb_db_create", Tier: 1, Scope: "database"},
	{Name: "fb_db_drop", Tier: 3, Scope: "database"},
	{Name: "fb_shutdown_window", Tier: 2, Scope: "database", Preconditions: []policy.Precondition{
		{Name: "verified_backup_exists", Op: "true", Why: "verified backup required"},
		{Name: "backup_freshness", Op: "lt", Value: 24.0, Why: "backup < 24h"},
	}},
	{Name: "fb_config_get", Tier: 0, Scope: "instance"},
	{Name: "fb_config_diff", Tier: 0, Scope: "instance"},
	{Name: "fb_config_set", Tier: 2, Scope: "instance"},
	{Name: "fb_window_open", Tier: 1, Scope: "database"},
	{Name: "fb_set_page_buffers", Tier: 1, Scope: "database"},
	{Name: "fb_restore_replace", Tier: 2, Scope: "database", Preconditions: []policy.Precondition{
		{Name: "verified_backup_exists", Op: "true", Why: "verified backup required"},
		{Name: "backup_freshness", Op: "lt", Value: 24.0, Why: "backup < 24h"},
	}},
	{Name: "fb_backup_nbackup", Tier: 1, Scope: "database"},
	{Name: "fb_trace_start", Tier: 1, Scope: "database"},
	{Name: "fb_trace_stop", Tier: 1, Scope: "database"},
	{Name: "fb_trace_list", Tier: 0, Scope: "database"},
	{Name: "fb_effective_access", Tier: 0, Scope: "database"},
	{Name: "fb_schedule_list", Tier: 0, Scope: "database"},
	{Name: "fb_schedule_create", Tier: 1, Scope: "database"}, // dynamic: max tier of target
	{Name: "fb_schedule_delete", Tier: 1, Scope: "database"},
	{Name: "fb_retention_run", Tier: 1, Scope: "database"},
	{Name: "fb_service_start", Tier: 2, Scope: "instance"},
	{Name: "fb_service_stop", Tier: 2, Scope: "instance"},
	{Name: "fb_service_restart", Tier: 2, Scope: "instance"},
}

func main() {
	if maybeRunService() {
		return
	}
	runForegroundCtx(context.Background())
}

func runForegroundCtx(ctx context.Context) {
	cfgPath := flag.String("config", "fbmcp.yaml", "path to fbmcp configuration")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fbmcp: %v\n", err)
		os.Exit(1)
	}
	identity.SetLocalMaxTier(cfg.LocalMaxTierOrDefault()) // WS2.3; re-applied on reload via ApplyAuth
	instLock, err := instlock.Acquire(cfg.State.Dir)      // D8: one kernel; extra stdio clients attach
	if err != nil {
		if attach.PipedStdin() {
			fmt.Fprintf(os.Stderr, "fbmcp: attaching to existing instance (pid %d)\n", instlock.OwnerPID(cfg.State.Dir))
			if aerr := attach.RunProxy(cfg.State.Dir); aerr != nil {
				fmt.Fprintf(os.Stderr, "fbmcp: %v\n", err)
				fmt.Fprintf(os.Stderr, "fbmcp: attach: %v\n", aerr)
				os.Exit(1)
			}
			return
		}
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
	ql, err := qlog.Open(cfg.State.Dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fbmcp: qlog: %v\n", err)
		os.Exit(1)
	}
	defer ql.Close()
	st, err := state.Open(cfg.State.Dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fbmcp: state: %v\n", err)
		os.Exit(1)
	}

	g := gate.New(st, aud)
	handle := config.NewHandle(cfg)
	pools := dbpool.NewManager(handle)
	defer pools.Close()
	runner := jobs.NewRunner(st)
	defer runner.Close()
	engFacts := facts.NewEngineFacts(handle, pools) // first real facts provider (P2.1)
	allFacts := state.CompositeFacts{engFacts, backupsvc.NewCatalog(st)}
	eng := policy.New(toolMeta, allFacts, st)

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
		for _, db := range handle.Current().Databases {
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
			return errText("offline: " + err.Error())
		}
		aud.Log(audit.Entry{Identity: "local", Database: a.Db, Tool: "fb_db_health", Tier: 0, Decision: "allow"})
		return text("online"), nil, nil
	})

	// fb_demo_write — the M1 gated-tool demo: policy → pending action →
	execSvc := &execpkg.Service{Pools: pools}
	wfEng := workflows.New(st)
	gt := &gatedTools{cfg: handle, pools: pools, eng: eng, g: g, runner: runner, aud: aud, qlog: ql, st: st,
		execSvc: execSvc, wf: wfEng, execs: map[string]executor{}, args: map[string]map[string]any{},
		traces: map[string]*backupsvc.LiveTrace{}, facts: engFacts}
	hookSecret := ""
	if cfg.Notify.WebhookSecretEnv != "" {
		hookSecret, _ = config.SecretFromEnv(cfg.Notify.WebhookSecretEnv)
	}
	bus, err := notify.New(cfg.State.Dir, cfg.Notify.WebhookURL, hookSecret)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fbmcp: notify: %v\n", err)
		os.Exit(1)
	}
	gt.bus = bus
	httpLn := &httpListener{mcp: server}
	gt.httpLn = httpLn
	gt.reloader = reload.New(handle, gt.reloadHooks())
	runner.SetHook(func(j state.Job) {
		typ := "job." + j.State
		_ = bus.Emit(notify.Event{Type: typ, Database: j.Database, Tool: j.Type, Message: j.Message,
			Detail: map[string]string{"job_id": j.ID, "request_id": j.RequestID}})
	})
	g.SetHooks(
		func(p state.PendingAction) {
			_ = bus.Emit(notify.Event{Type: "gate.pending", Database: p.Database, Tool: p.Tool, Message: "confirmation waiting",
				Detail: map[string]string{"request_id": p.ID}})
		},
		nil,
		func(p state.PendingAction) {
			_ = bus.Emit(notify.Event{Type: "gate.expired", Database: p.Database, Tool: p.Tool, Message: "pending action expired",
				Detail: map[string]string{"request_id": p.ID}})
		},
	)
	registerP3Tools(server, gt)
	gt.registerRestore(server)
	registerP4Tools(server, gt)
	registerP5Tools(server, gt)
	// AutoReopen reconciliation must run AFTER all workflow types are
	// registered above (C7a): a restore_replace interrupted by a kill is
	// resumed from its .pre-restore snapshot — marking it failed here would
	// leave the database file removed.
	wfEng.Reconcile(context.Background())
	gt.startApprovalWatcher(context.Background())
	sched := schedule.New(st, gt.fireSchedule).OnSkip(func(s state.Schedule, reason string) {
		_ = bus.Emit(notify.Event{Type: "scheduler.skip", Database: s.Database, Tool: s.Target, Message: reason,
			Detail: map[string]string{"schedule_id": s.ID, "confirmer": s.Confirmer, "channel": s.Channel, "creating_request": s.CreatingRequest}})
	}).WithDBExists(func(id string) bool {
		_, err := handle.DB(id)
		return err == nil
	})
	sched.Start(context.Background())
	startTrendsSampler(ctx, handle, pools) // C.4: capacity sampler (trends/ dir)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
				g.SweepExpired()
			}
		}
	}()
	// fb_confirm → gate → dispatcher → job manager.
	mcp.AddTool(server, &mcp.Tool{Name: "fb_demo_write", Description: "DEMO Tier-1 gated tool: policy → impact statement → confirm → job (no real mutation)"}, func(ctx context.Context, req *mcp.CallToolRequest, a dbArg) (*mcp.CallToolResult, any, error) {
		id := identity.Caller(ctx)
		d := eng.Evaluate(id, a.Db, "fb_demo_write")
		if d.Outcome == "deny" {
			aud.Log(audit.Entry{Identity: id.Name, Database: a.Db, Tool: "fb_demo_write", Tier: d.Meta.Tier, Decision: "denied", Detail: map[string]interface{}{"reason": d.Reason}})
			return errText("DENIED: " + d.Reason)
		}
		argHash := hashOf(a.Db)
		p, err := g.Request(id, a.Db, d.Meta,
			fmt.Sprintf("Demo write on database %s (no-op function; demonstrates the gate).", a.Db),
			argHash, d.FailedPreconditions)
		if err != nil {
			return errText("gate error: " + err.Error())
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
		Wait      bool   `json:"wait,omitempty" jsonschema:"block until the submitted job reaches a terminal state; emits progress notifications when the client supplied a progress token (phase8_plan D3.2)"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_confirm", Description: "confirm a pending action (in-band: Tier 1 only; Tier >= 2 requires the out-of-band surface)"}, func(ctx context.Context, req *mcp.CallToolRequest, a confirmArg) (*mcp.CallToolResult, any, error) {
		caller := identity.Caller(ctx)
		p, err := g.Confirm(a.RequestID, caller.Name, gate.ChannelInBand, a.Token)
		if err != nil {
			return errText("confirmation rejected: " + err.Error())
		}
		if p.Tool == "fb_demo_write" { // legacy demo path
			jobID, err := runner.Submit("demo_write", p.Database, p.Identity, p.ID, func(ctx context.Context, prog func(float64, string)) (string, error) {
				prog(0.5, "demo work")
				return "demo mutation 'executed' (no-op)", nil
			})
			if err != nil {
				return errText("submit failed: " + err.Error())
			}
			if a.Wait {
				return waitJobResult(ctx, req, runner, jobID)
			}
			return text(fmt.Sprintf("confirmed; job %s submitted — check fb_job_status", jobID)), nil, nil
		}
		jobID, err := gt.dispatch(p)
		if err != nil {
			return errText("dispatch failed: " + err.Error())
		}
		if a.Wait {
			return waitJobResult(ctx, req, runner, jobID)
		}
		return text(fmt.Sprintf("confirmed; job %s submitted — check fb_job_status", jobID)), nil, nil
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
	mcp.AddTool(server, &mcp.Tool{Name: "fb_job_status", Description: "Tier 0: job status (structured: id/type/db/state/progress/message/detail)"}, func(ctx context.Context, req *mcp.CallToolRequest, a jobArg) (*mcp.CallToolResult, any, error) {
		j, ok := runner.Status(a.JobID)
		if !ok {
			return errText("unknown job")
		}
		return jobPayload(j), jobStruct(j), nil
	})

	// fb_info — P2.1: capability probe; the facts also feed version gating.
	mcp.AddTool(server, &mcp.Tool{Name: "fb_info", Description: "Tier 0: engine version, ODS, dialect, page size, RO/ForceWrite state"}, func(ctx context.Context, req *mcp.CallToolRequest, a dbArg) (*mcp.CallToolResult, any, error) {
		snap, err := engFacts.Snapshot(ctx, a.Db)
		if err != nil {
			return errText("error: " + err.Error())
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

	mcp.AddTool(server, &mcp.Tool{Name: "fb_connected_dbs", Description: "Tier 0: list active databases on the chosen instance and map them to managed db ids"}, func(ctx context.Context, req *mcp.CallToolRequest, a instanceArg) (*mcp.CallToolResult, any, error) {
		info, err := facts.ConnectedDatabases(handle.Current(), a.Instance)
		if err != nil {
			aud.Log(audit.Entry{Identity: "local", Database: a.Instance, Tool: "fb_connected_dbs", Tier: 0, Decision: "error", Detail: map[string]interface{}{"error": err.Error()}})
			return errText("error: " + err.Error())
		}
		aud.Log(audit.Entry{Identity: "local", Database: a.Instance, Tool: "fb_connected_dbs", Tier: 0, Decision: "allow"})
		return text(formatConnectedDBs(info)), nil, nil
	})
	registerDBTool(server, gt)

	mcp.AddTool(server, &mcp.Tool{Name: "fb_config_reload", Description: "Tier 0: re-read fbmcp.yaml and apply to the live kernel (no process restart)"}, func(ctx context.Context, req *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
		res, err := gt.reloader.Apply("fb_config_reload", "")
		return text(formatReload(res, err)), nil, nil
	})

	// fb_sessions — P2.2: MON$ATTACHMENTS (+running statement per attachment).
	mcp.AddTool(server, &mcp.Tool{Name: "fb_sessions", Description: "Tier 0: list attachments (user, remote address, state, timestamp) with running statements"}, func(ctx context.Context, req *mcp.CallToolRequest, a dbArg) (*mcp.CallToolResult, any, error) {
		tx, err := pools.ReadOnly(ctx, a.Db) // engine-enforced read-only
		if err != nil {
			return errText("error: " + err.Error())
		}
		defer tx.Rollback()
		rows, err := tx.QueryContext(ctx, `SELECT MON$USER, COALESCE(MON$REMOTE_ADDRESS,''), COALESCE(MON$STATE,''), MON$TIMESTAMP
			FROM MON$ATTACHMENTS ORDER BY MON$TIMESTAMP`)
		if err != nil {
			return errText("error: " + err.Error())
		}
		defer rows.Close()
		var b strings.Builder
		n := 0
		for rows.Next() {
			var user, addr, st string
			var ts time.Time
			if err := rows.Scan(&user, &addr, &st, &ts); err != nil {
				return errText("error: " + err.Error())
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
			return errText("error: " + err.Error())
		}
		defer tx.Rollback()
		var oit, oat, ost, next int64
		if err := tx.QueryRowContext(ctx, `SELECT MON$OLDEST_TRANSACTION, MON$OLDEST_ACTIVE, MON$OLDEST_SNAPSHOT, MON$NEXT_TRANSACTION FROM MON$DATABASE`).
			Scan(&oit, &oat, &ost, &next); err != nil {
			return errText("error: " + err.Error())
		}
		out := fmt.Sprintf("oldest_transaction: %d\noldest_active: %d\noldest_snapshot: %d\nnext: %d\nnext-oit gap: %d\n", oit, oat, ost, next, next-oit)
		aud.Log(audit.Entry{Identity: "local", Database: a.Db, Tool: "fb_transactions", Tier: 0, Decision: "allow"})
		return text(out), nil, nil
	})

	registerExtra(server, handle, pools, engFacts, aud, st)
	registerSurfaces(server, handle, st)
	if err := serve(ctx, server, handle, gt); err != nil {
		log.Fatalf("fbmcp: server: %v", err)
	}
}

// stopAndWait cancels a foreground run and waits (bounded) for its deferred
// cleanup — audit close, job drain, pools, instance lock — to finish. Used by
// the Windows service Stop path (P6.2 T6 / improvement-plan A.2) so the SCM
// sees Stopped only after the kernel has drained.
func stopAndWait(cancel context.CancelFunc, done <-chan struct{}, timeout time.Duration) {
	cancel()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func serve(ctx context.Context, server *mcp.Server, handle *config.Handle, gt *gatedTools) error {
	cfg := handle.Current()
	stop, err := attach.Start(cfg.State.Dir, func(c net.Conn) {
		_ = server.Run(ctx, &mcp.IOTransport{Reader: c, Writer: c})
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "fbmcp: attach listener: %v\n", err)
	} else {
		defer stop()
	}
	if gt.reloader != nil {
		if err := gt.reloader.Watch(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "fbmcp: config watch: %v\n", err)
		}
	}
	if strings.TrimSpace(cfg.Listen) != "" {
		if err := transport.CheckRemote(cfg.Listen, cfg.TLS.Cert, cfg.TLS.Key, len(cfg.Identities), len(cfg.AllowedOrigins)); err != nil {
			return err
		}
		if err := gt.httpLn.Start(cfg); err != nil {
			return err
		}
		go func() {
			<-ctx.Done()
			cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = gt.httpLn.Close(cctx)
		}()
	}
	defer func() {
		if gt.httpLn == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = gt.httpLn.Close(ctx)
	}()
	if attach.PipedStdin() {
		return server.Run(ctx, &mcp.StdioTransport{})
	}
	if strings.TrimSpace(cfg.Listen) != "" {
		return gt.httpLn.Wait()
	}
	return server.Run(ctx, &mcp.StdioTransport{})
}

func hashOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// errText is the D.2 error envelope (phase8_plan D3.1): a failing tool
// result that sets IsError and carries {code, message, hint?, remediation?}
// as structuredContent (the handler's second return) so clients can react
// programmatically. The text is unchanged for backward compatibility.
func errText(msg string) (*mcp.CallToolResult, map[string]any, error) {
	env := map[string]any{"code": "fbmcp", "message": msg}
	if d, ok := errdoc.Lookup(msg); ok {
		env["code"] = d.Code
		env["hint"] = d.Hint
		env["remediation"] = d.Remediation
	}
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}, env, nil
}

// jobStruct is the structured payload for job results (D3.2).
func jobStruct(j state.Job) map[string]any {
	return map[string]any{"id": j.ID, "type": j.Type, "db": j.Database, "state": j.State,
		"progress": j.Progress, "message": j.Message, "detail": j.Detail}
}

func jobPayload(j state.Job) *mcp.CallToolResult {
	return text(fmt.Sprintf("%s: %s (%.0f%%) %s", j.ID, j.State, j.Progress*100, j.Message))
}

// waitJobResult blocks until the job reaches a terminal state (D3.2 wait
// mode). Progress notifications go to clients that supplied a progress
// token; cancellation or the 30-minute bound returns the last known state.
func waitJobResult(ctx context.Context, req *mcp.CallToolRequest, runner *jobs.Runner, jobID string) (*mcp.CallToolResult, map[string]any, error) {
	token := req.Params.GetProgressToken()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	timeout := time.After(30 * time.Minute)
	for {
		j, ok := runner.Status(jobID)
		if !ok {
			return errText("unknown job")
		}
		switch j.State {
		case "succeeded", "failed", "cancelled", "interrupted":
			if j.State != "succeeded" {
				return errText(fmt.Sprintf("%s: %s (%.0f%%) %s", j.ID, j.State, j.Progress*100, j.Message))
			}
			return jobPayload(j), jobStruct(j), nil
		}
		if token != nil {
			_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
				ProgressToken: token, Progress: j.Progress * 100, Total: 100, Message: j.Message})
		}
		select {
		case <-ctx.Done():
			return jobPayload(j), jobStruct(j), nil
		case <-timeout:
			return errText("wait timeout (30 minutes) — poll fb_job_status")
		case <-tick.C:
		}
	}
}

func formatConnectedDBs(info *facts.ConnectedDBs) string {
	if info == nil {
		return "instance: \nattachment_count: 0\ndatabase_count: 0\n(active databases: none)\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "instance: %s\nattachment_count: %d\ndatabase_count: %d\n", info.Instance, info.AttachmentsCount, info.DatabaseCount)
	if len(info.Databases) == 0 {
		b.WriteString("(active databases: none)\n")
		return b.String()
	}
	for _, db := range info.Databases {
		fmt.Fprintf(&b, "- path: %s\n  match_status: %s\n", db.Path, db.MatchStatus)
		if db.DBID != "" {
			fmt.Fprintf(&b, "  db: %s\n", db.DBID)
		}
	}
	return b.String()
}

func init() { registerExtra = registerP2Tools }
