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
	"github.com/aleks/fbmcp/internal/gate"
	"github.com/aleks/fbmcp/internal/jobs"
	"github.com/aleks/fbmcp/internal/policy"
	"github.com/aleks/fbmcp/internal/state"
)

// executor runs the confirmed body of a gated tool as a job.
type executor func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error)

type gatedTools struct {
	cfg    *config.Config
	pools  *dbpool.Manager
	eng    *policy.Engine
	g      *gate.Gate
	runner *jobs.Runner
	aud    *audit.Logger
	st     *state.Store
	execs  map[string]executor

	mu   sync.Mutex
	args map[string]map[string]any // requestID -> tool args (in-memory; single instance per D8)
}

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
	gt.execs[meta.Name] = exec
	type dbArg struct {
		Db   string         `json:"db"`
		Args map[string]any `json:"args,omitempty"`
	}
	localID := policy.Identity{Name: "local", MaxTier: 2}
	mcp.AddTool(server, &mcp.Tool{Name: meta.Name, Description: fmt.Sprintf("Tier %d (gated): %s", meta.Tier, meta.Name)}, func(ctx context.Context, req *mcp.CallToolRequest, a dbArg) (*mcp.CallToolResult, any, error) {
		d := gt.eng.Evaluate(localID, a.Db, meta.Name)
		if d.Outcome == "deny" {
			gt.aud.Log(audit.Entry{Identity: "local", Database: a.Db, Tool: meta.Name, Tier: meta.Tier, Decision: "denied", Detail: map[string]interface{}{"reason": d.Reason}})
			return text("DENIED: " + d.Reason), nil, nil
		}
		argsJSON, _ := json.Marshal(a.Args)
		argHash := hashOf(a.Db + string(argsJSON))
		p, err := gt.g.Request(localID, a.Db, meta, fmt.Sprintf(impactFmt, a.Db), argHash, d.FailedPreconditions)
		if err != nil {
			return text("gate error: " + err.Error()), nil, nil
		}
		gt.mu.Lock()
		gt.args[p.ID] = a.Args
		gt.mu.Unlock()
		var b strings.Builder
		b.WriteString(gate.ImpactStatement(p))
		if meta.Tier <= 1 {
			fmt.Fprintf(&b, "In-band token (Tier 1 only): %s\n", gate.IssueToken(p.ID, argHash))
		}
		return text(b.String()), nil, nil
	})
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
	return gt.runner.Submit(p.Tool, p.Database, p.Identity, p.ID, func(ctx context.Context, prog func(float64, string)) (string, error) {
		return exec(ctx, p.Database, args, prog)
	})
}

func registerP3Tools(server *mcp.Server, gt *gatedTools) {
	// P3.1 — fb_backup_start (Tier 1): full backup into the DB's backup dir,
	// registered in the catalog (unverified until a test-restore).
	gt.registerTool(server, policy.ToolMeta{Name: "fb_backup_start", Tier: 1, Scope: "database"},
		"Full gbak backup of %s to its configured backup dir (async job).",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
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
			fbk := filepath.Join(backupDir, fmt.Sprintf("%s_%s.fbk", dbID, time.Now().Format("20060102_150405")))
			prog(0.1, "backup started")
			if err := c.Backup(db.Path, fbk, func(m string) { prog(0.5, m) }); err != nil {
				return "", fmt.Errorf("backup failed: %w", err)
			}
			if err := backupsvc.NewCatalog(gt.st).Register(dbID, fbk, false); err != nil {
				return "", err
			}
			return fmt.Sprintf("backup written: %s (unverified — run fb_restore_test to verify)", fbk), nil
		})

	// P3.2 — fb_restore_test (Tier 1): restore the newest artifact into the
	// work dir, validate, mark verified (verification = successful
	// test-restore per the P0.2 finding that gbak has no standalone verify).
	gt.registerTool(server, policy.ToolMeta{Name: "fb_restore_test", Tier: 1, Scope: "database"},
		"Test-restore of the newest backup of %s into the work dir (never touches the source DB).",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			c, db, err := gt.client(dbID)
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
				return "", fmt.Errorf("no backup artifacts in %s — run fb_backup_start first", backupDir)
			}
			workDir := db.WorkDir
			if workDir == "" {
				workDir = backupDir
			}
			restored := filepath.Join(workDir, "verify_"+dbID+".fdb")
			os.Remove(restored)
			prog(0.2, "restoring "+fbk)
			if err := c.Restore(fbk, restored, false, func(m string) { prog(0.6, m) }); err != nil {
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

// startApprovalWatcher polls the OOB approvals dir and confirms matching
// pending actions through the out-of-band channel (the LLM cannot reach it).
func (gt *gatedTools) startApprovalWatcher(ctx context.Context) {
	go func() {
		dir := filepath.Join(gt.cfg.State.Dir, "approvals")
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			ents, _ := os.ReadDir(dir)
			for _, e := range ents {
				id := e.Name()
				// confirm as the identity that opened the request; the trust
				// comes from the CHANNEL (operator-run CLI wrote the marker)
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
			}
		}
	}()
}

// registerRestore adds the Tier-2 guarded restore tool.
func (gt *gatedTools) registerRestore(server *mcp.Server) {
	gt.registerTool(server,
		policy.ToolMeta{Name: "fb_restore_replace", Tier: 2, Scope: "database", Preconditions: []policy.Precondition{
			{Name: "verified_backup_exists", Op: "true", Why: "a verified backup must exist in the catalog"},
			{Name: "backup_freshness", Op: "lt", Value: 24.0, Why: "newest verified backup must be < 24h old"},
		}},
		"Replace database %s from its newest backup (downtime; Tier 2 — out-of-band approval required).",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			c, db, err := gt.client(dbID)
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
			// safety net: keep a .pre-restore copy of the current file
			pre := db.Path + ".pre-restore"
			if _, err := os.Stat(db.Path); err == nil {
				data, err := os.ReadFile(db.Path)
				if err != nil {
					return "", err
				}
				if err := os.WriteFile(pre, data, 0o640); err != nil {
					return "", err
				}
			}
			// drop our own read-pool connections before replacing the file
			gt.pools.CloseDB(dbID)
			if err := os.Remove(db.Path); err != nil && !os.IsNotExist(err) {
				return "", fmt.Errorf("cannot remove current database file (attached?): %w", err)
			}
			prog(0.3, "restoring "+fbk)
			if err := c.Restore(fbk, db.Path, false, func(m string) { prog(0.7, m) }); err != nil {
				// failure leaves the system safer: try to put the old file back
				if preData, rerr := os.ReadFile(pre); rerr == nil {
					os.WriteFile(db.Path, preData, 0o640)
				}
				return "", fmt.Errorf("restore failed (previous file restored if possible): %w", err)
			}
			prog(0.9, "validating")
			if err := c.Validate(db.Path, 0); err != nil {
				return "", fmt.Errorf("restored database failed validation: %w", err)
			}
			return fmt.Sprintf("database restored from %s (previous copy kept at %s)", fbk, pre), nil
		})
}
