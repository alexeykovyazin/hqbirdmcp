// Phase 11 D3/D4/D5a — diagnostics reads: firebird.log, trace perf/deadlock
// reports, backup/restore job-family settings.
//
// Survey fact that shaped D3: the fbagent Local API LISTS rotated
// firebird.log copies (server/firebird/files/watched) but serves no content
// route for them, so log bodies come from the engine's own Services API
// (isc_action_svc_get_fb_log) — agent optional, works on every registered
// instance.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/agentapi"
	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/facts"
	"github.com/aleks/fbmcp/internal/identity"
)

// tier0Allowed runs the Tier-0 Evaluate + audit prologue shared by the
// phase-11 reads; returns a non-empty denial text when refused.
func tier0Allowed(gt *gatedTools, ctx context.Context, tool, dbKey string) string {
	id := identity.Caller(ctx)
	if d := gt.eng.Evaluate(id, dbKey, tool); d.Outcome == "deny" {
		gt.aud.Log(audit.Entry{Identity: id.Name, Database: dbKey, Tool: tool, Tier: 0, Decision: "denied", Detail: map[string]any{"reason": d.Reason}})
		return "DENIED: " + d.Reason
	}
	return ""
}

func tier0Done(gt *gatedTools, ctx context.Context, tool, dbKey string, err error) {
	id := identity.Caller(ctx)
	if err != nil {
		gt.aud.Log(audit.Entry{Identity: id.Name, Database: dbKey, Tool: tool, Tier: 0, Decision: "error", Detail: map[string]any{"reason": err.Error()}})
		return
	}
	gt.aud.Log(audit.Entry{Identity: id.Name, Database: dbKey, Tool: tool, Tier: 0, Decision: "allow"})
}

// tailLines keeps the last n lines of s (n<=0 → all).
func tailLines(s string, n int) string {
	if n <= 0 {
		return s
	}
	lines := strings.Split(strings.TrimRight(s, "\r\n"), "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n") + "\n"
}

func registerPhase11Tools(server *mcp.Server, gt *gatedTools) {
	// --- D3: fb_firebird_log (Tier 0) ---
	type fbLogArg struct {
		Instance string `json:"instance" jsonschema:"registered instance id (fbmcp.yaml instances:)"`
		Tail     int    `json:"tail,omitempty" jsonschema:"last N lines (default 100, max 5000, 0=default)"`
		Archived bool   `json:"archived,omitempty" jsonschema:"true: list the co-located fbagent's rotated firebird.log / conf-change copies instead of reading the live log (names only — the agent API serves no content for them)"`
		Glob     string `json:"glob,omitempty" jsonschema:"glob for the archived listing (default *)"`
		Agent    string `json:"agent,omitempty" jsonschema:"agent registry id for archived=true (default: the single configured agent)"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_firebird_log", Description: "Tier 0: read the instance's live firebird.log via the Services API (tail applied); archived=true lists the fbagent's rotated copies"}, func(ctx context.Context, req *mcp.CallToolRequest, a fbLogArg) (*mcp.CallToolResult, any, error) {
		if msg := tier0Allowed(gt, ctx, "fb_firebird_log", a.Instance); msg != "" {
			return errText(msg)
		}
		if a.Archived {
			cl, acfg, err := gt.agentClient(a.Agent)
			if err != nil {
				tier0Done(gt, ctx, "fb_firebird_log", a.Instance, err)
				return errText(err.Error())
			}
			raw, err := cl.GetWatchedFiles(ctx, a.Glob)
			tier0Done(gt, ctx, "fb_firebird_log", a.Instance, err)
			if err != nil {
				return errText(err.Error())
			}
			return text(fmt.Sprintf("rotated copies on agent %s (listing only; read the live log without archived=true):\n%s", acfg.ID, prettyJSON(raw))), nil, nil
		}
		logText, err := facts.FirebirdLog(gt.live(), a.Instance)
		tier0Done(gt, ctx, "fb_firebird_log", a.Instance, err)
		if err != nil {
			return errText(err.Error())
		}
		n := a.Tail
		if n <= 0 {
			n = 100
		}
		if n > 5000 {
			n = 5000
		}
		total := strings.Count(logText, "\n")
		out := tailLines(logText, n)
		return text(fmt.Sprintf("firebird.log (%s): last %d of ~%d lines\n---\n%s", a.Instance, min(n, total), total, out)), nil, nil
	})

	// --- D4: fb_agent_perf_report (Tier 0) ---
	type perfArg struct {
		agentArg
		Report string `json:"report,omitempty" jsonschema:"\"perf\" (default: newest trace performance report, top slow queries grouped by plan) or \"deadlocks\" (newest deadlock/conflict report)"`
	}
	registerAgentRead(server, gt, "fb_agent_perf_report", "newest trace performance report (report=perf) or deadlock report (report=deadlocks) from the agent artifact hub; refresh via fb_agent_job_run job=process_traces|find_deadlocks", func(ctx context.Context, cl *agentapi.Client, acfg config.Agent, a perfArg) (string, error) {
		path := "traces/perf-report/latest"
		switch strings.ToLower(strings.TrimSpace(a.Report)) {
		case "", "perf":
		case "deadlocks", "deadlock":
			path = "traces/deadlocks/latest"
		default:
			return "", fmt.Errorf("fbagent bad-request: report must be \"perf\" or \"deadlocks\"")
		}
		raw, err := cl.GetArtifact(ctx, "server", path, agentapi.ArtifactQuery{})
		if err != nil {
			return "", err
		}
		return prettyJSON(raw), nil
	})

	// --- D5a: fb_agent_backup_config (Tier 0) ---
	type backupCfgArg struct {
		agentArg
		Family string `json:"family,omitempty" jsonschema:"one job family (vbackup, nbackup, restore, sweep, grp_sweep, gfix_check, index_recalc, sql_script, scheduler, storage) — default: the backup/restore set"`
	}
	registerAgentRead(server, gt, "fb_agent_backup_config", "backup/restore/sweep job-family settings from the agent's newest redacted config snapshot; change them with fb_agent_config_set", func(ctx context.Context, cl *agentapi.Client, acfg config.Agent, a backupCfgArg) (string, error) {
		raw, err := cl.GetArtifact(ctx, "server", "config/snapshots", agentapi.ArtifactQuery{Limit: 1})
		if err != nil {
			return "", err
		}
		var env struct {
			Snapshots []map[string]json.RawMessage `json:"snapshots"`
		}
		if err := json.Unmarshal(raw, &env); err != nil || len(env.Snapshots) == 0 {
			return "", fmt.Errorf("no config snapshot available (enable agent_config_snapshot or wait for the first interval)")
		}
		snap := env.Snapshots[len(env.Snapshots)-1]
		families := []string{"vbackup", "nbackup", "restore", "sweep", "grp_sweep"}
		if f := strings.ToLower(strings.TrimSpace(a.Family)); f != "" {
			if !backupCfgFamilies[f] {
				return "", fmt.Errorf("fbagent bad-request: unknown family %q", a.Family)
			}
			families = []string{f}
		}
		out := map[string]any{}
		for _, f := range families {
			rawDoc, ok := snap[f]
			if !ok {
				continue
			}
			var doc any
			if err := json.Unmarshal(rawDoc, &doc); err != nil {
				continue
			}
			out[f] = redactSecrets(doc)
		}
		if len(out) == 0 {
			return "", fmt.Errorf("none of the requested families present in the snapshot")
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		return fmt.Sprintf("agent %s — job-family settings (newest config snapshot, secrets redacted):\n%s", acfg.ID, string(b)), nil
	})

	// --- D5b: fb_agent_config_set (Tier 2, OOB) ---
	registerAgentConfigSetTool(server, gt)
}

var backupCfgFamilies = map[string]bool{
	"vbackup": true, "nbackup": true, "restore": true, "sweep": true,
	"grp_sweep": true, "gfix_check": true, "index_recalc": true,
	"sql_script": true, "scheduler": true, "storage": true,
}

// redactSecrets walks a decoded JSON document and masks values whose key
// smells like a credential. Defense in depth: the agent redacts snapshot
// secrets itself, but this output reaches the model either way.
func redactSecrets(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			lk := strings.ToLower(k)
			if strings.Contains(lk, "token") || strings.Contains(lk, "password") || strings.Contains(lk, "secret") || strings.Contains(lk, "credential") {
				t[k] = "[redacted]"
				continue
			}
			t[k] = redactSecrets(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = redactSecrets(val)
		}
		return t
	default:
		return v
	}
}
