// Phase 9 — fbagent Local API bridge (phase9_plan.md, ADR-031..033).
// Tier 0 read tools proxy the co-located fbagent directly (Evaluate +
// audit, no gate); Tier 1/2 go through the standard gated flow. The HTTP
// client is constructed lazily per call — registration stays hermetic.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/agentapi"
	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/gate"
	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/policy"
)

// agentTier2Jobs is the ADR-032 hot list: job families whose run is
// classified Tier 2 (service-affecting) by fb_agent_job_run. Extensible per
// agent via agents[].tier2_jobs.
var agentTier2Jobs = []string{"restore", "sql_script", "vbackup", "grpvbackup", "nbackup_dump", "grpnbackup_dump"}

func agentJobTier(acfg config.Agent, job string) int {
	for _, j := range append(agentTier2Jobs, acfg.Tier2Jobs...) {
		if j == job {
			return 2
		}
	}
	return 1
}

// agentCfg resolves the target agent from the live config (no token
// resolution, no dialing — safe pre-gate).
func (gt *gatedTools) agentCfg(agentID string) (config.Agent, error) {
	agents := gt.live().Agents
	if len(agents) == 0 {
		return config.Agent{}, fmt.Errorf("no agent configured: add an agents: entry to fbmcp.yaml (phase9 bridge)")
	}
	if agentID == "" {
		if len(agents) > 1 {
			ids := make([]string, len(agents))
			for i, a := range agents {
				ids[i] = a.ID
			}
			return config.Agent{}, fmt.Errorf("multiple agents configured (%s): pass agent", strings.Join(ids, ", "))
		}
		return agents[0], nil
	}
	for _, a := range agents {
		if a.ID == agentID {
			return a, nil
		}
	}
	return config.Agent{}, fmt.Errorf("unknown agent id %q", agentID)
}

// agentClient resolves the agent + token and builds the HTTP client.
func (gt *gatedTools) agentClient(agentID string) (*agentapi.Client, config.Agent, error) {
	acfg, err := gt.agentCfg(agentID)
	if err != nil {
		return nil, config.Agent{}, err
	}
	tok, err := config.SecretFromEnv(acfg.TokenEnv)
	if err != nil {
		return nil, config.Agent{}, fmt.Errorf("agent %q token: %w", acfg.ID, err)
	}
	cl, err := agentapi.New(acfg.BaseURL, tok)
	if err != nil {
		return nil, config.Agent{}, err
	}
	return cl, acfg, nil
}

// agentWaitOp polls an async operation within the agent's timeout budget.
func (gt *gatedTools) agentWaitOp(ctx context.Context, cl *agentapi.Client, acfg config.Agent, opID string, prog func(float64, string)) (agentapi.Operation, error) {
	pctx, cancel := context.WithTimeout(ctx, time.Duration(acfg.TimeoutOrDefault())*time.Second)
	defer cancel()
	return cl.PollOperation(pctx, opID, 2*time.Second, prog)
}

// --- arg/coercion helpers ---

func agentStr(v any) string {
	s, _ := v.(string)
	return s
}

func agentInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

func agentStrSlice(v any) []string {
	switch s := v.(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, e := range s {
			out = append(out, agentStr(e))
		}
		return out
	}
	return nil
}

func agentParams(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "(empty response)"
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err == nil {
		return buf.String()
	}
	return string(raw)
}

// --- Tier 0 read tools ---

// agentArg is the base argument shape of every bridge tool (embedded into
// the per-tool structs).
type agentArg struct {
	Agent string `json:"agent,omitempty" jsonschema:"agent registry id (optional when exactly one agent is configured)"`
	Db    string `json:"db,omitempty" jsonschema:"registry database id (audit key)"`
}

// registerAgentRead wires one Tier-0 bridge tool: Evaluate (identity
// ceiling + unknown-tool guard), audit, client resolution, then the proxy
// call. Generic over the args struct so each tool gets an exact schema.
func registerAgentRead[A any](server *mcp.Server, gt *gatedTools, name, desc string, fn func(ctx context.Context, cl *agentapi.Client, acfg config.Agent, a A) (string, error)) {
	mcp.AddTool(server, &mcp.Tool{Name: name, Description: "Tier 0: " + desc}, func(ctx context.Context, req *mcp.CallToolRequest, a A) (*mcp.CallToolResult, any, error) {
		id := identity.Caller(ctx)
		// Extract the embedded agentArg (audit key + agent selector) via a
		// JSON round-trip — every tool's struct embeds it.
		b, _ := json.Marshal(a)
		var base agentArg
		_ = json.Unmarshal(b, &base)
		if d := gt.eng.Evaluate(id, base.Db, name); d.Outcome == "deny" {
			gt.aud.Log(audit.Entry{Identity: id.Name, Database: base.Db, Tool: name, Tier: 0, Decision: "denied", Detail: map[string]any{"reason": d.Reason}})
			return errText("DENIED: " + d.Reason)
		}
		cl, acfg, err := gt.agentClient(base.Agent)
		if err == nil {
			var out string
			out, err = fn(ctx, cl, acfg, a)
			if err == nil {
				gt.aud.Log(audit.Entry{Identity: id.Name, Database: base.Db, Tool: name, Tier: 0, Decision: "allow", Detail: map[string]any{"agent": acfg.ID}})
				return text(out), nil, nil
			}
		}
		gt.aud.Log(audit.Entry{Identity: id.Name, Database: base.Db, Tool: name, Tier: 0, Decision: "error", Detail: map[string]any{"reason": err.Error()}})
		return errText(err.Error())
	})
}

// --- Tier 1/2 gated executors ---

// agentEngineExec builds the executor for restart/stop/start: 202 → poll →
// report (ADR-033).
func (gt *gatedTools) agentEngineExec(action string) executor {
	return func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
		cl, acfg, err := gt.agentClient(agentStr(args["agent"]))
		if err != nil {
			return "", err
		}
		req := agentapi.EngineOpRequest{
			RequestedBy: agentStr(args["_grant_identity"]),
			Reason:      agentStr(args["reason"]),
		}
		if b, ok := args["coalesce"].(bool); ok {
			req.Coalesce = &b
		}
		resp, err := cl.EngineOp(ctx, acfg.InstanceIDOrDefault(), action, req)
		if err != nil {
			return "", err
		}
		prog(0.1, fmt.Sprintf("operation %s accepted", resp.OperationID))
		op, err := gt.agentWaitOp(ctx, cl, acfg, resp.OperationID, prog)
		if err != nil {
			return "", err
		}
		if op.State == "failed" {
			return "", fmt.Errorf("agent %s failed: %s", action, agentapi.FormatOperation(op))
		}
		return fmt.Sprintf("agent %s via agent %q:\n%s", action, acfg.ID, agentapi.FormatOperation(op)), nil
	}
}

// agentUpdateApplyExec applies a prepared Firebird update and, when the
// agent enqueued the nested restart, polls that too (ADR-033).
func (gt *gatedTools) agentUpdateApplyExec() executor {
	return func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
		cl, acfg, err := gt.agentClient(agentStr(args["agent"]))
		if err != nil {
			return "", err
		}
		resp, err := cl.ApplyUpdate(ctx, acfg.InstanceIDOrDefault(), agentStr(args["_grant_identity"]), agentStr(args["reason"]))
		if err != nil {
			return "", err
		}
		prog(0.1, fmt.Sprintf("update operation %s accepted", resp.OperationID))
		op, err := gt.agentWaitOp(ctx, cl, acfg, resp.OperationID, prog)
		if err != nil {
			return "", err
		}
		if op.State == "failed" {
			return "", fmt.Errorf("firebird update failed: %s", agentapi.FormatOperation(op))
		}
		out := fmt.Sprintf("firebird update via agent %q:\n%s", acfg.ID, agentapi.FormatOperation(op))
		if rid := op.RestartOpID(); rid != "" {
			prog(0.7, "polling nested engine restart "+rid)
			rop, err := gt.agentWaitOp(ctx, cl, acfg, rid, prog)
			if err != nil {
				return out + fmt.Sprintf("\nnested restart %s: poll later with fb_agent_operation (%v)", rid, err), nil
			}
			out += "\nnested restart:\n" + agentapi.FormatOperation(rop)
		}
		return out, nil
	}
}

// runAgentJob is the fb_agent_job_run executor: POST run → poll operation.
func (gt *gatedTools) runAgentJob(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
	cl, acfg, err := gt.agentClient(agentStr(args["agent"]))
	if err != nil {
		return "", err
	}
	job := agentStr(args["job"])
	req := agentapi.JobRunRequest{
		RequestedBy: agentStr(args["_grant_identity"]),
		Reason:      agentStr(args["reason"]),
		JobID:       agentStr(args["job_id"]),
		DBKeys:      agentStrSlice(args["db_keys"]),
		Params:      agentParams(args["params"]),
	}
	resp, err := cl.RunJob(ctx, job, req)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "job %q accepted by agent %q (operation %s)", job, acfg.ID, resp.OperationID)
	if len(resp.Warnings) > 0 {
		fmt.Fprintf(&b, "\nwarnings: %s", strings.Join(resp.Warnings, "; "))
	}
	if resp.StatusURL != "" {
		fmt.Fprintf(&b, "\nstatus_url: %s", resp.StatusURL)
	}
	op, err := gt.agentWaitOp(ctx, cl, acfg, resp.OperationID, prog)
	if err != nil {
		return "", fmt.Errorf("%w\n(%s)", err, b.String())
	}
	fmt.Fprintf(&b, "\n%s", agentapi.FormatOperation(op))
	if op.State == "failed" {
		return "", fmt.Errorf("job %q failed: %s", job, b.String())
	}
	if op.Kind == "job_enqueue" {
		fmt.Fprintf(&b, "\n(enqueued: the agent scheduler picks the run up; check the agent-side status via fb_agent_job_status)")
	}
	return b.String(), nil
}

// agentJobRunArg is the fb_agent_job_run argument schema (package level so
// tests can drive the gate path directly).
type agentJobRunArg struct {
	Agent  string         `json:"agent,omitempty" jsonschema:"agent registry id (optional when exactly one agent is configured)"`
	Db     string         `json:"db,omitempty" jsonschema:"registry database id — required for Tier-2-classified jobs (maintenance-window key)"`
	Job    string         `json:"job" jsonschema:"agent scheduler job name, e.g. sweep, grp_sweep, gfix_check, collect_metadata, sql_script"`
	JobID  string         `json:"job_id,omitempty" jsonschema:"configured job id (required by forced-queue families; ad-hoc allowed for sweep/grp_sweep/gfix_check/index_recalc with db_keys)"`
	DBKeys []string       `json:"db_keys,omitempty" jsonschema:"restrict the run to these agent-side database keys"`
	Reason string         `json:"reason,omitempty"`
	Params map[string]any `json:"params,omitempty" jsonschema:"per-family params: level (nbackup), scriptPath/scriptBody (sql_script), sync/force_outside_window (sweep)"`
	Mode   string         `json:"mode,omitempty" jsonschema:"preview or execute (default execute)"`
}

// requestAgentJobRun is the pre-gate body of fb_agent_job_run: validation,
// dynamic-tier classification (ADR-032), impact statement, gate request.
func (gt *gatedTools) requestAgentJobRun(ctx context.Context, a agentJobRunArg) (string, bool) {
	acfg, err := gt.agentCfg(a.Agent)
	if err != nil {
		return err.Error(), true
	}
	if a.Job == "" {
		return "fbagent bad-request: job is required", true
	}
	if a.Job == "sql_script" {
		if agentStr(a.Params["scriptPath"]) == "" && agentStr(a.Params["scriptBody"]) == "" {
			return "fbagent bad-request: sql_script requires params.scriptPath or params.scriptBody (one is required)", true
		}
	}
	tier := agentJobTier(acfg, a.Job)
	tierNote := ""
	if tier == 2 {
		tierNote = " (hot-list family: Tier 2 — maintenance window on db required, out-of-band approval)"
	}
	paramKeys := make([]string, 0, len(a.Params))
	for k := range a.Params {
		paramKeys = append(paramKeys, k)
	}
	impact := fmt.Sprintf("Run fbagent scheduler job %q on agent %q (instance %s).\njob_id: %q\ndb_keys: %v\nparams keys: %v\nrequested_by will record the confirming identity.\nDynamic tier: %d%s.",
		a.Job, acfg.ID, acfg.InstanceIDOrDefault(), a.JobID, a.DBKeys, paramKeys, tier, tierNote)
	argsMap := map[string]any{"agent": a.Agent, "job": a.Job, "job_id": a.JobID, "db_keys": a.DBKeys, "reason": a.Reason, "params": a.Params}
	return gt.requestGatedMeta(ctx, policy.ToolMeta{Name: "fb_agent_job_run", Tier: tier, Scope: "instance"}, impact, a.Db, argsMap, a.Mode)
}

// requestGatedMeta is requestGated (p3tools.go) with an explicit meta —
// the dynamic-tier path (fb_write uses EvaluateMeta the same way). Returns
// (text, denied). Kept separate from requestGated on purpose: the static
// path keeps its Evaluate-by-name behavior untouched.
func (gt *gatedTools) requestGatedMeta(ctx context.Context, meta policy.ToolMeta, impact, dbID string, args map[string]any, mode string) (string, bool) {
	if _, err := gt.cfg.DB(dbID); err != nil && meta.Scope == "database" {
		return "DENIED: " + err.Error(), true
	}
	id := identity.Caller(ctx)
	d := gt.eng.EvaluateMeta(id, dbID, meta)
	if d.Outcome == "deny" {
		gt.aud.Log(audit.Entry{Identity: id.Name, Database: dbID, Tool: meta.Name, Tier: meta.Tier, Decision: "denied", Detail: map[string]any{"reason": d.Reason}})
		return "DENIED: " + d.Reason, true
	}
	if strings.EqualFold(mode, "preview") {
		var bld strings.Builder
		bld.WriteString(impact)
		fmt.Fprintf(&bld, "\n\nmode=preview (informational — confirmation still required to execute)\nTier: %d | Database: %s\nAccepted confirmation channels: %s\n",
			meta.Tier, dbID, strings.Join(gate.AllowedChannels(meta.Tier), ", "))
		if len(d.FailedPreconditions) > 0 {
			fmt.Fprintf(&bld, "preconditions currently failing: %s\n", strings.Join(d.FailedPreconditions, "; "))
		}
		return bld.String(), false
	}
	if len(d.FailedPreconditions) > 0 {
		return "DENIED: " + d.Reason, true
	}
	argsJSON, _ := json.Marshal(args)
	argHash := hashOf(dbID + string(argsJSON))
	p, err := gt.g.Request(id, dbID, meta, impact, argHash, d.FailedPreconditions)
	if err != nil {
		return "gate error: " + err.Error(), true
	}
	gt.mu.Lock()
	gt.args[p.ID] = args
	gt.mu.Unlock()
	var bld strings.Builder
	bld.WriteString(gate.ImpactStatement(p))
	if meta.Tier <= 1 {
		fmt.Fprintf(&bld, "In-band token (Tier 1 only): %s\n", gate.IssueToken(p.ID, argHash))
	}
	return bld.String(), false
}

// --- registration ---

func registerAgentTools(server *mcp.Server, gt *gatedTools) {
	// --- Tier 0: direct reads (Evaluate + audit, no gate) ---

	type instancesArg struct {
		agentArg
		InstanceID string `json:"instance_id,omitempty" jsonschema:"agent instance id override (default from config, usually fb1)"`
	}
	registerAgentRead(server, gt, "fb_agent_instances", "list (or, with instance_id, detail) the fbagent-managed Firebird instance(s): state, uptime, port", func(ctx context.Context, cl *agentapi.Client, acfg config.Agent, a instancesArg) (string, error) {
		id := acfg.InstanceIDOrDefault()
		if a.InstanceID != "" {
			id = a.InstanceID
		}
		raw, err := cl.GetInstances(ctx, id)
		if err != nil {
			return "", err
		}
		return prettyJSON(raw), nil
	})

	type statusArg struct {
		agentArg
		Section string `json:"section,omitempty" jsonschema:"backups or upgrade"`
		View    string `json:"view,omitempty" jsonschema:"health"`
	}
	registerAgentRead(server, gt, "fb_agent_status", "read the agent's server_status.json (full, or section=backups|upgrade, or view=health)", func(ctx context.Context, cl *agentapi.Client, acfg config.Agent, a statusArg) (string, error) {
		if a.View != "" && a.Section != "" {
			return "", fmt.Errorf("pass section or view, not both")
		}
		raw, err := cl.GetStatus(ctx, a.Section, a.View)
		if err != nil {
			return "", err
		}
		return prettyJSON(raw), nil
	})

	registerAgentRead(server, gt, "fb_agent_databases", "latest active-databases artifact from the agent collector", func(ctx context.Context, cl *agentapi.Client, acfg config.Agent, a agentArg) (string, error) {
		raw, err := cl.GetDatabases(ctx)
		if err != nil {
			return "", err
		}
		return prettyJSON(raw), nil
	})

	type artListArg struct {
		agentArg
		Scope string `json:"scope" jsonschema:"\"server\" or a database folder key (the sanitized on-disk folder name)"`
		Glob  string `json:"glob" jsonschema:"glob matched against the slash-relative path and base name (e.g. *gstat_full*)"`
	}
	registerAgentRead(server, gt, "fb_agent_artifact_list", "glob listing under server_level/ (scope=server) or a database folder (scope=<dbKey>); max 200 results", func(ctx context.Context, cl *agentapi.Client, acfg config.Agent, a artListArg) (string, error) {
		if err := agentapi.ValidateScope(a.Scope); err != nil {
			return "", fmt.Errorf("fbagent bad-request: %v", err)
		}
		if a.Glob == "" {
			return "", fmt.Errorf("fbagent bad-request: glob is required")
		}
		raw, err := cl.GetArtifactList(ctx, a.Scope, a.Glob)
		if err != nil {
			return "", err
		}
		return prettyJSON(raw), nil
	})

	type artGetArg struct {
		agentArg
		Scope string `json:"scope" jsonschema:"\"server\" or a database folder key"`
		Path  string `json:"path" jsonschema:"artifact path, e.g. backups/status, lwm/summary, gstat-full/latest, metrics/latest"`
		Tail  int    `json:"tail,omitempty" jsonschema:"last N lines (db-scoped */latest artifacts only)"`
		Date  string `json:"date,omitempty" jsonschema:"YYYY-MM-DD (server lwm/summary only)"`
		At    string `json:"at,omitempty" jsonschema:"RFC3339 instant (server lwm/summary|raw only)"`
		Limit int    `json:"limit,omitempty" jsonschema:"server config/snapshots (1-100) and traces/processed (<=200) only"`
	}
	registerAgentRead(server, gt, "fb_agent_artifact_get", "read one agent artifact (JSON parsed; non-JSON wrapped as {file, body}); 413 over-cap bodies advise tail", func(ctx context.Context, cl *agentapi.Client, acfg config.Agent, a artGetArg) (string, error) {
		if err := agentapi.ValidateScope(a.Scope); err != nil {
			return "", fmt.Errorf("fbagent bad-request: %v", err)
		}
		if err := agentapi.ValidateArtifactPath(a.Path); err != nil {
			return "", fmt.Errorf("fbagent bad-request: %v", err)
		}
		q := agentapi.ArtifactQuery{Tail: a.Tail, Date: a.Date, At: a.At, Limit: a.Limit}
		if err := agentapi.ValidateArtifactQuery(a.Scope, a.Path, q); err != nil {
			return "", fmt.Errorf("fbagent bad-request: %v", err)
		}
		raw, err := cl.GetArtifact(ctx, a.Scope, a.Path, q)
		if err != nil {
			return "", err
		}
		return prettyJSON(raw), nil
	})

	type jobStatusArg struct {
		agentArg
		Job      string `json:"job" jsonschema:"agent scheduler job name (e.g. sweep, nbackup, metadata_tracking)"`
		DbFilter string `json:"db_filter,omitempty" jsonschema:"agent-side ?db= filter (database path or job id); backup families only"`
	}
	registerAgentRead(server, gt, "fb_agent_job_status", "read an agent job family's persistent status document (families without one return a note)", func(ctx context.Context, cl *agentapi.Client, acfg config.Agent, a jobStatusArg) (string, error) {
		if a.Job == "" {
			return "", fmt.Errorf("fbagent bad-request: job is required")
		}
		raw, err := cl.GetJobStatus(ctx, a.Job, a.DbFilter)
		if err != nil {
			return "", err
		}
		return prettyJSON(raw), nil
	})

	type opArg struct {
		agentArg
		OperationID string `json:"operation_id" jsonschema:"operation id from a job run / engine op / update apply"`
	}
	registerAgentRead(server, gt, "fb_agent_operation", "poll one fbagent async operation (queued → in_progress → succeeded|failed)", func(ctx context.Context, cl *agentapi.Client, acfg config.Agent, a opArg) (string, error) {
		if a.OperationID == "" {
			return "", fmt.Errorf("fbagent bad-request: operation_id is required")
		}
		op, err := cl.GetOperation(ctx, a.OperationID)
		if err != nil {
			return "", err
		}
		return agentapi.FormatOperation(op), nil
	})

	registerAgentRead(server, gt, "fb_agent_dbaccess", "effective Firebird DatabaseAccess policy inventory (mode, restrict roots, aliases)", func(ctx context.Context, cl *agentapi.Client, acfg config.Agent, a agentArg) (string, error) {
		raw, err := cl.GetDBAccess(ctx, acfg.InstanceIDOrDefault())
		if err != nil {
			return "", err
		}
		return prettyJSON(raw), nil
	})

	type dbCheckArg struct {
		agentArg
		Path string `json:"path" jsonschema:"database file path to evaluate against the policy"`
	}
	registerAgentRead(server, gt, "fb_agent_dbaccess_check", "evaluate one path against the agent's DatabaseAccess policy (frozen CheckPath contract; Tier 0 — pure evaluation)", func(ctx context.Context, cl *agentapi.Client, acfg config.Agent, a dbCheckArg) (string, error) {
		if a.Path == "" {
			return "", fmt.Errorf("fbagent bad-request: path is required")
		}
		raw, err := cl.CheckDBAccess(ctx, acfg.InstanceIDOrDefault(), a.Path)
		if err != nil {
			return "", err
		}
		return prettyJSON(raw), nil
	})

	registerAgentRead(server, gt, "fb_agent_update_status", "firebird-updater runtime state sidecar", func(ctx context.Context, cl *agentapi.Client, acfg config.Agent, a agentArg) (string, error) {
		raw, err := cl.GetUpdateStatus(ctx, acfg.InstanceIDOrDefault())
		if err != nil {
			return "", err
		}
		return prettyJSON(raw), nil
	})

	// --- Tier 1: gated ---

	gt.registerToolEx(server, policy.ToolMeta{Name: "fb_agent_config_reload", Tier: 1, Scope: "instance"},
		"Hot-reload agent_config.json into the running fbagent scheduler (agent of %s). Applied sync steps mirror process start; restart_required_for sections are reported.",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			cl, acfg, err := gt.agentClient(agentStr(args["agent"]))
			if err != nil {
				return "", err
			}
			raw, err := cl.ReloadConfig(ctx)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("agent %q config reload:\n%s", acfg.ID, prettyJSON(raw)), nil
		}, nil)

	gt.registerToolEx(server, policy.ToolMeta{Name: "fb_agent_pprof", Tier: 1, Scope: "instance"},
		"Capture a synchronous CPU/heap/goroutine profile of the fbagent process (agent of %s). The call lasts about cpu_seconds (0-30, default 5; 0 = heap+goroutine only).",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			cl, acfg, err := gt.agentClient(agentStr(args["agent"]))
			if err != nil {
				return "", err
			}
			secs := 5
			if v, ok := agentInt(args["cpu_seconds"]); ok {
				secs = v
			}
			if secs < 0 || secs > 30 {
				return "", fmt.Errorf("cpu_seconds must be 0-30")
			}
			pctx, cancel := context.WithTimeout(ctx, time.Duration(secs+30)*time.Second)
			defer cancel()
			raw, err := cl.Pprof(pctx, secs)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("agent %q profile (cpu_seconds=%d):\n%s", acfg.ID, secs, prettyJSON(raw)), nil
		}, nil)

	// fb_agent_job_run — dynamic tier (ADR-032): typed args + EvaluateMeta.
	mcp.AddTool(server, &mcp.Tool{Name: "fb_agent_job_run", Description: "Tier 1 (dynamic: hot-list families escalate to Tier 2) gated: run one fbagent scheduler job; polls the async operation"}, func(ctx context.Context, req *mcp.CallToolRequest, a agentJobRunArg) (*mcp.CallToolResult, any, error) {
		msg, denied := gt.requestAgentJobRun(ctx, a)
		if denied {
			return errText(msg)
		}
		return text(msg), nil, nil
	})
	gt.execs["fb_agent_job_run"] = gt.runAgentJob

	// --- Tier 2: engine control + update apply (window keyed on db, like
	// fb_service_*) ---

	for _, spec := range []struct{ name, action, impact string }{
		{"fb_agent_restart", "restart", "Restart the Firebird service supervised by fbagent (stop → start → TCP readiness; agent of %s). Tier 2; out-of-band approval."},
		{"fb_agent_stop", "stop", "Stop the Firebird service supervised by fbagent (agent of %s). Tier 2; out-of-band approval."},
		{"fb_agent_start", "start", "Start the Firebird service supervised by fbagent and wait for TCP readiness (agent of %s). Tier 2; out-of-band approval."},
	} {
		gt.registerTool(server, policy.ToolMeta{Name: spec.name, Tier: 2, Scope: "instance"}, spec.impact, gt.agentEngineExec(spec.action))
	}
	gt.registerTool(server, policy.ToolMeta{Name: "fb_agent_update_apply", Tier: 2, Scope: "instance"},
		"Apply the prepared Firebird update via fbagent (agent of %s). The agent auto-enqueues a restart when the engine stays stopped; the nested operation is polled and reported. Tier 2; out-of-band approval.",
		gt.agentUpdateApplyExec())
}
