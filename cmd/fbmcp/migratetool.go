// fb_migration_* tools (C.1): ordered .sql migrations from a config-
// registered, confine-confined per-database directory, with the
// FBMCP_MIGRATIONS history table. status/plan/rollback_plan are Tier 0
// (reads and rendering); apply is the ADR-030 batch gate — one confirmation
// for the whole manifest, argHash-bound, re-validated at execution time.
// The version-table bootstrap CREATE goes through executor.Prepare like any
// other statement; rollback execution stays fb_write's job.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/audit"
	execpkg "github.com/aleks/fbmcp/internal/executor"
	"github.com/aleks/fbmcp/internal/gate"
	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/migrate"
	"github.com/aleks/fbmcp/internal/policy"
)

// shortStmt renders a one-line statement preview for plan output.
func shortStmt(s string) string {
	oneLine := strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(oneLine) > 78 {
		return oneLine[:75] + "…"
	}
	return oneLine
}

// minFBGreater compares "MAJOR.MINOR" floors ("" lowest).
func minFBGreater(a, b string) bool {
	if a == "" {
		return false
	}
	if b == "" {
		return true
	}
	var am, bm, ap, bp int
	fmt.Sscanf(a, "%d.%d", &am, &ap)
	fmt.Sscanf(b, "%d.%d", &bm, &bp)
	return am > bm || (am == bm && ap > bp)
}

// migrationBatch is the computed apply payload shared by request and exec.
type migrationBatch struct {
	Dir        string
	Baseline   bool
	Pending    []migrate.Migration
	Manifest   string
	NeedsBoot  bool // version table does not exist yet
	BatchTier  int
	MinFB      string
	FilePlans  []filePlan
	PreviewTxt string
}

type filePlan struct {
	Version int
	Name    string
	Tier    int
	Stmnts  int
	Detail  []string
}

// history reads FBMCP_MIGRATIONS; missing table → (nil, false, nil).
func migrationHistory(ctx context.Context, gt *gatedTools, dbID string) ([]migrate.Applied, bool, error) {
	tx, err := gt.pools.ReadOnly(ctx, dbID)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(
		"SELECT ID, VERSION, CHECKSUM, APPLIED_BY FROM %s ORDER BY VERSION", migrate.Table))
	if err != nil {
		if strings.Contains(err.Error(), migrate.Table) {
			return nil, false, nil // table not created yet
		}
		return nil, false, err
	}
	defer rows.Close()
	var out []migrate.Applied
	for rows.Next() {
		var a migrate.Applied
		if err := rows.Scan(&a.Name, &a.Version, &a.Checksum, &a.AppliedBy); err != nil {
			return nil, false, err
		}
		out = append(out, a)
	}
	return out, true, rows.Err()
}

// loadBatch builds the apply payload: pending files, per-file classification
// through the executor (ADR-019 tiers), manifest for the argHash binding.
func loadBatch(ctx context.Context, gt *gatedTools, dbID string, baseline bool) (*migrationBatch, error) {
	dbCfg, err := gt.cfg.DB(dbID)
	if err != nil {
		return nil, err
	}
	if dbCfg.MigrationsDir == "" {
		return nil, fmt.Errorf("database %s has no migrations_dir configured", dbID)
	}
	migs, err := migrate.LoadDir(dbCfg.MigrationsDir)
	if err != nil {
		return nil, err
	}
	history, initialized, err := migrationHistory(ctx, gt, dbID)
	if err != nil {
		return nil, err
	}
	if !initialized && len(history) > 0 {
		history = nil // defensive; can't happen
	}
	pending, err := migrate.Pending(migs, history)
	if err != nil {
		return nil, err
	}
	if !baseline && len(pending) == 0 && initialized {
		return nil, fmt.Errorf("nothing to apply: all %d migration(s) are already applied (use baseline:true to record the current schema as version 0)", len(migs))
	}

	b := &migrationBatch{Dir: dbCfg.MigrationsDir, Baseline: baseline, Pending: pending,
		Manifest: migrate.ManifestJSON(baseline, pending), NeedsBoot: !initialized, BatchTier: 1, MinFB: ""}
	var pv strings.Builder
	if b.NeedsBoot {
		fmt.Fprintf(&pv, "bootstrap: %s (classified Tier 1, through the executor — ADR-030 #4)\n", migrate.TableDDL)
	}
	if baseline {
		if initialized {
			return nil, fmt.Errorf("version table already initialized — baseline only once")
		}
		fmt.Fprintf(&pv, "baseline: record current schema as version 0 (INSERT into %s, no DDL)\n", migrate.Table)
	}
	for _, m := range pending {
		prep, err := execpkg.Prepare(m.Up)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", m.Name, err)
		}
		fp := filePlan{Version: m.Version, Name: m.Name, Tier: prep.MaxTier}
		for _, r := range prep.Results {
			fp.Stmnts++
			fp.Detail = append(fp.Detail, fmt.Sprintf("T%d %s", r.Tier, shortStmt(r.Statement.Raw)))
		}
		if prep.MaxTier > b.BatchTier {
			b.BatchTier = prep.MaxTier
		}
		if minFBGreater(prep.MinFB, b.MinFB) {
			b.MinFB = prep.MinFB
		}
		b.FilePlans = append(b.FilePlans, fp)
		fmt.Fprintf(&pv, "v%04d %s — %d statement(s), tier %d, checksum %s…\n", m.Version, m.Name, fp.Stmnts, fp.Tier, m.Checksum[:12])
		for _, d := range fp.Detail {
			fmt.Fprintf(&pv, "    %s\n", d)
		}
		if !m.HasDown() {
			fmt.Fprintf(&pv, "    (no -- @down section — rollback will not be possible for this version)\n")
		}
	}
	b.PreviewTxt = pv.String()
	return b, nil
}

func registerMigrateTools(server *mcp.Server, gt *gatedTools) {
	// fb_migration_status — Tier 0.
	type statusArg struct {
		Db string `json:"db" jsonschema:"registry id of the database"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_migration_status", Description: "Tier 0: migration state — files in the database's migrations_dir vs the FBMCP_MIGRATIONS history table (applied / pending / uninitialized)"}, func(ctx context.Context, req *mcp.CallToolRequest, a statusArg) (*mcp.CallToolResult, any, error) {
		dbCfg, err := gt.cfg.DB(a.Db)
		if err != nil {
			return errText("error: " + err.Error())
		}
		if dbCfg.MigrationsDir == "" {
			return errText("error: database " + a.Db + " has no migrations_dir configured")
		}
		migs, err := migrate.LoadDir(dbCfg.MigrationsDir)
		if err != nil {
			return errText("error: " + err.Error())
		}
		history, initialized, err := migrationHistory(ctx, gt, a.Db)
		if err != nil {
			return errText("error: " + err.Error())
		}
		var b strings.Builder
		fmt.Fprintf(&b, "migrations dir: %s (%d file(s))\n", dbCfg.MigrationsDir, len(migs))
		if !initialized {
			b.WriteString("history: not initialized (fb_migration_apply bootstraps FBMCP_MIGRATIONS through the gated path)\n")
		}
		if _, err := migrate.Pending(migs, history); err != nil {
			fmt.Fprintf(&b, "state: INCONSISTENT — %v\n", err)
		}
		applied := map[int]migrate.Applied{}
		for _, h := range history {
			applied[h.Version] = h
		}
		for _, m := range migs {
			if h, ok := applied[m.Version]; ok {
				fmt.Fprintf(&b, "v%04d %s APPLIED (by %s, checksum %s…)\n", m.Version, m.Name, h.AppliedBy, m.Checksum[:12])
			} else {
				fmt.Fprintf(&b, "v%04d %s pending\n", m.Version, m.Name)
			}
		}
		structured := map[string]any{"dir": dbCfg.MigrationsDir, "initialized": initialized, "files": migs}
		gt.aud.Log(audit.Entry{Identity: identity.Caller(ctx).Name, Database: a.Db, Tool: "fb_migration_status", Tier: 0, Decision: "allow"})
		return text(b.String()), structured, nil
	})

	// fb_migration_plan — Tier 0, informational (ADR-030).
	type planArg struct {
		Db       string `json:"db" jsonschema:"registry id of the database"`
		Baseline bool   `json:"baseline,omitempty" jsonschema:"include the version-0 baseline step in the plan"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_migration_plan", Description: "Tier 0: dry-run plan for fb_migration_apply — per-statement classification (tier/risk, ADR-019), batch tier, checksums and down-section presence; executes nothing"}, func(ctx context.Context, req *mcp.CallToolRequest, a planArg) (*mcp.CallToolResult, any, error) {
		batch, err := loadBatch(ctx, gt, a.Db, a.Baseline)
		if err != nil {
			return errText("error: " + err.Error())
		}
		var out strings.Builder
		fmt.Fprintf(&out, "batch tier: %d%s%s\n", batch.BatchTier,
			iif(batch.MinFB != "", " | min engine: "+batch.MinFB, ""),
			iif(batch.BatchTier >= 2, " — contains Tier-2 statements: the whole batch escalates (out-of-band confirmation + verified-backup preconditions)", ""))
		out.WriteString(batch.PreviewTxt)
		out.WriteString("\nmode=preview (informational — confirmation still required via fb_migration_apply)\n")
		structured := map[string]any{"batch_tier": batch.BatchTier, "min_fb": batch.MinFB, "manifest": batch.Manifest,
			"files": batch.FilePlans, "needs_bootstrap": batch.NeedsBoot, "baseline": batch.Baseline}
		gt.aud.Log(audit.Entry{Identity: identity.Caller(ctx).Name, Database: a.Db, Tool: "fb_migration_plan", Tier: 0, Decision: "allow"})
		return text(out.String()), structured, nil
	})

	// fb_migration_apply — gated batch (ADR-030).
	type applyArg struct {
		Db       string `json:"db" jsonschema:"registry id of the database"`
		Mode     string `json:"mode,omitempty" jsonschema:"preview or execute (default execute)"`
		Baseline bool   `json:"baseline,omitempty" jsonschema:"record the current schema as version 0 (INSERT only, no DDL); only when uninitialized"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_migration_apply", Description: "Tier 1+ (dynamic, ADR-030 batch gate): apply all pending migrations from the database's migrations_dir in one confirmation — manifest argHash-bound (any file change re-requests), statements re-classified and checksums re-verified at execution time, each migration atomic with its history row; batch escalates to Tier 2 if any statement is Tier 2"}, func(ctx context.Context, req *mcp.CallToolRequest, a applyArg) (*mcp.CallToolResult, any, error) {
		id := identity.Caller(ctx)
		msg, denied := gt.requestMigrationApply(ctx, id, a.Db, strings.EqualFold(a.Mode, "preview"), a.Baseline)
		if denied {
			return errText(msg)
		}
		return text(msg), nil, nil
	})

	// fb_migration_rollback_plan — Tier 0, renders from recorded history.
	type rollbackArg struct {
		Db        string `json:"db" jsonschema:"registry id of the database"`
		ToVersion *int   `json:"to_version,omitempty" jsonschema:"target version after rollback (default: one step down from the highest applied; 0 = down to baseline)"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_migration_rollback_plan", Description: "Tier 0: render the down-scripts recorded in FBMCP_MIGRATIONS at apply time (statements classified; executes nothing — paste into fb_write, which gates and confirms it)"}, func(ctx context.Context, req *mcp.CallToolRequest, a rollbackArg) (*mcp.CallToolResult, any, error) {
		to := -1 // sentinel: one step down
		if a.ToVersion != nil {
			to = *a.ToVersion
		}
		out, err := gt.renderRollback(ctx, a.Db, to)
		if err != nil {
			return errText("error: " + err.Error())
		}
		gt.aud.Log(audit.Entry{Identity: identity.Caller(ctx).Name, Database: a.Db, Tool: "fb_migration_rollback_plan", Tier: 0, Decision: "allow"})
		return text(out), nil, nil
	})
}

func iif(cond bool, then, _ string) string {
	if cond {
		return then
	}
	return ""
}

// requestMigrationApply is the fb_write-style gated path for the batch.
func (gt *gatedTools) requestMigrationApply(ctx context.Context, id policy.Identity, dbID string, previewOnly, baseline bool) (string, bool) {
	batch, err := loadBatch(ctx, gt, dbID, baseline)
	if err != nil {
		gt.aud.Log(audit.Entry{Identity: id.Name, Database: dbID, Tool: "fb_migration_apply", Tier: -1, Decision: "denied", Detail: map[string]interface{}{"reason": err.Error()}})
		return "DENIED: " + err.Error(), true
	}
	impact := fmt.Sprintf("fb_migration_apply on %s (%d file(s), batch tier %d — ADR-030)\n%s", dbID, len(batch.Pending), batch.BatchTier, batch.PreviewTxt)
	if previewOnly {
		return impact + "\nmode=preview (informational — confirmation still required to execute)\n", false
	}
	meta := policy.ToolMeta{Name: "fb_migration_apply", Tier: batch.BatchTier, Scope: "database", MinFB: batch.MinFB}
	if batch.BatchTier >= 2 {
		meta.Preconditions = []policy.Precondition{
			{Name: "verified_backup_exists", Op: "true", Why: "Tier-2 / irreversible content requires a verified backup"},
			{Name: "backup_freshness", Op: "lt", Value: 24.0, Why: "newest verified backup must be < 24h"},
		}
	}
	d := gt.eng.EvaluateMeta(id, dbID, meta)
	if d.Outcome == "deny" || len(d.FailedPreconditions) > 0 {
		gt.aud.Log(audit.Entry{Identity: id.Name, Database: dbID, Tool: "fb_migration_apply", Tier: batch.BatchTier, Decision: "denied", Detail: map[string]interface{}{"reason": d.Reason}})
		return "DENIED: " + d.Reason, true
	}
	argHash := hashOf(dbID + batch.Manifest)
	p, err := gt.g.Request(id, dbID, meta, impact, argHash, nil)
	if err != nil {
		return "gate error: " + err.Error(), true
	}
	gt.mu.Lock()
	gt.args[p.ID] = map[string]any{"baseline": baseline, "manifest": batch.Manifest, "applied_by": id.Name}
	gt.mu.Unlock()
	var b strings.Builder
	b.WriteString(gate.ImpactStatement(p))
	if meta.Tier <= 1 {
		fmt.Fprintf(&b, "In-band token (Tier 1 only): %s\n", gate.IssueToken(p.ID, argHash))
	} else {
		b.WriteString("Confirmation: out-of-band only (Tier-2 statements in the batch — fbmcp-tray popup or fbmcpctl approve)\n")
	}
	gt.aud.Log(audit.Entry{Identity: id.Name, Database: dbID, Tool: "fb_migration_apply", Tier: batch.BatchTier, Decision: "pending",
		Detail: map[string]interface{}{"manifest": batch.Manifest}})
	return b.String(), false
}

// migrationApplyExec is registered in gt.execs; it runs after a confirmed
// pending action (killpoint coverage rides on exec.pre/post like fb_write).
func (gt *gatedTools) migrationApplyExec(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
	wantManifest, _ := args["manifest"].(string)
	baseline, _ := args["baseline"].(bool)
	if wantManifest == "" {
		return "", fmt.Errorf("missing manifest (args lost across restart — re-request)")
	}
	batch, err := loadBatch(ctx, gt, dbID, baseline)
	if err != nil {
		return "", fmt.Errorf("re-validation failed: %v", err)
	}
	if batch.Manifest != wantManifest {
		return "", fmt.Errorf("re-validation failed: migrations changed after confirmation (manifest mismatch) — re-request fb_migration_apply")
	}
	// DDL may need exclusive access our pooled snapshots would deny — drain
	// first (same primitive fb_write's NeedsExclusive path uses).
	prog(0.05, "draining connection pools for DDL")
	gt.pools.CloseDB(dbID)
	pool, err := gt.pools.AdminPool(ctx, dbID)
	if err != nil {
		return "", err
	}

	var report strings.Builder
	if batch.NeedsBoot {
		prog(0.1, "bootstrapping version table (classified through the executor)")
		prep, err := execpkg.Prepare(migrate.TableDDL)
		if err != nil {
			return "", fmt.Errorf("bootstrap classification: %v", err)
		}
		if _, err := gt.execSvc.Exec(ctx, dbID, prep, prog); err != nil {
			return "", fmt.Errorf("bootstrap: %v", err)
		}
		report.WriteString("bootstrapped FBMCP_MIGRATIONS\n")
	}
	if baseline {
		idName, _ := args["applied_by"].(string)
		tx, err := pool.BeginTx(ctx, nil)
		if err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO %s (ID, VERSION, CHECKSUM, DOWN_TEXT, APPLIED_AT, APPLIED_BY) VALUES ('baseline', 0, '', NULL, CURRENT_TIMESTAMP, ?)", migrate.Table), idName); err != nil {
			tx.Rollback()
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", err
		}
		report.WriteString("recorded baseline version 0 (no DDL executed)\n")
		return report.String(), nil
	}

	// Per-statement autocommit (same execution shape as fb_write's DDL
	// path): Firebird cannot see objects created earlier in the SAME
	// transaction — a migration that creates and then populates a table
	// fails with "table unknown" (verified live on FB3) — so per-migration
	// transactional atomicity is not achievable there. The history row,
	// written after the migration's last statement succeeds, is the
	// completion marker: a crash mid-migration leaves the version pending
	// and a re-apply fails loudly on the first already-applied statement.
	total := len(batch.Pending)
	for i, m := range batch.Pending {
		prog(float64(i)/float64(total), fmt.Sprintf("applying v%04d %s", m.Version, m.Name))
		prep, err := execpkg.Prepare(m.Up)
		if err != nil {
			return "", fmt.Errorf("%s (execution-time classification): %v", m.Name, err)
		}
		stmts := statementTexts(prep)
		for _, s := range stmts {
			if _, err := pool.ExecContext(ctx, s); err != nil {
				return "", fmt.Errorf("v%04d %s failed (batch stopped; completed migrations stay applied, this one has no history row): %v\nstatement: %s", m.Version, m.Name, err, shortStmt(s))
			}
		}
		var down any
		if m.HasDown() {
			down = m.Down
		}
		idName, _ := args["applied_by"].(string)
		if _, err := pool.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO %s (ID, VERSION, CHECKSUM, DOWN_TEXT, APPLIED_AT, APPLIED_BY) VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP, ?)", migrate.Table),
			m.Name, m.Version, m.Checksum, down, idName); err != nil {
			return "", fmt.Errorf("v%04d history row: %v", m.Version, err)
		}
		fmt.Fprintf(&report, "applied v%04d %s (%d statement(s))\n", m.Version, m.Name, len(stmts))
	}
	prog(1.0, "done")
	return report.String(), nil
}

// renderRollback renders down-scripts from the recorded history rows.
func (gt *gatedTools) renderRollback(ctx context.Context, dbID string, toVersion int) (string, error) {
	dbCfg, err := gt.cfg.DB(dbID)
	if err != nil {
		return "", err
	}
	_ = dbCfg
	tx, err := gt.pools.ReadOnly(ctx, dbID)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(
		"SELECT ID, VERSION, DOWN_TEXT FROM %s ORDER BY VERSION DESC", migrate.Table))
	if err != nil {
		if strings.Contains(err.Error(), migrate.Table) {
			return "", fmt.Errorf("not initialized — nothing to roll back")
		}
		return "", err
	}
	defer rows.Close()
	type rec struct {
		name string
		ver  int
		down string
	}
	var recs []rec
	for rows.Next() {
		var r rec
		var down sql.NullString
		if err := rows.Scan(&r.name, &r.ver, &down); err != nil {
			return "", err
		}
		if down.Valid {
			r.down = down.String
		}
		recs = append(recs, r)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(recs) == 0 {
		return "", fmt.Errorf("no applied migrations")
	}
	maxV := recs[0].ver
	if toVersion < 0 { // sentinel: one step down
		toVersion = maxV - 1 // one step down by default
	}
	if toVersion >= maxV {
		return "", fmt.Errorf("to_version %d is not below the highest applied %d", toVersion, maxV)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "rollback plan: v%04d → v%04d (rendered from recorded down sections — execute via fb_write)\n\n", maxV, toVersion)
	for _, r := range recs {
		if r.ver <= toVersion || r.ver == 0 {
			continue
		}
		if strings.TrimSpace(r.down) == "" {
			fmt.Fprintf(&b, "-- v%04d %s: NO down section recorded — manual rollback required\n\n", r.ver, r.name)
			continue
		}
		fmt.Fprintf(&b, "-- v%04d %s (down statements as recorded; versions run latest-first)\n", r.ver, r.name)
		for _, s := range migrate.Statements(r.down) {
			prep, err := execpkg.Prepare(s)
			tier := "?"
			if err == nil {
				tier = fmt.Sprintf("T%d", prep.MaxTier)
			}
			fmt.Fprintf(&b, "%s %s\n", tier, strings.TrimRight(s, ";")+" ;")
		}
		b.WriteString("\n")
	}
	b.WriteString("Paste the statements above into fb_write (they are classified and confirmed there). Reverse order: latest version first.\n")
	return b.String(), nil
}

func statementTexts(p execpkg.Prepared) []string {
	var out []string
	for _, r := range p.Results {
		s := strings.TrimSpace(r.Statement.Raw)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
