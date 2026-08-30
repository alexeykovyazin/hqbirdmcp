// Phase-4 tools (P4.1–P4.7) on the gate→job pattern; fb_write carries a
// DYNAMIC tier computed from statement classification (ADR-019/021).
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/classify"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/configedit"
	execpkg "github.com/aleks/fbmcp/internal/executor"
	"github.com/aleks/fbmcp/internal/gate"
	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/killpoint"
	"github.com/aleks/fbmcp/internal/lockout"
	"github.com/aleks/fbmcp/internal/policy"
	"github.com/aleks/fbmcp/internal/privs"
	"github.com/aleks/fbmcp/internal/state"
	"github.com/aleks/fbmcp/internal/workflows"
)

func registerP4Tools(server *mcp.Server, gt *gatedTools) {
	registerFBWrite(server, gt)
	registerQueryTool(server, gt)
	registerMigrateTools(server, gt)
	gt.execs["fb_migration_apply"] = gt.migrationApplyExec
	registerIndexTools(server, gt)
	registerSecurityTools(server, gt)
	registerEffectiveAccess(server, gt)
	registerSessionKill(server, gt)
	registerComment(server, gt)
	registerLifecycle(server, gt)
	registerShutdown(server, gt)
	registerConfigTools(server, gt)
	registerWindowOpen(server, gt)
}

func registerFBWrite(server *mcp.Server, gt *gatedTools) {
	type writeArg struct {
		Db   string `json:"db"`
		SQL  string `json:"sql" jsonschema:"one or more DML/DDL/DCL statements"`
		Mode string `json:"mode,omitempty" jsonschema:"preview or execute (default execute)"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_write", Description: "Tier 1+ (dynamic) gated: classified statement script; mode=preview|execute; executed on the admin pool"}, func(ctx context.Context, req *mcp.CallToolRequest, a writeArg) (*mcp.CallToolResult, any, error) {
		// WS2.1 fix (kept by requestWrite): the identity comes from the
		// caller context, never a hardcoded "local" — remote API-key calls
		// keep their max_tier ceiling and can confirm their own pendings.
		id := identity.Caller(ctx)
		msg, denied := gt.requestWrite(ctx, id, a.Db, a.SQL, strings.EqualFold(a.Mode, "preview"))
		if denied {
			return errText(msg)
		}
		return text(msg), nil, nil
	})

	gt.execs["fb_write"] = func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
		sqlText, _ := args["sql"].(string)
		if sqlText == "" {
			return "", fmt.Errorf("missing sql (args lost across restart — re-request)")
		}
		prep, err := execpkg.Prepare(sqlText)
		if err != nil {
			return "", err
		}
		if prep.NeedsExclusive {
			// WS3.1: our own pooled connections hold snapshot transactions
			// the engine counts as concurrent use — an exclusive-mode
			// REFRESH MATERIALIZED VIEW then fails with "in use by
			// concurrent transaction". Drain this DB's pools first (same
			// primitive restore_replace uses); Exec reopens fresh.
			prog(0.05, "draining connection pools (exclusive reservation required)")
			gt.pools.CloseDB(dbID)
		}
		return gt.execSvc.Exec(ctx, dbID, prep, prog)
	}
}

// requestWrite is the fb_write path (also the fb_query fallback for
// EXECUTE PROCEDURE calls the engine refuses on the read-only transaction):
// classify → preview short-circuit → policy → pending action. It never
// executes; confirmation dispatches to gt.execs["fb_write"].
func (gt *gatedTools) requestWrite(ctx context.Context, id policy.Identity, dbID, sqlText string, previewOnly bool) (string, bool) {
	if _, err := gt.cfg.DB(dbID); err != nil {
		return "DENIED: " + err.Error(), true
	}
	prep, err := execpkg.Prepare(sqlText)
	if err != nil {
		gt.aud.Log(audit.Entry{Identity: id.Name, Database: dbID, Tool: "fb_write", Tier: -1, Decision: "denied", Detail: map[string]interface{}{"reason": err.Error()}})
		return "DENIED: " + err.Error(), true
	}
	impact := "fb_write on " + dbID + "\n" + gt.execSvc.Impact(ctx, dbID, prep)
	if previewOnly {
		return impact + "\nmode=preview (informational — confirmation still required to execute)\n", false
	}
	meta := policy.ToolMeta{Name: "fb_write", Tier: prep.MaxTier, Scope: "database", MinFB: prep.MinFB}
	if prep.MaxTier >= 2 {
		meta.Preconditions = []policy.Precondition{
			{Name: "verified_backup_exists", Op: "true", Why: "Tier-2 / irreversible content requires a verified backup"},
			{Name: "backup_freshness", Op: "lt", Value: 24.0, Why: "newest verified backup must be < 24h"},
		}
	}
	d := gt.eng.EvaluateMeta(id, dbID, meta)
	if d.Outcome == "deny" || len(d.FailedPreconditions) > 0 {
		why := d.Reason
		gt.aud.Log(audit.Entry{Identity: id.Name, Database: dbID, Tool: "fb_write", Tier: prep.MaxTier, Decision: "denied", Detail: map[string]interface{}{"reason": why, "templates": classify.Template(sqlText)}})
		return "DENIED: " + why, true
	}
	argHash := hashOf(dbID + sqlText)
	p, err := gt.g.Request(id, dbID, meta, impact, argHash, nil)
	if err != nil {
		return "gate error: " + err.Error(), true
	}
	gt.mu.Lock()
	gt.args[p.ID] = map[string]any{"sql": sqlText}
	gt.mu.Unlock()
	var b strings.Builder
	b.WriteString(gate.ImpactStatement(p))
	if meta.Tier <= 1 {
		fmt.Fprintf(&b, "In-band token (Tier 1 only): %s\n", gate.IssueToken(p.ID, argHash))
	} else {
		b.WriteString("Confirmation: out-of-band only (fbmcp-tray popup or fbmcpctl approve)\n")
	}
	gt.aud.Log(audit.Entry{Identity: id.Name, Database: dbID, Tool: "fb_write", Tier: prep.MaxTier, Decision: "pending", Detail: map[string]interface{}{"templates": classify.Template(sqlText)}})
	return b.String(), false
}

func registerIndexTools(server *mcp.Server, gt *gatedTools) {
	gt.registerTool(server, policy.ToolMeta{Name: "fb_index_rebuild", Tier: 1, Scope: "database"},
		"Rebuild (INACTIVE+ACTIVE) or SET STATISTICS on an index of %s (args: {\"index\":\"NAME\",\"action\":\"rebuild|statistics\",\"advisory_id\":\"adv…\"}).",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			idx, adv, err := resolveIndexArg(gt, dbID, "fb_index_rebuild", args)
			if err != nil {
				return "", err
			}
			action, _ := args["action"].(string)
			if action == "" {
				action = "rebuild"
			}
			if err := refuseConstraintIndex(ctx, gt, dbID, idx, action); err != nil {
				return "", err
			}
			var sql string
			switch strings.ToLower(action) {
			case "statistics":
				sql = "SET STATISTICS INDEX " + qident(idx)
			case "rebuild":
				sql = "ALTER INDEX " + qident(idx) + " INACTIVE; ALTER INDEX " + qident(idx) + " ACTIVE"
			default:
				return "", fmt.Errorf("action must be rebuild or statistics")
			}
			msg, err := runWrite(ctx, gt, dbID, sql, prog)
			if err != nil {
				return "", err
			}
			if adv != "" {
				gt.aud.Log(audit.Entry{Identity: "local", Database: dbID, Tool: "fb_index_rebuild", Tier: 1, Decision: "approved",
					Detail: map[string]interface{}{"advisory_id": adv, "index": idx}})
			}
			return msg, nil
		})

	gt.registerTool(server, policy.ToolMeta{Name: "fb_index_drop", Tier: 1, Scope: "database"},
		"DROP INDEX on %s (args: {\"index\":\"NAME\",\"advisory_id\":\"adv…\"}); refused if the index backs a constraint.",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			idx, adv, err := resolveIndexArg(gt, dbID, "fb_index_drop", args)
			if err != nil {
				return "", err
			}
			if err := refuseConstraintIndex(ctx, gt, dbID, idx, "drop"); err != nil {
				return "", err
			}
			msg, err := runWrite(ctx, gt, dbID, "DROP INDEX "+qident(idx), prog)
			if err != nil {
				return "", err
			}
			if adv != "" {
				gt.aud.Log(audit.Entry{Identity: "local", Database: dbID, Tool: "fb_index_drop", Tier: 1, Decision: "approved",
					Detail: map[string]interface{}{"advisory_id": adv, "index": idx}})
			}
			return msg, nil
		})
}

func resolveIndexArg(gt *gatedTools, dbID, tool string, args map[string]any) (index, advisoryID string, err error) {
	idx, _ := args["index"].(string)
	adv, _ := args["advisory_id"].(string)
	if adv != "" {
		a, ok := gt.st.Advisory(adv)
		if !ok {
			return "", "", fmt.Errorf("unknown advisory_id %s", adv)
		}
		if a.Database != dbID || a.Tool != tool {
			return "", "", fmt.Errorf("advisory %s does not apply to %s on %s", adv, tool, dbID)
		}
		if idx != "" && !strings.EqualFold(idx, a.Object) {
			return "", "", fmt.Errorf("advisory object %s != index %s", a.Object, idx)
		}
		idx = a.Object
	}
	if !isIdentifier(idx) {
		return "", "", fmt.Errorf("invalid index name")
	}
	return idx, adv, nil
}

func refuseConstraintIndex(ctx context.Context, gt *gatedTools, dbID, idx, action string) error {
	if action != "drop" {
		return nil
	}
	tx, err := gt.pools.ReadOnly(ctx, dbID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM RDB$RELATION_CONSTRAINTS WHERE TRIM(RDB$INDEX_NAME) = ?`, strings.ToUpper(idx)).Scan(&n)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("index %s backs a constraint — drop the constraint via fb_write DDL instead", idx)
	}
	return nil
}

func registerSecurityTools(server *mcp.Server, gt *gatedTools) {
	gt.registerTool(server, policy.ToolMeta{Name: "fb_user_create", Tier: 1, Scope: "database"},
		"CREATE USER on %s (args: {\"user\":\"NAME\",\"password\":\"...\"}). Password is never audited.",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			user, _ := args["user"].(string)
			pass, _ := args["password"].(string)
			if !isIdentifier(user) || pass == "" {
				return "", fmt.Errorf("user (identifier) and password required")
			}
			sql := fmt.Sprintf("CREATE USER %s PASSWORD '%s'", qident(user), escapeLit(pass))
			msg, err := runWrite(ctx, gt, dbID, sql, prog)
			if err != nil {
				return "", err
			}
			return "user created; password delivered out-of-band (not logged). " + scrubPass(msg, pass), nil
		})

	gt.registerTool(server, policy.ToolMeta{Name: "fb_user_drop", Tier: 1, Scope: "database"},
		"DROP USER on %s (args: {\"user\":\"NAME\"}). Lockout guards apply.",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			user, _ := args["user"].(string)
			if !isIdentifier(user) {
				return "", fmt.Errorf("invalid user")
			}
			if err := lockoutUser(gt, dbID, user); err != nil {
				return "", err
			}
			return runWrite(ctx, gt, dbID, "DROP USER "+qident(user), prog)
		})

	gt.registerTool(server, policy.ToolMeta{Name: "fb_role_create", Tier: 1, Scope: "database"},
		"CREATE ROLE on %s (args: {\"role\":\"NAME\"}).",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			role, _ := args["role"].(string)
			if !isIdentifier(role) {
				return "", fmt.Errorf("invalid role")
			}
			return runWrite(ctx, gt, dbID, "CREATE ROLE "+qident(role), prog)
		})

	gt.registerToolEx(server, policy.ToolMeta{Name: "fb_grant", Tier: 1, Scope: "database"},
		"GRANT privilege on %s (args: {\"privilege\":\"SELECT\",\"on\":\"TABLE\",\"name\":\"T\",\"to\":\"U\"}).",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			sql, err := grantSQL("GRANT", args)
			if err != nil {
				return "", err
			}
			return runWrite(ctx, gt, dbID, sql, prog)
		}, grantPreview(gt, "GRANT"))

	gt.registerToolEx(server, policy.ToolMeta{Name: "fb_revoke", Tier: 1, Scope: "database"},
		"REVOKE privilege on %s (args: {\"privilege\":\"SELECT\",\"on\":\"TABLE\",\"name\":\"T\",\"from\":\"U\"}).",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			to, _ := args["from"].(string)
			if to == "" {
				to, _ = args["to"].(string)
			}
			if err := lockoutRevoke(gt, dbID, to); err != nil {
				return "", err
			}
			sql, err := grantSQL("REVOKE", args)
			if err != nil {
				return "", err
			}
			return runWrite(ctx, gt, dbID, sql, prog)
		}, grantPreview(gt, "REVOKE"))
}

func grantSQL(verb string, args map[string]any) (string, error) {
	priv, _ := args["privilege"].(string)
	obj, _ := args["on"].(string)
	name, _ := args["name"].(string)
	grantee, _ := args["to"].(string)
	if verb == "REVOKE" {
		if g, _ := args["from"].(string); g != "" {
			grantee = g
		}
	}
	if priv == "" || !isIdentifier(name) || !isIdentifier(grantee) {
		return "", fmt.Errorf("privilege, name, and grantee required")
	}
	priv = strings.ToUpper(priv)
	switch priv {
	case "SELECT", "INSERT", "UPDATE", "DELETE", "EXECUTE", "ALL", "USAGE":
	default:
		return "", fmt.Errorf("unsupported privilege")
	}
	prep := "ON"
	if obj != "" {
		prep = "ON " + strings.ToUpper(obj)
	}
	tail := "TO"
	if verb == "REVOKE" {
		tail = "FROM"
	}
	return fmt.Sprintf("%s %s %s %s %s %s", verb, priv, prep, qident(name), tail, qident(grantee)), nil
}

func lockoutUser(gt *gatedTools, dbID, user string) error {
	db, err := gt.cfg.DB(dbID)
	if err != nil {
		return err
	}
	return lockout.DropUser(db.AdminUser, db.ROUser, user)
}

func lockoutRevoke(gt *gatedTools, dbID, grantee string) error {
	db, err := gt.cfg.DB(dbID)
	if err != nil {
		return err
	}
	return lockout.Revoke(db.AdminUser, db.ROUser, grantee)
}

func grantPreview(gt *gatedTools, verb string) func(context.Context, string, map[string]any) string {
	return func(ctx context.Context, dbID string, args map[string]any) string {
		priv, _ := args["privilege"].(string)
		name, _ := args["name"].(string)
		grantee, _ := args["to"].(string)
		if verb == "REVOKE" {
			if g, _ := args["from"].(string); g != "" {
				grantee = g
			}
		}
		if grantee == "" {
			return ""
		}
		if verb == "REVOKE" {
			if err := lockoutRevoke(gt, dbID, grantee); err != nil {
				return "lockout: " + err.Error()
			}
		}
		before, err := loadGrants(ctx, gt, dbID, grantee)
		if err != nil {
			return "effective-access unavailable: " + err.Error()
		}
		after := privs.ApplyPreview(before, verb, priv, name, grantee)
		added, removed := privs.Diff(before, after)
		var b strings.Builder
		fmt.Fprintf(&b, "effective access for %s (cap 200 rows, no role expansion):\n%s\n", grantee, privs.Format(before))
		if len(added) > 0 {
			fmt.Fprintf(&b, "after (+): %s\n", strings.Join(added, ", "))
		}
		if len(removed) > 0 {
			fmt.Fprintf(&b, "after (-): %s\n", strings.Join(removed, ", "))
		}
		return b.String()
	}
}

func loadGrants(ctx context.Context, gt *gatedTools, dbID, user string) ([]privs.Grant, error) {
	tx, err := gt.pools.ReadOnly(ctx, dbID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT TRIM(RDB$USER), TRIM(RDB$PRIVILEGE), TRIM(RDB$RELATION_NAME),
		       TRIM(COALESCE(RDB$FIELD_NAME,'')), RDB$GRANT_OPTION
		FROM RDB$USER_PRIVILEGES
		WHERE TRIM(RDB$USER) = ?
		ROWS 200`, strings.ToUpper(user))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []privs.Grant
	for rows.Next() {
		var g privs.Grant
		var goFlag int
		if err := rows.Scan(&g.User, &g.Privilege, &g.Relation, &g.Field, &goFlag); err != nil {
			break
		}
		g.GrantOption = goFlag != 0
		out = append(out, g)
	}
	return out, nil
}

func registerEffectiveAccess(server *mcp.Server, gt *gatedTools) {
	type accArg struct {
		Db   string `json:"db"`
		User string `json:"user" jsonschema:"user or role name"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_effective_access", Description: "Tier 0: resolved object privileges for a user/role (RDB$USER_PRIVILEGES, cap 200, no role expansion)"}, func(ctx context.Context, req *mcp.CallToolRequest, a accArg) (*mcp.CallToolResult, any, error) {
		if !isIdentifier(a.User) {
			return text("invalid user"), nil, nil
		}
		gs, err := loadGrants(ctx, gt, a.Db, a.User)
		if err != nil {
			return errText("error: " + err.Error())
		}
		return text(privs.Format(gs)), nil, nil
	})
}

func registerSessionKill(server *mcp.Server, gt *gatedTools) {
	gt.registerTool(server, policy.ToolMeta{Name: "fb_session_kill", Tier: 1, Scope: "database"},
		"Terminate attachment(s) on %s (args: {\"attachment_id\": N} or {\"attachment_ids\": [N,…]}).",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			ids := attachmentIDs(args)
			if len(ids) == 0 {
				return "", fmt.Errorf("attachment_id or attachment_ids required")
			}
			pool, err := gt.pools.AdminPool(ctx, dbID)
			if err != nil {
				return "", err
			}
			var own int64
			_ = pool.QueryRowContext(ctx, "SELECT CURRENT_CONNECTION FROM RDB$DATABASE").Scan(&own)
			var b strings.Builder
			killed, failed := 0, 0
			for _, id := range ids {
				if isOwnAttachment(own, id) {
					fmt.Fprintf(&b, "%d: refused (server's own connection)\n", id)
					failed++
					continue
				}
				res, err := pool.ExecContext(ctx, "DELETE FROM MON$ATTACHMENTS WHERE MON$ATTACHMENT_ID = ?", id)
				if err != nil {
					fmt.Fprintf(&b, "%d: error %v\n", id, err)
					failed++
					continue
				}
				n, _ := res.RowsAffected()
				if n == 0 {
					fmt.Fprintf(&b, "%d: not found\n", id)
					failed++
					continue
				}
				fmt.Fprintf(&b, "%d: terminated\n", id)
				killed++
			}
			return fmt.Sprintf("killed %d, failed %d\n%s", killed, failed, b.String()), nil
		})
}

func isOwnAttachment(own, id int64) bool { return own != 0 && id == own }

func attachmentIDs(args map[string]any) []int64 {
	if n, ok := toInt(args["attachment_id"]); ok {
		return []int64{n}
	}
	switch v := args["attachment_ids"].(type) {
	case []any:
		var out []int64
		for _, x := range v {
			if n, ok := toInt(x); ok {
				out = append(out, n)
			}
		}
		return out
	}
	return nil
}

func registerComment(server *mcp.Server, gt *gatedTools) {
	gt.registerTool(server, policy.ToolMeta{Name: "fb_comment_set", Tier: 1, Scope: "database"},
		"COMMENT ON a table/column of %s (args: {\"on\":\"TABLE|COLUMN\",\"name\":\"T\",\"column\":\"C\",\"text\":\"...\"}).",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			on, _ := args["on"].(string)
			name, _ := args["name"].(string)
			col, _ := args["column"].(string)
			txt, _ := args["text"].(string)
			if !isIdentifier(name) {
				return "", fmt.Errorf("invalid name")
			}
			var sql string
			switch strings.ToUpper(on) {
			case "COLUMN":
				if !isIdentifier(col) {
					return "", fmt.Errorf("column required")
				}
				sql = fmt.Sprintf("COMMENT ON COLUMN %s.%s IS '%s'", qident(name), qident(col), escapeLit(txt))
			default:
				sql = fmt.Sprintf("COMMENT ON TABLE %s IS '%s'", qident(name), escapeLit(txt))
			}
			return runWrite(ctx, gt, dbID, sql, prog)
		})
}

func registerLifecycle(server *mcp.Server, gt *gatedTools) {
	gt.registerTool(server, policy.ToolMeta{Name: "fb_db_create", Tier: 1, Scope: "database"},
		"Create a database file by copying the registered template %s into its work dir (args: {\"filename\":\"name.fdb\"}). Add a config entry to manage it.",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			db, err := gt.cfg.DB(dbID)
			if err != nil {
				return "", err
			}
			fn, _ := args["filename"].(string)
			if fn == "" || strings.ContainsAny(fn, `/\`) || strings.Contains(fn, "..") {
				return "", fmt.Errorf("filename must be a bare name (no path)")
			}
			work := db.WorkDir
			if work == "" {
				work = filepath.Dir(db.Path)
			}
			dst := filepath.Join(work, fn)
			if _, err := os.Stat(dst); err == nil {
				return "", fmt.Errorf("refusing to overwrite %s", dst)
			}
			prog(0.2, "copying template")
			if err := copyFile(db.Path, dst); err != nil {
				return "", err
			}
			return fmt.Sprintf("created %s from template %s — add a databases: entry to fbmcp.yaml to manage it", dst, dbID), nil
		})

	gt.registerTool(server, policy.ToolMeta{Name: "fb_db_drop", Tier: 3, Scope: "database"},
		"DROP DATABASE stub on %s — Tier 3 disabled.",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			return "", fmt.Errorf("Tier 3 DROP DATABASE is disabled")
		})
}

func registerShutdown(server *mcp.Server, gt *gatedTools) {
	if gt.wf != nil {
		gt.wf.Register("shutdown_window", shutdownSteps(gt))
	}
	gt.registerTool(server, policy.ToolMeta{Name: "fb_shutdown_window", Tier: 2, Scope: "database", Preconditions: []policy.Precondition{
		{Name: "verified_backup_exists", Op: "true", Why: "verified backup required before exclusive window"},
		{Name: "backup_freshness", Op: "lt", Value: 24.0, Why: "backup < 24h"},
	}},
		"Exclusive shutdown window on %s: close pools → gfix -shut → callback → always gfix -online (Tier 2, out-of-band). args: {\"mode\":\"force|attach|tran\",\"kick\":true}.",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			if gt.wf == nil {
				return "", fmt.Errorf("workflow engine not initialised")
			}
			id := fmt.Sprintf("wf%d", time.Now().UnixNano())
			detail := map[string]string{"mode": strArg(args, "mode", "force")}
			if kick, _ := args["kick"].(bool); kick {
				detail["kick"] = "true"
			}
			return gt.wf.Run(ctx, id, "shutdown_window", dbID, true, detail, prog)
		})
}

func shutdownSteps(gt *gatedTools) []workflows.StepDef {
	return []workflows.StepDef{
		{Name: "close_pools", Do: func(ctx context.Context, wf *state.Workflow, prog func(float64, string)) error {
			gt.pools.CloseDB(wf.Database)
			return nil
		}},
		{Name: "optional_kick", Do: func(ctx context.Context, wf *state.Workflow, prog func(float64, string)) error {
			if wf.Detail["kick"] != "true" {
				return nil
			}
			pool, err := gt.pools.AdminPool(ctx, wf.Database)
			if err != nil {
				return err
			}
			var own int64
			_ = pool.QueryRowContext(ctx, "SELECT CURRENT_CONNECTION FROM RDB$DATABASE").Scan(&own)
			_, err = pool.ExecContext(ctx, "DELETE FROM MON$ATTACHMENTS WHERE MON$ATTACHMENT_ID <> ?", own)
			return err
		}},
		{Name: "gfix_shut", Do: func(ctx context.Context, wf *state.Workflow, prog func(float64, string)) error {
			inst, db, user, pass, err := gfixCreds(gt, wf.Database)
			if err != nil {
				return err
			}
			gt.pools.CloseDB(wf.Database)
			err = workflows.GfixShutdown(ctx, inst, db.Path, user, pass, strArg(map[string]any{"mode": wf.Detail["mode"]}, "mode", "force"), 30*time.Second)
			if err == nil {
				killpoint.Hit("wf.shut") // chaos harness (C7a): database shut, not yet brought online
			}
			return err
		}, Compensate: func(ctx context.Context, wf *state.Workflow) error {
			inst, db, user, pass, err := gfixCreds(gt, wf.Database)
			if err != nil {
				return err
			}
			return workflows.GfixOnline(ctx, inst, db.Path, user, pass)
		}},
		{Name: "gfix_online", AlwaysRun: true, Do: func(ctx context.Context, wf *state.Workflow, prog func(float64, string)) error {
			inst, db, user, pass, err := gfixCreds(gt, wf.Database)
			if err != nil {
				return err
			}
			return workflows.GfixOnline(ctx, inst, db.Path, user, pass)
		}},
		{Name: "verify", Do: func(ctx context.Context, wf *state.Workflow, prog func(float64, string)) error {
			return gt.pools.Health(ctx, wf.Database)
		}},
	}
}

func gfixCreds(gt *gatedTools, dbID string) (config.FBInstance, config.Database, string, string, error) {
	db, err := gt.cfg.DB(dbID)
	if err != nil {
		return config.FBInstance{}, db, "", "", err
	}
	inst, err := gt.cfg.Instance(db.Instance)
	if err != nil {
		return inst, db, "", "", err
	}
	pass, err := config.SecretFromEnv(db.AdminSecretEnv)
	return inst, db, db.AdminUser, pass, err
}

func registerConfigTools(server *mcp.Server, gt *gatedTools) {
	type instArg struct {
		Instance string `json:"instance"`
		Param    string `json:"param,omitempty"`
		Value    string `json:"value,omitempty"`
		Mode     string `json:"mode,omitempty"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_config_get", Description: "Tier 0: read a firebird.conf parameter (or all registry keys) for an instance"}, func(ctx context.Context, req *mcp.CallToolRequest, a instArg) (*mcp.CallToolResult, any, error) {
		inst, err := gt.cfg.Instance(a.Instance)
		if err != nil {
			return errText("error: " + err.Error())
		}
		f, err := configedit.ParseFile(configedit.ConfPath(inst.BinDir))
		if err != nil {
			return errText("error: " + err.Error())
		}
		if a.Param != "" {
			v, ok := f.Get(a.Param)
			if !ok {
				if p, err := configedit.ValidateSet(a.Param, "0"); err == nil {
					return text(fmt.Sprintf("%s: (not set; registry default %s, restart=%v)", a.Param, p.Default, p.Restart)), nil, nil
				}
				return text("unknown or unset: " + a.Param), nil, nil
			}
			return text(a.Param + " = " + v), nil, nil
		}
		var b strings.Builder
		for name := range configedit.Registry {
			if v, ok := f.Get(name); ok {
				fmt.Fprintf(&b, "%s = %s\n", name, v)
			}
		}
		if b.Len() == 0 {
			return text("(no registry keys present in file)"), nil, nil
		}
		return text(b.String()), nil, nil
	})

	gt.registerTool(server, policy.ToolMeta{Name: "fb_config_set", Tier: 2, Scope: "instance"},
		"Set a firebird.conf parameter on instance-scope %s (args: {\"param\":\"WireCrypt\",\"value\":\"Required\"}). Restart-required keys advise fb_service_status; never auto-restart.",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			// dbID is unused; instance comes from args
			instID, _ := args["instance"].(string)
			if instID == "" {
				// fall back: use the database's instance
				db, err := gt.cfg.DB(dbID)
				if err != nil {
					return "", fmt.Errorf("instance id required (args.instance)")
				}
				instID = db.Instance
			}
			name, _ := args["param"].(string)
			val, _ := args["value"].(string)
			p, err := configedit.ValidateSet(name, val)
			if err != nil {
				return "", err
			}
			inst, err := gt.cfg.Instance(instID)
			if err != nil {
				return "", err
			}
			path := configedit.ConfPath(inst.BinDir)
			f, err := configedit.ParseFile(path)
			if err != nil {
				return "", err
			}
			old, _ := f.Get(p.Name)
			g := f.Apply(p.Name, val)
			if err := configedit.AtomicWrite(path, g.String()); err != nil {
				return "", err
			}
			_ = configedit.AppendJournal(gt.live().State.Dir, instID, path, p.Name, old, val)
			msg := fmt.Sprintf("%s: %q → %q (previous copy at %s.prev)", p.Name, old, val, path)
			if p.Restart {
				msg += " — restart required (fb_service_status; start/stop waits on host posture)"
			}
			return msg, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "fb_config_diff", Description: "Tier 0: diff firebird.conf vs registry defaults; flag restart-required"}, func(ctx context.Context, req *mcp.CallToolRequest, a instArg) (*mcp.CallToolResult, any, error) {
		inst, err := gt.cfg.Instance(a.Instance)
		if err != nil {
			return errText("error: " + err.Error())
		}
		f, err := configedit.ParseFile(configedit.ConfPath(inst.BinDir))
		if err != nil {
			return errText("error: " + err.Error())
		}
		var b strings.Builder
		for name, p := range configedit.Registry {
			v, ok := f.Get(name)
			if !ok {
				continue
			}
			fmt.Fprintf(&b, "%s = %s", name, v)
			if p.Default != "" && !strings.EqualFold(v, p.Default) {
				fmt.Fprintf(&b, " (default %s)", p.Default)
			}
			if p.Restart {
				b.WriteString(" [restart-required]")
			}
			if p.Security {
				b.WriteString(" [security]")
			}
			b.WriteByte('\n')
		}
		if b.Len() == 0 {
			return text("(no registry keys present)"), nil, nil
		}
		return text(b.String()), nil, nil
	})
}

func registerWindowOpen(server *mcp.Server, gt *gatedTools) {
	gt.registerTool(server, policy.ToolMeta{Name: "fb_window_open", Tier: 1, Scope: "database"},
		"Open a maintenance window for %s (args: {\"hours\": N, default 2}). Required for Tier-2 tools.",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			hours := 2.0
			if n, ok := toInt(args["hours"]); ok && n > 0 && n <= 24 {
				hours = float64(n)
			}
			w := state.Window{Database: dbID, From: time.Now().UTC(), To: time.Now().UTC().Add(time.Duration(hours * float64(time.Hour)))}
			if err := gt.st.AddWindow(w); err != nil {
				return "", err
			}
			return fmt.Sprintf("maintenance window open for %s until %s", dbID, w.To.Format(time.RFC3339)), nil
		})
}

func runWrite(ctx context.Context, gt *gatedTools, dbID, sql string, prog func(float64, string)) (string, error) {
	prep, err := execpkg.Prepare(sql)
	if err != nil {
		return "", err
	}
	return gt.execSvc.Exec(ctx, dbID, prep, prog)
}

func qident(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func escapeLit(s string) string { return strings.ReplaceAll(s, `'`, `''`) }

func scrubPass(msg, pass string) string {
	if pass == "" {
		return msg
	}
	return strings.ReplaceAll(msg, pass, "******")
}

func strArg(args map[string]any, k, def string) string {
	if v, _ := args[k].(string); v != "" {
		return v
	}
	return def
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func isIdentifier(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for _, r := range s {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '$' || r == '_') {
			return false
		}
	}
	return true
}

func toInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return int64(n), true
	case float64:
		return int64(n), true
	}
	return 0, false
}
