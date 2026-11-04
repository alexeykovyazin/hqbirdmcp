// Phase 11 D5b — fb_agent_config_set (Tier 2, out-of-band; ADR-037).
//
// Write path = the agent's own candidate mechanism: compose a whole
// agent_config.json from the live file with one narrow patch, drop it as
// agent_config_pending_<UTC>.json beside the live file, wait out the agent's
// 2s settle time, then POST /v1/config/reload. The agent validates, backs up,
// installs, activates and rolls back on failure. No API route accepts a
// config document, and the redacted snapshot cannot be a base (its "***"
// would overwrite every credential), so the live file must be readable by
// the fbmcp account — a deployment decision the operator makes explicitly.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/agentcfg"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/policy"
)

// readAgentConfig reads the live agent_config.json named by the agents:
// entry, with an actionable message for the usual ACL refusal.
func readAgentConfig(acfg config.Agent) ([]byte, error) {
	if acfg.ConfigPath == "" {
		return nil, fmt.Errorf("agent %q has no config_path: re-register it with the config_path from fb_agent_discover", acfg.ID)
	}
	b, err := os.ReadFile(acfg.ConfigPath)
	if errors.Is(err, fs.ErrPermission) {
		return nil, fmt.Errorf("cannot read %s: access denied. The live config holds the agent's secrets and is ACL-locked; to allow fb_agent_config_set, the operator grants the fbmcp account read on that file and create on its directory (docs/agent-bridge.md §config writes). Until then use HQ Monitor", acfg.ConfigPath)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", acfg.ConfigPath, err)
	}
	return b, nil
}

// agentRefused reports whether a reload error is the agent's answer about
// the candidate (it then archives it as rejected). Auth, transport and 5xx
// errors leave the candidate pending.
func agentRefused(err error) bool {
	s := err.Error()
	return strings.HasPrefix(s, "fbagent bad-request") || strings.HasPrefix(s, "fbagent conflict") ||
		strings.HasPrefix(s, "fbagent payload-too-large")
}

func registerAgentConfigSetTool(server *mcp.Server, gt *gatedTools) {
	type cfgSetArg struct {
		Agent  string         `json:"agent,omitempty" jsonschema:"agent registry id (optional when exactly one agent is configured)"`
		Db     string         `json:"db,omitempty" jsonschema:"registry database id — maintenance-window key (Tier 2)"`
		Family string         `json:"family" jsonschema:"job family: vbackup, nbackup, restore, sweep, grp_sweep, gfix_check, index_recalc, scheduler"`
		JobID  string         `json:"job_id,omitempty" jsonschema:"patch one job (its id inside the family's jobs lists) instead of the family root"`
		Task   string         `json:"task,omitempty" jsonschema:"for family scheduler only: the scheduler.tasks name (keys: enabled, schedule, timeout_seconds)"`
		Patch  map[string]any `json:"patch" jsonschema:"nested object of EXISTING settings to change, e.g. {\"cronExpression\":\"0 0 1 * * ?\",\"retention\":{\"maxBackups\":7}}; same JSON type only; credentials, ids, commands and job lists are refused"`
		Mode   string         `json:"mode,omitempty" jsonschema:"preview (show the exact diff, no request) or execute (default)"`
	}
	meta := policy.ToolMeta{Name: "fb_agent_config_set", Tier: 2, Scope: "instance"}

	gt.execs[meta.Name] = func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
		acfg, err := gt.agentCfg(agentStr(args["agent"]))
		if err != nil {
			return "", err
		}
		live, err := readAgentConfig(acfg)
		if err != nil {
			return "", err
		}
		if agentcfg.Fingerprint(live) != agentStr(args["base_sha"]) {
			return "", fmt.Errorf("agent_config.json changed since this request was approved; nothing written — run fb_agent_config_set again to review the new diff")
		}
		res, err := agentcfg.Apply(live, agentStr(args["family"]), agentStr(args["job_id"]), agentStr(args["task"]), agentParams(args["patch"]))
		if err != nil {
			return "", err
		}
		if p, _ := agentcfg.Pending(acfg.ConfigPath); len(p) > 0 {
			return "", fmt.Errorf("another candidate is already waiting (%s); let the agent apply it or remove it first", strings.Join(p, ", "))
		}
		// Check the token before the write: the agent promotes a pending
		// candidate by itself on its next config tick, so a reload that
		// fails on auth after the write would still change the config
		// (phase 12 test W: a wrong token gave 401 only after the write).
		cl, _, err := gt.agentClient(acfg.ID)
		if err != nil {
			return "", fmt.Errorf("nothing written: %w", err)
		}
		if _, err := cl.GetInstances(ctx, ""); err != nil {
			return "", fmt.Errorf("nothing written: agent %s check failed (%v); fix the token or the agent first", acfg.ID, err)
		}
		prog(0.2, "writing candidate")
		name, err := agentcfg.WriteCandidate(acfg.ConfigPath, res.Doc, time.Now())
		if err != nil {
			return "", err
		}
		prog(0.4, "candidate "+name+" written; waiting for the agent settle time")
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("cancelled after writing %s (the agent promotes it on its next config tick): %w", name, ctx.Err())
		case <-time.After(agentcfg.SettleTime):
		}
		prog(0.6, "POST /v1/config/reload")
		raw, err := cl.ReloadConfig(ctx)
		if err != nil {
			if agentRefused(err) {
				return "", fmt.Errorf("agent refused candidate %s (it archives it as agent_config_rejected_*; live config unchanged): %w", name, err)
			}
			return "", fmt.Errorf("reload request failed (%v); candidate %s is still pending and the agent promotes it on its next config tick — check with fb_agent_backup_config", err, name)
		}
		var rr struct {
			Reloaded           bool     `json:"reloaded"`
			PromotedFrom       string   `json:"promoted_from"`
			RestartRequiredFor []string `json:"restart_required_for"`
			SyncedPaths        []string `json:"synced_paths"`
		}
		_ = json.Unmarshal(raw, &rr)
		var b strings.Builder
		fmt.Fprintf(&b, "agent %s: %s updated\n%s\n", acfg.ID, res.Target, strings.Join(res.Diff, "\n"))
		switch {
		case rr.PromotedFrom == name:
			fmt.Fprintf(&b, "candidate %s promoted and reloaded (agent backed up the previous config as agent_config_backup_*)", name)
		case rr.PromotedFrom != "":
			fmt.Fprintf(&b, "WARNING: the agent promoted %s, not %s — check the agent directory", rr.PromotedFrom, name)
		default:
			fmt.Fprintf(&b, "WARNING: reload ran but did not promote %s (not settled yet?); the agent applies it on its next config tick — verify with fb_agent_backup_config", name)
		}
		if len(rr.RestartRequiredFor) > 0 {
			fmt.Fprintf(&b, "\nrestart_required_for: %s (consumed only at agent start)", strings.Join(rr.RestartRequiredFor, ", "))
		}
		if len(rr.SyncedPaths) > 0 {
			fmt.Fprintf(&b, "\nsynced_paths: %s", strings.Join(rr.SyncedPaths, ", "))
		}
		prog(1.0, "done")
		return b.String(), nil
	}

	mcp.AddTool(server, &mcp.Tool{Name: meta.Name, Description: "Tier 2 (gated): change backup/restore/sweep job settings in the fbagent config via the agent's candidate file + reload (out-of-band approval); preview shows the exact diff"}, func(ctx context.Context, req *mcp.CallToolRequest, a cfgSetArg) (*mcp.CallToolResult, any, error) {
		acfg, err := gt.agentCfg(a.Agent)
		if err != nil {
			return errText(err.Error())
		}
		live, err := readAgentConfig(acfg)
		if err != nil {
			return errText(err.Error())
		}
		res, err := agentcfg.Apply(live, a.Family, a.JobID, a.Task, a.Patch)
		if err != nil {
			return errText("fbagent bad-request: " + err.Error())
		}
		if p, _ := agentcfg.Pending(acfg.ConfigPath); len(p) > 0 {
			return errText(fmt.Sprintf("another candidate is already waiting beside %s (%s); let the agent apply it or remove it first", acfg.ConfigPath, strings.Join(p, ", ")))
		}
		impact := fmt.Sprintf("Change fbagent %q configuration: %s\n%s\n\nMechanism: write agent_config_pending_<UTC>.json beside %s, then POST /v1/config/reload — the agent validates, backs up and installs it, and rolls back if it does not load. Approval is bound to the current file (sha256 %s…).",
			acfg.ID, res.Target, strings.Join(res.Diff, "\n"), acfg.ConfigPath, agentcfg.Fingerprint(live)[:12])
		argsMap := map[string]any{"agent": acfg.ID, "family": a.Family, "job_id": a.JobID, "task": a.Task, "patch": a.Patch, "base_sha": agentcfg.Fingerprint(live)}
		msg, denied := gt.requestGatedMeta(ctx, meta, impact, a.Db, argsMap, a.Mode)
		if denied {
			return errText(msg)
		}
		return text(msg), nil, nil
	})
}
