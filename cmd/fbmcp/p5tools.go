// Phase-5 tools: scheduler grants, nightly_verify chain, retention (P5.3).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/adminexec"
	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/gate"
	"github.com/aleks/fbmcp/internal/housekeep"
	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/notify"
	"github.com/aleks/fbmcp/internal/policy"
	"github.com/aleks/fbmcp/internal/posture"
	"github.com/aleks/fbmcp/internal/retention"
	"github.com/aleks/fbmcp/internal/schedule"
	"github.com/aleks/fbmcp/internal/state"
	"github.com/aleks/fbmcp/internal/workflows"
)

func registerP5Tools(server *mcp.Server, gt *gatedTools) {
	gt.wf.Register("nightly_verify", nightlyVerifySteps(gt))
	registerScheduleList(server, gt)
	registerScheduleCreate(server, gt)
	registerScheduleDelete(server, gt)
	registerRetentionRun(server, gt)
	registerServiceControl(server, gt)
}

func nightlyVerifySteps(gt *gatedTools) []workflows.StepDef {
	return []workflows.StepDef{
		{Name: "backup", Do: func(ctx context.Context, wf *state.Workflow, prog func(float64, string)) error {
			exec := gt.execs["fb_backup_start"]
			if exec == nil {
				return fmt.Errorf("fb_backup_start not registered")
			}
			_, err := exec(ctx, wf.Database, nil, prog)
			return err
		}},
		{Name: "restore_test", Do: func(ctx context.Context, wf *state.Workflow, prog func(float64, string)) error {
			exec := gt.execs["fb_restore_test"]
			if exec == nil {
				return fmt.Errorf("fb_restore_test not registered")
			}
			_, err := exec(ctx, wf.Database, nil, prog)
			return err
		}},
	}
}

func registerScheduleList(server *mcp.Server, gt *gatedTools) {
	type dbArg struct {
		Db string `json:"db"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_schedule_list", Description: "Tier 0: list durable schedule grants for a database"}, func(ctx context.Context, req *mcp.CallToolRequest, a dbArg) (*mcp.CallToolResult, any, error) {
		if _, err := gt.cfg.DB(a.Db); err != nil {
			return text("error: " + err.Error()), nil, nil
		}
		var b strings.Builder
		n := 0
		for _, s := range gt.st.Schedules() {
			if s.Database != a.Db {
				continue
			}
			n++
			fmt.Fprintf(&b, "- %s target=%s cron=%q tz=%s enabled=%v tier=%d confirmer=%s channel=%s last_fire=%s skip=%s\n",
				s.ID, s.Target, s.Cron, s.Timezone, s.Enabled, s.MaxTier, s.Confirmer, s.Channel,
				fmtTime(s.LastFiredAt), s.LastSkipReason)
		}
		if n == 0 {
			return text("(no schedules)"), nil, nil
		}
		gt.aud.Log(audit.Entry{Identity: "local", Database: a.Db, Tool: "fb_schedule_list", Tier: 0, Decision: "allow"})
		return text(b.String()), nil, nil
	})
}

func registerScheduleCreate(server *mcp.Server, gt *gatedTools) {
	type createArg struct {
		Db             string         `json:"db"`
		Target         string         `json:"target" jsonschema:"tool name or nightly_verify"`
		Cron           string         `json:"cron" jsonschema:"5-field cron"`
		Timezone       string         `json:"timezone" jsonschema:"IANA timezone, required"`
		WindowRequired bool           `json:"window_required,omitempty"`
		MissedRun      string         `json:"missed_run,omitempty" jsonschema:"skip or catchup-once"`
		Args           map[string]any `json:"args,omitempty"`
		Mode           string         `json:"mode,omitempty"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_schedule_create", Description: "Gated: persist a durable schedule grant (tier = max of target)"}, func(ctx context.Context, req *mcp.CallToolRequest, a createArg) (*mcp.CallToolResult, any, error) {
		id := identity.Caller(ctx)
		if _, err := gt.cfg.DB(a.Db); err != nil {
			return text("DENIED: " + err.Error()), nil, nil
		}
		kind, tier, err := schedule.ValidateCreate(a.Target, a.Cron, a.Timezone, func(name string) (string, int, error) {
			return scheduleTarget(name)
		})
		if err != nil {
			gt.aud.Log(audit.Entry{Identity: id.Name, Database: a.Db, Tool: "fb_schedule_create", Tier: -1, Decision: "denied", Detail: map[string]interface{}{"reason": err.Error()}})
			return text("DENIED: " + err.Error()), nil, nil
		}
		meta := policy.ToolMeta{Name: "fb_schedule_create", Tier: tier, Scope: "database"}
		d := gt.eng.EvaluateMeta(id, a.Db, meta)
		if d.Outcome == "deny" {
			return text("DENIED: " + d.Reason), nil, nil
		}
		impact := fmt.Sprintf("Create schedule grant on %s: target=%s cron=%q tz=%s (durable; fire path will NOT re-confirm). Informational preview — not a safety guarantee.",
			a.Db, a.Target, a.Cron, a.Timezone)
		if strings.EqualFold(a.Mode, "preview") {
			return text(fmt.Sprintf("%s\n\nmode=preview (informational — confirmation still required to execute)\nTier: %d | kind=%s\nAccepted confirmation channels: %s\n",
				impact, tier, kind, strings.Join(gate.AllowedChannels(tier), ", "))), nil, nil
		}
		args := map[string]any{
			"target": a.Target, "cron": a.Cron, "timezone": a.Timezone,
			"window_required": a.WindowRequired, "missed_run": a.MissedRun,
			"kind": kind, "tier": float64(tier), "args": a.Args,
		}
		raw, _ := json.Marshal(args)
		argHash := hashOf(a.Db + string(raw))
		p, err := gt.g.Request(id, a.Db, meta, impact, argHash, nil)
		if err != nil {
			return text("gate error: " + err.Error()), nil, nil
		}
		gt.mu.Lock()
		gt.args[p.ID] = args
		gt.mu.Unlock()
		var b strings.Builder
		b.WriteString(gate.ImpactStatement(p))
		if tier <= 1 {
			fmt.Fprintf(&b, "In-band token (Tier 1 only): %s\n", gate.IssueToken(p.ID, argHash))
		}
		return text(b.String()), nil, nil
	})
	gt.execs["fb_schedule_create"] = func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
		target, _ := args["target"].(string)
		cron, _ := args["cron"].(string)
		tz, _ := args["timezone"].(string)
		kind, _ := args["kind"].(string)
		missed, _ := args["missed_run"].(string)
		if missed == "" {
			missed = "skip"
		}
		win, _ := args["window_required"].(bool)
		tier := 1
		if n, ok := toInt(args["tier"]); ok {
			tier = int(n)
		}
		inner, _ := args["args"].(map[string]any)
		argsJSON := schedule.CanonicalJSON(inner)
		id := fmt.Sprintf("sch%d", time.Now().UnixNano())
		ch, _ := args["_grant_channel"].(string)
		conf, _ := args["_grant_identity"].(string)
		reqID, _ := args["_grant_request_id"].(string)
		sc := state.Schedule{
			ID: id, Database: dbID, Target: target, Kind: kind, ArgsJSON: argsJSON,
			ArgHash: schedule.HashArgs(argsJSON), MaxTier: tier, Confirmer: conf,
			Channel: ch, CreatingRequest: reqID, Cron: cron, Timezone: tz,
			WindowRequired: win, MissedRun: missed, Overlap: "skip", Enabled: true,
			CreatedAt: time.Now().UTC(),
		}
		if err := gt.st.PutSchedule(sc); err != nil {
			return "", err
		}
		// Detail carries the full grant so fbmcpctl repair --from-audit can
		// rebuild enriched-era grants from the chain alone (phase8_plan D1.2;
		// entries before this change are not reconstructable).
		gt.aud.Log(audit.Entry{Identity: conf, Database: dbID, Tool: "fb_schedule_create", Tier: tier, Decision: "approved", Channel: ch,
			Detail: map[string]interface{}{"schedule_id": id, "target": target, "creating_request": reqID,
				"cron": cron, "timezone": tz, "kind": kind, "missed_run": missed,
				"window_required": win, "args": argsJSON, "arg_hash": sc.ArgHash}})
		return fmt.Sprintf("schedule %s stored (target=%s cron=%q tz=%s grant=%s/%s)", id, target, cron, tz, conf, ch), nil
	}
}

func registerScheduleDelete(server *mcp.Server, gt *gatedTools) {
	gt.registerTool(server, policy.ToolMeta{Name: "fb_schedule_delete", Tier: 1, Scope: "database"},
		"Delete a schedule grant on %s (args: {\"id\":\"sch…\"}).",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			id, _ := args["id"].(string)
			if id == "" {
				return "", fmt.Errorf("id is required")
			}
			sc, ok := gt.st.Schedule(id)
			if !ok || sc.Database != dbID {
				return "", fmt.Errorf("unknown schedule %q", id)
			}
			if err := gt.st.RemoveSchedule(id); err != nil {
				return "", err
			}
			return "deleted " + id, nil
		})
}

func registerRetentionRun(server *mcp.Server, gt *gatedTools) {
	gt.registerTool(server, policy.ToolMeta{Name: "fb_retention_run", Tier: 1, Scope: "database"},
		"Retention housekeeping on %s (ADR-016). Default dry_run=true; only catalog-verified artifacts past keep_days. Uncataloged files are never touched.",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			keep := 0
			if n, ok := toInt(args["keep_days"]); ok {
				keep = int(n)
			} else if gt.live().Retention.KeepDays > 0 {
				keep = gt.live().Retention.KeepDays
			}
			dry := true
			if v, ok := args["dry_run"].(bool); ok {
				dry = v
			}
			plan := retention.Plan(gt.st, keep, time.Now().UTC())
			filtered := plan[:0]
			for _, a := range plan {
				for _, e := range gt.st.Catalog() {
					if e.ID == a.CatalogID && e.Database == dbID {
						filtered = append(filtered, a)
					}
				}
			}
			msg, err := retention.Execute(gt.st, filtered, dry)
			if err != nil {
				return "", err
			}
			if gt.bus != nil {
				_ = gt.bus.Emit(notify.Event{Type: "retention.report", Database: dbID, Tool: "fb_retention_run", Message: msg})
			}
			// log + work-dir rotation (best-effort)
			_ = housekeep.Rotate(filepath.Join(gt.live().State.Dir, "server.log"), 8<<20, 3)
			if db, err := gt.cfg.DB(dbID); err == nil && db.WorkDir != "" {
				_, _ = housekeep.CleanOrphans(db.WorkDir, 48*time.Hour, []string{".fdb", ".pre-restore"})
				_ = housekeep.Rotate(filepath.Join(db.WorkDir, "trace.out"), 8<<20, 2)
			}
			_ = os.MkdirAll(gt.live().State.Dir, 0o755)
			return msg, nil
		})
}

func scheduleTarget(name string) (kind string, tier int, err error) {
	switch name {
	case "nightly_verify":
		return "workflow", 1, nil
	case "fb_db_drop":
		return "tool", 3, nil
	case "fb_restore_replace":
		return "tool", 2, nil
	case "fb_backup_start", "fb_restore_test", "fb_index_rebuild", "fb_validate", "fb_sweep", "fb_retention_run":
		return "tool", 1, nil
	default:
		return "", 0, fmt.Errorf("unknown or unschedulable target %q", name)
	}
}

func (gt *gatedTools) fireSchedule(ctx context.Context, s state.Schedule) (string, error) {
	var args map[string]any
	if s.ArgsJSON != "" && s.ArgsJSON != "null" {
		_ = json.Unmarshal([]byte(s.ArgsJSON), &args)
	}
	gt.aud.Log(audit.Entry{Identity: "schedule:" + s.ID, Database: s.Database, Tool: s.Target, Tier: s.MaxTier, Decision: "allow", Channel: s.Channel,
		Detail: map[string]interface{}{"schedule_id": s.ID, "confirmer": s.Confirmer, "creating_request": s.CreatingRequest, "preauth": true}})
	if s.Kind == "workflow" {
		return gt.runner.Submit(s.Target, s.Database, "schedule:"+s.ID, s.CreatingRequest, func(ctx context.Context, prog func(float64, string)) (string, error) {
			id := fmt.Sprintf("wf%d", time.Now().UnixNano())
			return gt.wf.Run(ctx, id, s.Target, s.Database, false, map[string]string{"schedule_id": s.ID}, prog)
		})
	}
	exec, ok := gt.execs[s.Target]
	if !ok {
		return "", fmt.Errorf("no executor for %q", s.Target)
	}
	return gt.runner.Submit(s.Target, s.Database, "schedule:"+s.ID, s.CreatingRequest, func(ctx context.Context, prog func(float64, string)) (string, error) {
		return exec(ctx, s.Database, args, prog)
	})
}

func registerServiceControl(server *mcp.Server, gt *gatedTools) {
	for _, spec := range []struct {
		name, action, impact string
	}{
		{"fb_service_start", "start", "Start the Firebird OS service for instance of %s (Tier 2; refuses without ADR-017 posture)."},
		{"fb_service_stop", "stop", "Stop the Firebird OS service for instance of %s (Tier 2; refuses without ADR-017 posture)."},
		{"fb_service_restart", "restart", "Restart the Firebird OS service for instance of %s (Tier 2; refuses without ADR-017 posture)."},
	} {
		action := spec.action
		gt.registerTool(server, policy.ToolMeta{Name: spec.name, Tier: 2, Scope: "instance"}, spec.impact,
			func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
				return gt.runServiceControl(ctx, dbID, args, action)
			})
	}
}

func (gt *gatedTools) runServiceControl(ctx context.Context, dbID string, args map[string]any, action string) (string, error) {
	if !posture.Verified(gt.live().State.Dir) {
		return "", fmt.Errorf("%s", posture.RefuseMessage())
	}
	instID, _ := args["instance"].(string)
	if instID == "" {
		db, err := gt.cfg.DB(dbID)
		if err != nil {
			return "", fmt.Errorf("instance id required (args.instance)")
		}
		instID = db.Instance
	}
	in, err := gt.cfg.Instance(instID)
	if err != nil {
		return "", err
	}
	svc := in.Service
	if svc == "" {
		svc = "FirebirdServerDefaultInstance"
	}
	return controlService(ctx, svc, action)
}

func controlService(ctx context.Context, svc, action string) (string, error) {
	if runtime.GOOS == "windows" {
		bin := `C:\Windows\System32\sc.exe`
		switch action {
		case "start":
			res := adminexec.Run(ctx, bin, []string{"start", svc}, 60*time.Second, 64<<10, nil)
			if res.Err != nil {
				return "", fmt.Errorf("sc start: %s %v", res.Output, res.Err)
			}
			return strings.TrimSpace(res.Output), nil
		case "stop":
			res := adminexec.Run(ctx, bin, []string{"stop", svc}, 60*time.Second, 64<<10, nil)
			if res.Err != nil {
				return "", fmt.Errorf("sc stop: %s %v", res.Output, res.Err)
			}
			return strings.TrimSpace(res.Output), nil
		case "restart":
			_ = adminexec.Run(ctx, bin, []string{"stop", svc}, 60*time.Second, 64<<10, nil)
			res := adminexec.Run(ctx, bin, []string{"start", svc}, 60*time.Second, 64<<10, nil)
			if res.Err != nil {
				return "", fmt.Errorf("sc restart: %s %v", res.Output, res.Err)
			}
			return strings.TrimSpace(res.Output), nil
		}
	}
	bin := "/bin/systemctl"
	res := adminexec.Run(ctx, bin, []string{action, svc}, 60*time.Second, 64<<10, nil)
	if res.Err != nil {
		return "", fmt.Errorf("systemctl %s: %s %v", action, res.Output, res.Err)
	}
	return strings.TrimSpace(res.Output), nil
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}
