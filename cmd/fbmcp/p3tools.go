// Phase-3 admin tools (P3.1–P3.8) on the generic gate→job pattern.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/adminexec"
	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/backupsvc"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/dbpool"
	execpkg "github.com/aleks/fbmcp/internal/executor"
	"github.com/aleks/fbmcp/internal/facts"
	"github.com/aleks/fbmcp/internal/gate"
	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/jobs"
	"github.com/aleks/fbmcp/internal/notify"
	"github.com/aleks/fbmcp/internal/policy"
	"github.com/aleks/fbmcp/internal/reload"
	"github.com/aleks/fbmcp/internal/state"
	"github.com/aleks/fbmcp/internal/workflows"
)

// executor runs the confirmed body of a gated tool as a job.
type executor func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error)

type gatedTools struct {
	cfg      *config.Handle
	pools    *dbpool.Manager
	eng      *policy.Engine
	g        *gate.Gate
	runner   *jobs.Runner
	aud      *audit.Logger
	st       *state.Store
	execSvc  *execpkg.Service
	wf       *workflows.Engine
	execs    map[string]executor
	traces   map[string]*backupsvc.LiveTrace
	bus      *notify.Bus
	reloader *reload.Controller
	httpLn   *httpListener
	facts    *facts.EngineFacts

	mu   sync.Mutex
	args map[string]map[string]any // requestID -> tool args (in-memory; single instance per D8)
}

func (gt *gatedTools) live() *config.Config { return gt.cfg.Current() }

func (gt *gatedTools) client(dbID string) (*backupsvc.Client, config.Database, error) {
	db, err := gt.cfg.DB(dbID)
	if err != nil {
		return nil, db, err
	}
	inst, err := gt.cfg.Instance(db.Instance)
	if err != nil {
		return nil, db, err
	}
	pass, err := config.SecretFromEnv(db.AdminSecretEnv)
	if err != nil {
		return nil, db, err
	}
	return backupsvc.NewClient(inst, db.AdminUser, pass), db, nil
}

// registerTool wires one gated tool: policy → pending action → (confirm) → job.
func (gt *gatedTools) registerTool(server *mcp.Server, meta policy.ToolMeta, impactFmt string, exec executor) {
	gt.registerToolEx(server, meta, impactFmt, exec, nil)
}

func (gt *gatedTools) registerToolEx(server *mcp.Server, meta policy.ToolMeta, impactFmt string, exec executor, preview func(context.Context, string, map[string]any) string) {
	gt.execs[meta.Name] = exec
	type dbArg struct {
		Db   string         `json:"db"`
		Mode string         `json:"mode,omitempty" jsonschema:"preview or execute (default execute)"`
		Args map[string]any `json:"args,omitempty"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: meta.Name, Description: fmt.Sprintf("Tier %d (gated): %s", meta.Tier, meta.Name)}, func(ctx context.Context, req *mcp.CallToolRequest, a dbArg) (*mcp.CallToolResult, any, error) {
		return text(gt.requestGated(ctx, meta, impactFmt, a.Db, a.Args, a.Mode, preview)), nil, nil
	})
}

// requestGated is the real tool path (C2/C22): policy + audit, then gate.Request.
// The executor is never called from here.
func (gt *gatedTools) requestGated(ctx context.Context, meta policy.ToolMeta, impactFmt, dbID string, args map[string]any, mode string, preview func(context.Context, string, map[string]any) string) string {
	if _, err := gt.cfg.DB(dbID); err != nil && meta.Scope == "database" {
		return "DENIED: " + err.Error()
	}
	id := identity.Caller(ctx)
	d := gt.eng.Evaluate(id, dbID, meta.Name)
	if d.Outcome == "deny" {
		gt.aud.Log(audit.Entry{Identity: id.Name, Database: dbID, Tool: meta.Name, Tier: meta.Tier, Decision: "denied", Detail: map[string]interface{}{"reason": d.Reason}})
		return "DENIED: " + d.Reason
	}
	impact := fmt.Sprintf(impactFmt, dbID)
	if preview != nil {
		if extra := preview(ctx, dbID, args); extra != "" {
			impact += "\n" + extra
		}
	}
	if strings.EqualFold(mode, "preview") {
		var b strings.Builder
		b.WriteString(impact)
		fmt.Fprintf(&b, "\n\nmode=preview (informational — confirmation still required to execute)\nTier: %d | Database: %s\nAccepted confirmation channels: %s\n",
			meta.Tier, dbID, strings.Join(gate.AllowedChannels(meta.Tier), ", "))
		if len(d.FailedPreconditions) > 0 {
			fmt.Fprintf(&b, "preconditions currently failing: %s\n", strings.Join(d.FailedPreconditions, "; "))
		}
		return b.String()
	}
	if len(d.FailedPreconditions) > 0 {
		return "DENIED: " + d.Reason
	}
	argsJSON, _ := json.Marshal(args)
	argHash := hashOf(dbID + string(argsJSON))
	p, err := gt.g.Request(id, dbID, meta, impact, argHash, d.FailedPreconditions)
	if err != nil {
		return "gate error: " + err.Error()
	}
	gt.mu.Lock()
	gt.args[p.ID] = args
	gt.mu.Unlock()
	var b strings.Builder
	b.WriteString(gate.ImpactStatement(p))
	if meta.Tier <= 1 {
		fmt.Fprintf(&b, "In-band token (Tier 1 only): %s\n", gate.IssueToken(p.ID, argHash))
	}
	return b.String()
}

// dispatch is called by fb_confirm after a successful confirmation.
func (gt *gatedTools) dispatch(p state.PendingAction) (string, error) {
	exec, ok := gt.execs[p.Tool]
	if !ok {
		return "", fmt.Errorf("no executor registered for %q", p.Tool)
	}
	gt.mu.Lock()
	args := gt.args[p.ID]
	delete(gt.args, p.ID)
	gt.mu.Unlock()
	if args == nil {
		args = map[string]any{}
	}
	args["_grant_identity"] = p.Identity
	args["_grant_channel"] = p.ConfirmedChannel
	args["_grant_request_id"] = p.ID
	return gt.runner.Submit(p.Tool, p.Database, p.Identity, p.ID, func(ctx context.Context, prog func(float64, string)) (string, error) {
		return exec(ctx, p.Database, args, prog)
	})
}

func registerP3Tools(server *mcp.Server, gt *gatedTools) {
	// P3.1 — fb_backup_start (Tier 1): full backup into the DB's backup dir,
	// registered in the catalog (unverified until a test-restore).
	gt.registerTool(server, policy.ToolMeta{Name: "fb_backup_start", Tier: 1, Scope: "database"},
		"Full gbak backup of %s to its configured backup dir (async job; args: {\"parallel_workers\": N} for HQBird/FB5 multi-thread backup, 1-64).",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			c, db, err := gt.client(dbID)
			if err != nil {
				return "", err
			}
			parallel, _ := toInt(args["parallel_workers"])
			backupDir := db.BackupDir
			if backupDir == "" {
				backupDir = filepath.Dir(db.Path)
			}
			if err := os.MkdirAll(backupDir, 0o755); err != nil {
				return "", err
			}
			fbk := filepath.Join(backupDir, fmt.Sprintf("%s_%s.fbk", dbID, time.Now().Format("20060102_150405")))
			if parallel > 0 {
				prog(0.1, fmt.Sprintf("backup started (parallel_workers=%d)", parallel))
			} else {
				prog(0.1, "backup started")
			}
			if err := c.Backup(db.Path, fbk, int32(parallel), func(m string) { prog(0.5, m) }); err != nil {
				return "", fmt.Errorf("backup failed: %w", err)
			}
			if err := backupsvc.NewCatalog(gt.st).Register(dbID, fbk, false); err != nil {
				return "", err
			}
			return fmt.Sprintf("backup written: %s (unverified — run fb_restore_test to verify)", fbk), nil
		})

	// P3.1 leftover — nbackup levels 0–2.
	gt.registerTool(server, policy.ToolMeta{Name: "fb_backup_nbackup", Tier: 1, Scope: "database"},
		"nbackup of %s (args: {\"level\": 0|1|2}). Level >0 requires a prior nbackup of level-1 in the catalog.",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			level, ok := toInt(args["level"])
			if !ok {
				level = 0
			}
			if level < 0 || level > 2 {
				return "", fmt.Errorf("level must be 0, 1, or 2")
			}
			if level > 0 {
				if _, ok := gt.st.LatestNBackup(dbID, int(level-1)); !ok {
					return "", fmt.Errorf("no nbackup level %d in catalog — take a level %d first", level-1, level-1)
				}
			}
			c, db, err := gt.client(dbID)
			if err != nil {
				return "", err
			}
			backupDir := db.BackupDir
			if backupDir == "" {
				backupDir = filepath.Dir(db.Path)
			}
			if err := os.MkdirAll(backupDir, 0o755); err != nil {
				return "", err
			}
			nbk := filepath.Join(backupDir, fmt.Sprintf("%s_%s_l%d.nbk", dbID, time.Now().Format("20060102_150405"), level))
			prog(0.1, fmt.Sprintf("nbackup level %d", level))
			if err := c.NBackup(db.Path, nbk, int(level), func(m string) { prog(0.5, m) }); err != nil {
				return "", fmt.Errorf("nbackup failed: %w", err)
			}
			if err := backupsvc.NewCatalog(gt.st).RegisterKind(dbID, nbk, false, "nbackup", int(level)); err != nil {
				return "", err
			}
			return fmt.Sprintf("nbackup level %d written: %s (unverified)", level, nbk), nil
		})

	// P3.2 — fb_restore_test (Tier 1): restore the newest artifact into the
	// work dir, validate, mark verified (verification = successful
	// test-restore per the P0.2 finding that gbak has no standalone verify).
	gt.registerTool(server, policy.ToolMeta{Name: "fb_restore_test", Tier: 1, Scope: "database"},
		"Test-restore of the newest backup of %s into the work dir (never touches the source DB; args: {\"parallel_workers\": N} for HQBird/FB5 multi-thread index creation during restore, 1-64).",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			c, db, err := gt.client(dbID)
			if err != nil {
				return "", err
			}
			parallel, _ := toInt(args["parallel_workers"])
			backupDir := db.BackupDir
			if backupDir == "" {
				backupDir = filepath.Dir(db.Path)
			}
			matches, _ := filepath.Glob(filepath.Join(backupDir, dbID+"_*.fbk"))
			var fbk string
			var newest time.Time
			found := false
			for _, m := range matches {
				if fi, err := os.Stat(m); err == nil && (!found || fi.ModTime().After(newest)) {
					newest, fbk, found = fi.ModTime(), m, true
				}
			}
			if !found {
				return "", fmt.Errorf("no backup artifacts in %s — run fb_backup_start first", backupDir)
			}
			workDir := db.WorkDir
			if workDir == "" {
				workDir = backupDir
			}
			restored := filepath.Join(workDir, "verify_"+dbID+".fdb")
			os.Remove(restored)
			if parallel > 0 {
				prog(0.2, fmt.Sprintf("restoring %s (parallel_workers=%d)", fbk, parallel))
			} else {
				prog(0.2, "restoring "+fbk)
			}
			if err := c.Restore(fbk, restored, false, int32(parallel), func(m string) { prog(0.6, m) }); err != nil {
				return "", fmt.Errorf("restore failed: %w", err)
			}
			prog(0.8, "validating restored copy")
			if err := c.Validate(restored, 0); err != nil {
				return "", fmt.Errorf("validation failed: %w", err)
			}
			os.Remove(restored)
			if err := backupsvc.NewCatalog(gt.st).Register(dbID, fbk, true); err != nil {
				return "", err
			}
			return fmt.Sprintf("backup %s verified by test-restore; catalog updated", fbk), nil
		})

	// P3.4 — fb_validate (Tier 1).
	gt.registerTool(server, policy.ToolMeta{Name: "fb_validate", Tier: 1, Scope: "database"},
		"Online validation of %s (gfix -validate equivalent); findings only, no repair.",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			c, db, err := gt.client(dbID)
			if err != nil {
				return "", err
			}
			if err := c.Validate(db.Path, 0); err != nil {
				return "", fmt.Errorf("validation reported problems: %w", err)
			}
			return "validation clean (no orphan pages / checksum errors reported)", nil
		})

	// P3.5 — fb_sweep (Tier 1).
	gt.registerTool(server, policy.ToolMeta{Name: "fb_sweep", Tier: 1, Scope: "database"},
		"Manual sweep of %s (duration depends on the OIT gap).",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			c, db, err := gt.client(dbID)
			if err != nil {
				return "", err
			}
			if err := c.Sweep(db.Path); err != nil {
				return "", err
			}
			return "sweep completed", nil
		})

	// P3.5 family — fb_set_forcewrite (Tier 1); args: {"on": true|false}.
	gt.registerTool(server, policy.ToolMeta{Name: "fb_set_forcewrite", Tier: 1, Scope: "database"},
		"Toggle Forced Writes on %s (args: {\"on\": true|false}).",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			on, _ := args["on"].(bool)
			c, db, err := gt.client(dbID)
			if err != nil {
				return "", err
			}
			if err := c.SetForceWrite(db.Path, on); err != nil {
				return "", err
			}
			return fmt.Sprintf("forced writes %s", map[bool]string{true: "ON", false: "OFF"}[on]), nil
		})

	// P3.5 family — fb_set_readonly (Tier 1); args: {"readonly": bool}.
	gt.registerTool(server, policy.ToolMeta{Name: "fb_set_readonly", Tier: 1, Scope: "database"},
		"Set %s access mode (args: {\"readonly\": true|false}); RO requires no other attachments.",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			ro, _ := args["readonly"].(bool)
			c, db, err := gt.client(dbID)
			if err != nil {
				return "", err
			}
			if err := c.SetReadOnly(db.Path, ro); err != nil {
				return "", err
			}
			return fmt.Sprintf("access mode %s", map[bool]string{true: "READ ONLY", false: "READ WRITE"}[ro]), nil
		})

	// P3.5 leftover — fb_set_page_buffers (Tier 1); args: {"buffers": N}.
	gt.registerTool(server, policy.ToolMeta{Name: "fb_set_page_buffers", Tier: 1, Scope: "database"},
		"Set page buffers on %s (args: {\"buffers\": N}).",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			n, ok := toInt(args["buffers"])
			if !ok || n <= 0 || n > 100000 {
				return "", fmt.Errorf("buffers (positive int) required")
			}
			c, db, err := gt.client(dbID)
			if err != nil {
				return "", err
			}
			inst, err := gt.cfg.Instance(db.Instance)
			if err != nil {
				return "", err
			}
			pass, err := config.SecretFromEnv(db.AdminSecretEnv)
			if err != nil {
				return "", err
			}
			if err := workflows.GfixBuffers(ctx, inst, db.Path, db.AdminUser, pass, int(n)); err != nil {
				return "", err
			}
			_ = c
			return fmt.Sprintf("page buffers set to %d", n), nil
		})

	registerTraceTools(server, gt)

	// P3.7 — fb_service_status (Tier 0 read-only probe).
	type svcArg struct {
		Instance string `json:"instance"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_service_status", Description: "Tier 0: Firebird service status (read-only)"}, func(ctx context.Context, req *mcp.CallToolRequest, a svcArg) (*mcp.CallToolResult, any, error) {
		in, err := gt.cfg.Instance(a.Instance)
		if err != nil {
			return text("error: " + err.Error()), nil, nil
		}
		svc := in.Service
		if svc == "" {
			svc = "FirebirdServerDefaultInstance"
		}
		return text(probeService(ctx, svc)), nil, nil
	})
}

// probeService reports OS service state without changing it. Service control
// (start/stop/restart) stays behind the Tier-2 gate + §4.8 posture (P3.7).
func probeService(ctx context.Context, svc string) string {
	if runtime.GOOS == "windows" {
		res := adminexec.Run(ctx, `C:\Windows\System32\sc.exe`, []string{"query", svc}, 10*time.Second, 64<<10, nil)
		out := res.Output
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(strings.ToUpper(line), "STATE") {
				return strings.TrimSpace(line)
			}
		}
		return "status unavailable: " + firstErrStr(res.Err)
	}
	res := adminexec.Run(ctx, "/bin/systemctl", []string{"is-active", svc}, 10*time.Second, 64<<10, nil)
	return strings.TrimSpace(res.Output)
}

func firstErrStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// windowOpen reports whether db is inside a maintenance window now.
func (gt *gatedTools) windowOpen(db string) bool { return gt.st.InWindow(db, time.Now()) }

// startApprovalWatcher polls the OOB approvals/denials dirs and resolves
// matching pending actions through the out-of-band channel (the LLM cannot
// reach either).
func (gt *gatedTools) startApprovalWatcher(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			gt.consumeApprovalMarkers()
			gt.consumeDenialMarkers()
		}
	}()
}

// consumeApprovalMarkers is the OOB marker path (C19). Unknown ids are dropped.
// A consumed pending action cannot dispatch twice.
func (gt *gatedTools) consumeApprovalMarkers() int {
	dir := filepath.Join(gt.live().State.Dir, "approvals")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		id := e.Name()
		var as string
		for _, p := range gt.st.Pending() {
			if p.ID == id {
				as = p.Identity
			}
		}
		if as == "" {
			os.Remove(filepath.Join(dir, id)) // unknown — drop marker
			continue
		}
		p, err := gt.g.Confirm(id, as, gate.ChannelOutOfBand, "")
		if err != nil {
			continue // expired/unknown — marker retried next tick
		}
		gt.aud.Log(audit.Entry{Identity: "operator", Database: p.Database, Tool: p.Tool, Tier: p.Tier, Decision: "approved", Channel: gate.ChannelOutOfBand,
			Detail: map[string]interface{}{"request_id": id, "on_behalf_of": as}})
		os.Remove(filepath.Join(dir, id))
		if _, err := gt.dispatch(p); err != nil {
			gt.aud.Log(audit.Entry{Identity: "operator", Database: p.Database, Tool: p.Tool, Tier: p.Tier, Decision: "error",
				Detail: map[string]interface{}{"error": err.Error()}})
		}
		n++
	}
	return n
}

// consumeDenialMarkers is the OOB reject path, symmetric with
// consumeApprovalMarkers: an operator's Deny takes effect immediately
// instead of waiting out the 15-minute TTL (gate.SweepExpired). Unlike
// approval this never dispatches — it just removes the pending action.
func (gt *gatedTools) consumeDenialMarkers() int {
	dir := filepath.Join(gt.live().State.Dir, "denials")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		id := e.Name()
		p, ok, _ := gt.st.TakePending(id)
		os.Remove(filepath.Join(dir, id))
		if !ok {
			continue // unknown/already-resolved — marker dropped
		}
		gt.aud.Log(audit.Entry{Identity: "operator", Database: p.Database, Tool: p.Tool, Tier: p.Tier, Decision: "denied", Channel: gate.ChannelOutOfBand,
			Detail: map[string]interface{}{"request_id": id, "reason": "operator denied"}})
		if gt.bus != nil {
			_ = gt.bus.Emit(notify.Event{Type: "gate.denied", Database: p.Database, Tool: p.Tool, Message: "pending action denied by operator",
				Detail: map[string]string{"request_id": id}})
		}
		n++
	}
	return n
}

// registerRestore adds the Tier-2 guarded restore tool (K5 workflow).
func (gt *gatedTools) registerRestore(server *mcp.Server) {
	if gt.wf != nil {
		gt.wf.Register("restore_replace", restoreReplaceSteps(gt))
	}
	gt.registerTool(server,
		policy.ToolMeta{Name: "fb_restore_replace", Tier: 2, Scope: "database", Preconditions: []policy.Precondition{
			{Name: "verified_backup_exists", Op: "true", Why: "a verified backup must exist in the catalog"},
			{Name: "backup_freshness", Op: "lt", Value: 24.0, Why: "newest verified backup must be < 24h old"},
		}},
		"Replace database %s from its newest backup (downtime; Tier 2 — out-of-band approval required).",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			if gt.wf == nil {
				return "", fmt.Errorf("workflow engine not initialised")
			}
			_, db, err := gt.client(dbID)
			if err != nil {
				return "", err
			}
			backupDir := db.BackupDir
			if backupDir == "" {
				backupDir = filepath.Dir(db.Path)
			}
			matches, _ := filepath.Glob(filepath.Join(backupDir, dbID+"_*.fbk"))
			var fbk string
			var newest time.Time
			found := false
			for _, m := range matches {
				if fi, err := os.Stat(m); err == nil && (!found || fi.ModTime().After(newest)) {
					newest, fbk, found = fi.ModTime(), m, true
				}
			}
			if !found {
				return "", fmt.Errorf("no backup artifact found")
			}
			id := fmt.Sprintf("wf%d", time.Now().UnixNano())
			return gt.wf.Run(ctx, id, "restore_replace", dbID, true, map[string]string{
				"fbk": fbk, "pre": db.Path + ".pre-restore", "path": db.Path,
			}, prog)
		})
}

func restoreReplaceSteps(gt *gatedTools) []workflows.StepDef {
	putBack := func(ctx context.Context, wf *state.Workflow) error {
		pre, path := wf.Detail["pre"], wf.Detail["path"]
		if pre == "" || path == "" {
			return nil
		}
		if _, err := os.Stat(pre); err != nil {
			return err
		}
		gt.pools.CloseDB(wf.Database)
		_ = os.Remove(path)
		return copyFile(pre, path)
	}
	return []workflows.StepDef{
		{Name: "snapshot_pre", Do: func(ctx context.Context, wf *state.Workflow, prog func(float64, string)) error {
			path, pre := wf.Detail["path"], wf.Detail["pre"]
			if _, err := os.Stat(path); err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			return copyFile(path, pre)
		}},
		{Name: "close_pools", Do: func(ctx context.Context, wf *state.Workflow, prog func(float64, string)) error {
			gt.pools.CloseDB(wf.Database)
			return nil
		}},
		{Name: "replace", Do: func(ctx context.Context, wf *state.Workflow, prog func(float64, string)) error {
			c, db, err := gt.client(wf.Database)
			if err != nil {
				return err
			}
			gt.pools.CloseDB(wf.Database)
			if err := os.Remove(db.Path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("cannot remove current database file (attached?): %w", err)
			}
			prog(0.4, "restoring "+wf.Detail["fbk"])
			return c.Restore(wf.Detail["fbk"], db.Path, false, 0, func(m string) { prog(0.7, m) })
		}, Compensate: putBack},
		{Name: "validate", Do: func(ctx context.Context, wf *state.Workflow, prog func(float64, string)) error {
			c, db, err := gt.client(wf.Database)
			if err != nil {
				return err
			}
			return c.Validate(db.Path, 0)
		}, Compensate: putBack},
		{Name: "verify_online", AlwaysRun: true, Do: func(ctx context.Context, wf *state.Workflow, prog func(float64, string)) error {
			return gt.pools.Health(ctx, wf.Database)
		}},
	}
}

func registerTraceTools(server *mcp.Server, gt *gatedTools) {
	gt.registerTool(server, policy.ToolMeta{Name: "fb_trace_start", Tier: 1, Scope: "database"},
		"Start a template-only trace on %s (args: {\"template\":\"audit-lite|performance|errors\"}).",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			tmpl, _ := args["template"].(string)
			if _, ok := backupsvc.TraceTemplates[tmpl]; !ok {
				return "", fmt.Errorf("unknown trace template %q (allowed: audit-lite, performance, errors)", tmpl)
			}
			c, db, err := gt.client(dbID)
			if err != nil {
				return "", err
			}
			work := db.WorkDir
			if work == "" {
				work = db.BackupDir
			}
			if work == "" {
				work = filepath.Dir(db.Path)
			}
			if err := os.MkdirAll(work, 0o755); err != nil {
				return "", err
			}
			id := fmt.Sprintf("t%d", time.Now().UnixNano())
			name := "fbmcp-" + dbID + "-" + tmpl + "-" + id
			path := filepath.Join(work, "trace_"+id+".log")
			f, err := os.Create(path)
			if err != nil {
				return "", err
			}
			lt, err := c.StartTrace(context.Background(), name, tmpl, f)
			if err != nil {
				f.Close()
				os.Remove(path)
				return "", err
			}
			gt.mu.Lock()
			if gt.traces == nil {
				gt.traces = map[string]*backupsvc.LiveTrace{}
			}
			gt.traces[id] = lt
			gt.mu.Unlock()
			_ = gt.st.PutTrace(state.TraceRec{ID: id, Database: dbID, Template: tmpl, Path: path, StartedAt: time.Now().UTC()})
			return fmt.Sprintf("trace started id=%s template=%s log=%s (stop with fb_trace_stop)", id, tmpl, path), nil
		})

	gt.registerTool(server, policy.ToolMeta{Name: "fb_trace_stop", Tier: 1, Scope: "database"},
		"Stop a trace started by this server on %s (args: {\"session_id\":\"t…\"}).",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			id, _ := args["session_id"].(string)
			if id == "" {
				return "", fmt.Errorf("session_id required")
			}
			gt.mu.Lock()
			lt := gt.traces[id]
			delete(gt.traces, id)
			gt.mu.Unlock()
			if err := lt.Stop(); err != nil {
				return "", err
			}
			_ = gt.st.RemoveTrace(id)
			return "trace stopped " + id, nil
		})

	type dbArg struct {
		Db string `json:"db"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_trace_list", Description: "Tier 0: list engine trace sessions and this server's tracked traces"}, func(ctx context.Context, req *mcp.CallToolRequest, a dbArg) (*mcp.CallToolResult, any, error) {
		c, _, err := gt.client(a.Db)
		if err != nil {
			return text("error: " + err.Error()), nil, nil
		}
		engine, err := c.ListTrace()
		if err != nil {
			engine = "(engine list unavailable: " + err.Error() + ")"
		}
		var b strings.Builder
		b.WriteString("engine:\n")
		b.WriteString(engine)
		b.WriteString("\ntracked by fbmcp:\n")
		for _, tr := range gt.st.Traces() {
			if tr.Database == a.Db {
				fmt.Fprintf(&b, "- id=%s template=%s log=%s started=%s\n", tr.ID, tr.Template, tr.Path, tr.StartedAt.Format(time.RFC3339))
			}
		}
		orphans := 0
		gt.mu.Lock()
		tracked := len(gt.traces)
		gt.mu.Unlock()
		for _, tr := range gt.st.Traces() {
			gt.mu.Lock()
			_, live := gt.traces[tr.ID]
			gt.mu.Unlock()
			if !live {
				orphans++
			}
		}
		fmt.Fprintf(&b, "in-process=%d persisted-orphans-this-process=%d (restart: orphans are reported, not auto-killed)\n", tracked, orphans)
		return text(b.String()), nil, nil
	})
}
