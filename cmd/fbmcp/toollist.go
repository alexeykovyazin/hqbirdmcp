// Tool list shaping (UX review W1.8): every client gets the whole tool list
// in its context, so list only the tools this caller can use with this
// config, and explain the shared rules once in the server instructions
// instead of in every description.
package main

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/policy"
)

// serverInstructions go to the client once, in the initialize result.
// Every fb_ name here must exist (TestServerInstructionsNameRealTools).
const serverInstructions = `fbmcp manages Firebird databases. Start with fb_db_list (database ids), then fb_db_health db=<id>.

Tiers: 0 = read, runs at once. 1 = change: the reply has a request id and token; show the impact to the user, and after they agree call fb_confirm. 2 = risky: an operator approves out-of-band (fbmcp tray, or fbmcp ctl approve on the server); then call fb_job_status with the request_id. Tier 2 also needs an open maintenance window (fb_window_open) and a verified backup younger than 24 h (fb_backup_start, then fb_restore_test).
Gated tools take db, args (an object) and mode. mode=preview shows the impact and what blocks the request, and changes nothing. Replies end with "Next:"; do that step.

Want -> use:
- read data: fb_query (check a draft first with fb_query_check)
- change data or DDL: fb_write
- schema: fb_schema_list, fb_describe
- slow query: fb_analyze_query, fb_index_advice
- sessions, long transactions: fb_sessions, fb_transactions
- statistics: fb_gstat, fb_index_stats, fb_trends
- backup: fb_backup_start; prove it restores: fb_restore_test
- server log: fb_firebird_log
- firebird.conf: fb_config_get, fb_config_set (fb_config_reload reloads fbmcp.yaml, not firebird.conf)
- job progress: fb_job_status (job_id, or request_id after an out-of-band approval)
- fbagent (HQbird): fb_agent_discover finds local agents; the other fb_agent_ tools are listed when agents are configured`

// agentToolsWithoutAgents stay listed with an empty agents: section: they
// are how an operator gets the first agent configured.
var agentToolsWithoutAgents = map[string]bool{"fb_agent_discover": true, "fb_agent_register": true}

var migrationTools = map[string]bool{
	"fb_migration_plan": true, "fb_migration_apply": true, "fb_migration_status": true, "fb_migration_rollback_plan": true,
}

// confirmTools are useless for a read-only identity.
var confirmTools = map[string]bool{"fb_confirm": true, "fb_cancel": true}

// toolVisible reports whether tools/list shows a tool to this identity with
// this config. Hidden tools stay registered: a direct call still goes
// through policy and gets a clear denial.
func toolVisible(name string, id policy.Identity, cfg *config.Config, tiers map[string]int) bool {
	if tier, ok := tiers[name]; ok {
		if tier >= 3 || tier > id.MaxTier {
			return false // Tier 3 is always denied; above the ceiling is always denied
		}
	}
	if confirmTools[name] && id.MaxTier < 1 {
		return false
	}
	if strings.HasPrefix(name, "fb_agent_") && !agentToolsWithoutAgents[name] && len(cfg.Agents) == 0 {
		return false
	}
	if strings.HasPrefix(name, "fb_cluster_") && !cfg.RCM.On() {
		return false
	}
	if migrationTools[name] && !anyMigrationsDir(cfg) {
		return false
	}
	return true
}

func anyMigrationsDir(cfg *config.Config) bool {
	for _, db := range cfg.Databases {
		if db.MigrationsDir != "" {
			return true
		}
	}
	return false
}

func toolTiers(meta []policy.ToolMeta) map[string]int {
	m := make(map[string]int, len(meta))
	for _, t := range meta {
		m[t.Name] = t.Tier
	}
	return m
}

// toolListFilter filters the tools/list result per caller.
func toolListFilter(handle *config.Handle, meta []policy.ToolMeta) mcp.Middleware {
	tiers := toolTiers(meta)
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if err != nil || method != "tools/list" {
				return res, err
			}
			lr, ok := res.(*mcp.ListToolsResult)
			if !ok {
				return res, err
			}
			id := identity.Caller(ctx)
			cfg := handle.Current()
			out := *lr
			out.Tools = make([]*mcp.Tool, 0, len(lr.Tools))
			for _, t := range lr.Tools {
				if toolVisible(t.Name, id, cfg, tiers) {
					out.Tools = append(out.Tools, t)
				}
			}
			return &out, nil
		}
	}
}

// relistIfChanged sends tools/list_changed when a reload changed the
// visible tool set (agents or migrations added or removed), so clients
// fetch the list again.
func (gt *gatedTools) relistIfChanged(cfg *config.Config) {
	key := toolListKey(cfg)
	gt.mu.Lock()
	changed := gt.toolKey != key
	gt.toolKey = key
	gt.mu.Unlock()
	if changed && gt.relist != nil {
		gt.relist()
	}
}

// toolListKey is the part of the config that changes the visible tool set.
func toolListKey(cfg *config.Config) string {
	return strings.Join([]string{boolKey(len(cfg.Agents) > 0), boolKey(anyMigrationsDir(cfg)), boolKey(cfg.RCM.On())}, ",")
}

func boolKey(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
