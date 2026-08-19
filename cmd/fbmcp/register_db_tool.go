package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/gate"
	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/policy"
)

type instanceArg struct {
	Instance string `json:"instance" jsonschema:"registry id of the Firebird instance"`
}

type registerDBArg struct {
	Instance       string `json:"instance" jsonschema:"registry id of the Firebird instance"`
	Path           string `json:"path" jsonschema:"absolute database path reported by discovery"`
	DB             string `json:"db,omitempty" jsonschema:"registry id to assign; defaults to a filename-based suggestion"`
	BackupDir      string `json:"backup_dir,omitempty" jsonschema:"optional override for backup_dir"`
	WorkDir        string `json:"work_dir,omitempty" jsonschema:"optional override for work_dir"`
	ROUser         string `json:"ro_user,omitempty" jsonschema:"optional override for ro_user"`
	ROSecretEnv    string `json:"ro_secret_env,omitempty" jsonschema:"optional override for ro_secret_env"`
	AdminUser      string `json:"admin_user,omitempty" jsonschema:"optional override for admin_user"`
	AdminSecretEnv string `json:"admin_secret_env,omitempty" jsonschema:"optional override for admin_secret_env"`
	Mode           string `json:"mode,omitempty" jsonschema:"preview or execute (default execute)"`
}

func registerDBTool(server *mcp.Server, gt *gatedTools) {
	meta := policy.ToolMeta{Name: "fb_db_register", Tier: 2, Scope: "instance"}
	gt.execs[meta.Name] = func(ctx context.Context, instanceID string, args map[string]any, prog func(float64, string)) (string, error) {
		if gt.live().SourcePath == "" {
			return "", fmt.Errorf("config source path unavailable; cannot persist registration")
		}
		cfg, err := config.Load(gt.live().SourcePath)
		if err != nil {
			return "", err
		}
		opt, err := registerOptionsFromArgs(args)
		if err != nil {
			return "", err
		}
		db, err := config.MaterializeDatabase(cfg, opt)
		if err != nil {
			return "", err
		}
		prog(0.5, "writing fbmcp.yaml")
		if err := config.RegisterDatabase(cfg.SourcePath, db); err != nil {
			return "", err
		}
		if gt.reloader != nil {
			res, rerr := gt.reloader.Apply("fb_db_register", liveJobID(gt, instanceID, "fb_db_register"))
			if rerr != nil {
				return "", fmt.Errorf("registered on disk but live reload failed: %w", rerr)
			}
			prog(1.0, res.Message)
			return fmt.Sprintf("registered database %s on instance %s; live config updated (%s)", db.ID, db.Instance, res.Class), nil
		}
		prog(1.0, "registration saved")
		return fmt.Sprintf("registered database %s on instance %s", db.ID, db.Instance), nil
	}
	mcp.AddTool(server, &mcp.Tool{Name: meta.Name, Description: "Tier 2 (gated): persist a discovered database into fbmcp.yaml; applied live after out-of-band confirm"}, func(ctx context.Context, req *mcp.CallToolRequest, a registerDBArg) (*mcp.CallToolResult, any, error) {
		return text(requestRegisterDB(ctx, gt, meta, a)), nil, nil
	})
}

func requestRegisterDB(ctx context.Context, gt *gatedTools, meta policy.ToolMeta, a registerDBArg) string {
	opt, summary, err := previewRegisterDB(gt.live(), a)
	if err != nil {
		return "DENIED: " + err.Error()
	}
	impact := "Persist a new database entry into fbmcp.yaml for a discovered database path."
	if summary != "" {
		impact += "\n" + summary
	}
	if strings.EqualFold(a.Mode, "preview") {
		return fmt.Sprintf("%s\n\nmode=preview (informational — confirmation still required to execute)\nTier: %d | Instance: %s\nAccepted confirmation channels: %s\n",
			impact, meta.Tier, opt.InstanceID, strings.Join(gate.AllowedChannels(meta.Tier), ", "))
	}
	id := identity.Caller(ctx)
	argPayload := map[string]any{
		"instance":         opt.InstanceID,
		"db":               opt.DBID,
		"path":             opt.Path,
		"backup_dir":       opt.BackupDir,
		"work_dir":         opt.WorkDir,
		"ro_user":          opt.ROUser,
		"ro_secret_env":    opt.ROSecretEnv,
		"admin_user":       opt.AdminUser,
		"admin_secret_env": opt.AdminSecretEnv,
	}
	argsJSON, _ := json.Marshal(argPayload)
	argHash := hashOf(opt.InstanceID + string(argsJSON))
	p, err := gt.g.Request(id, opt.InstanceID, meta, impact, argHash, nil)
	if err != nil {
		return "gate error: " + err.Error()
	}
	gt.mu.Lock()
	gt.args[p.ID] = argPayload
	gt.mu.Unlock()
	return gate.ImpactStatement(p)
}

func previewRegisterDB(cfg *config.Config, a registerDBArg) (config.RegisterOptions, string, error) {
	if strings.TrimSpace(a.Instance) == "" {
		return config.RegisterOptions{}, "", fmt.Errorf("instance is required")
	}
	if strings.TrimSpace(a.Path) == "" {
		return config.RegisterOptions{}, "", fmt.Errorf("path is required")
	}
	dbID := strings.TrimSpace(a.DB)
	if dbID == "" {
		dbID = config.SuggestedDBID(a.Path)
	}
	opt := config.RegisterOptions{
		InstanceID:     a.Instance,
		DBID:           dbID,
		Path:           a.Path,
		BackupDir:      a.BackupDir,
		WorkDir:        a.WorkDir,
		ROUser:         a.ROUser,
		ROSecretEnv:    a.ROSecretEnv,
		AdminUser:      a.AdminUser,
		AdminSecretEnv: a.AdminSecretEnv,
	}
	db, err := config.MaterializeDatabase(cfg, opt)
	if err != nil {
		return config.RegisterOptions{}, "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "instance: %s\n", db.Instance)
	fmt.Fprintf(&b, "db: %s\n", db.ID)
	fmt.Fprintf(&b, "path: %s\n", db.Path)
	fmt.Fprintf(&b, "backup_dir: %s\n", db.BackupDir)
	fmt.Fprintf(&b, "work_dir: %s\n", db.WorkDir)
	fmt.Fprintf(&b, "ro_user: %s\n", db.ROUser)
	fmt.Fprintf(&b, "ro_secret_env: %s\n", db.ROSecretEnv)
	fmt.Fprintf(&b, "admin_user: %s\n", db.AdminUser)
	fmt.Fprintf(&b, "admin_secret_env: %s\n", db.AdminSecretEnv)
	return opt, b.String(), nil
}

func liveJobID(gt *gatedTools, db, typ string) string {
	if gt == nil || gt.st == nil {
		return ""
	}
	for _, j := range gt.st.Jobs() {
		if j.Database == db && j.Type == typ && (j.State == "running" || j.State == "queued") {
			return j.ID
		}
	}
	return ""
}

func registerOptionsFromArgs(args map[string]any) (config.RegisterOptions, error) {
	opt := config.RegisterOptions{}
	asString := func(k string) string {
		if v, ok := args[k]; ok {
			return fmt.Sprint(v)
		}
		return ""
	}
	opt.InstanceID = asString("instance")
	opt.DBID = asString("db")
	opt.Path = asString("path")
	opt.BackupDir = asString("backup_dir")
	opt.WorkDir = asString("work_dir")
	opt.ROUser = asString("ro_user")
	opt.ROSecretEnv = asString("ro_secret_env")
	opt.AdminUser = asString("admin_user")
	opt.AdminSecretEnv = asString("admin_secret_env")
	if opt.InstanceID == "" || opt.DBID == "" || opt.Path == "" {
		return config.RegisterOptions{}, fmt.Errorf("missing registration args")
	}
	return opt, nil
}
