// fbagent DBA operations (/v1/dba/..., fbagent DBA_ASSISTANT_PLAN.ru.md §3
// and §9): one-time DBA actions on the agent's host — shutdown and online,
// copy, validation and mend, gstat, sweep, ICU fix, properties, trace
// sessions, limbo, stop of a tool. Every action has a check that changes
// nothing and shows the exact commands (passwords masked). The database is
// named by its full path as the agent knows it.
//
//   - Tier 0: fb_agent_tools_running, fb_agent_dba_ops,
//     fb_agent_dba_database, fb_agent_dba_runs, fb_agent_dba_check,
//     fb_agent_dba_output.
//   - Tier 1: fb_agent_dba_cancel.
//   - fb_agent_dba_run: the tier comes from the class the check returns
//     (read/normal 1, disruptive 2, destructive 3, critical refused: the
//     Firebird service goes through fb_agent_stop/start/restart).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/agentapi"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/policy"
)

const (
	agentDBARunTool    = "fb_agent_dba_run"
	agentDBACancelTool = "fb_agent_dba_cancel"

	// dbaOutputCap is the most output text a tool returns to the model.
	dbaOutputCap = 64 << 10
	// dbaDetailsCap is the most of a check's details shown in a reply.
	dbaDetailsCap = 4 << 10
	// dbaRunTailLines is how much output a finished run shows.
	dbaRunTailLines = 20
)

// The short wait of fb_agent_dba_run after the start: a fast operation
// reports its end, a long one its run id. Variables for the tests.
var (
	dbaQuickWait = 10 * time.Second
	dbaPollEvery = time.Second
)

const dbaAbout = "One-time DBA action on the host of the fbagent. Every action has a check that changes nothing and shows the exact commands. The database is named by its full path as the agent knows it (fb_agent_databases, fb_agent_dba_database)."

// dbaClassTier maps the class a check returned to the gate tier. A critical
// operation (Firebird stop/start/restart) is refused: msg says why.
func dbaClassTier(class string) (tier int, msg string) {
	switch class {
	case agentapi.DBAClassRead, agentapi.DBAClassNormal:
		return 1, ""
	case agentapi.DBAClassDisruptive:
		return 2, ""
	case agentapi.DBAClassDestructive:
		return 3, ""
	case agentapi.DBAClassCritical:
		return 0, "this operation stops or starts the Firebird server (class critical). fb_agent_dba_run does not run it: use fb_agent_stop, fb_agent_start or fb_agent_restart (Tier 2)."
	}
	return 0, fmt.Sprintf("the agent returned an unknown class %q: nothing was run", class)
}

// formatDBAChecks writes the check lines, one per line.
func formatDBAChecks(b *strings.Builder, items []agentapi.DBACheckItem) {
	if len(items) == 0 {
		b.WriteString("checks: none\n")
		return
	}
	b.WriteString("checks:\n")
	for _, it := range items {
		fmt.Fprintf(b, "  [%s] %s\n", it.Level, it.Text)
	}
}

// formatDBACommands writes the commands, with the outcome of a run step.
func formatDBACommands(b *strings.Builder, cmds []agentapi.DBACommand) {
	if len(cmds) == 0 {
		return
	}
	b.WriteString("commands (exact, passwords masked):\n")
	for i, c := range cmds {
		fmt.Fprintf(b, "  %d. [%s] %s", i+1, c.Phase, c.Command)
		switch {
		case c.Error != "":
			fmt.Fprintf(b, "  -> failed: %s", c.Error)
		case c.OK != nil && *c.OK:
			b.WriteString("  -> ok")
		case c.OK != nil:
			b.WriteString("  -> failed")
		}
		b.WriteString("\n")
	}
}

func dbaTarget(db string) string {
	if db == "" {
		return "the server"
	}
	return db
}

func compactJSON(raw json.RawMessage, max int) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if len(s) > max {
		s = s[:max] + " … (cut)"
	}
	return s
}

// formatDBACheck is the check as the model and the approver see it. The
// confirm token is never shown.
func formatDBACheck(title string, c agentapi.DBACheck) string {
	var b strings.Builder
	name := c.Op
	if title != "" {
		name = fmt.Sprintf("%s (%s)", title, c.Op)
	}
	fmt.Fprintf(&b, "operation: %s, class %s\n", name, c.Class)
	fmt.Fprintf(&b, "database: %s\n", dbaTarget(c.Database))
	formatDBAChecks(&b, c.Checks)
	formatDBACommands(&b, c.Commands)
	if d := compactJSON(c.Details, dbaDetailsCap); d != "" {
		fmt.Fprintf(&b, "details: %s\n", d)
	}
	if c.ConfirmName != "" {
		fmt.Fprintf(&b, "the run must name the database file: %s\n", c.ConfirmName)
	}
	if c.Blocked {
		b.WriteString("BLOCKED: a check is at the block level; the agent will not run it.\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatDBARefusal(r *agentapi.DBARefusal) string {
	var b strings.Builder
	fmt.Fprintf(&b, "the agent refused: %s", r.Message)
	if r.Reason != "" {
		fmt.Fprintf(&b, " (reason %s)", r.Reason)
	}
	b.WriteString("\n")
	if len(r.Checks) > 0 {
		formatDBAChecks(&b, r.Checks)
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatDBARun is one run in short form.
func formatDBARun(r agentapi.DBARun) string {
	var b strings.Builder
	fmt.Fprintf(&b, "run %s: %s on %s, state %s", r.RunID, r.Op, dbaTarget(r.Database), r.State)
	if r.StateReason != "" {
		fmt.Fprintf(&b, " (%s)", r.StateReason)
	}
	b.WriteString("\n")
	if r.StateMessage != "" {
		fmt.Fprintf(&b, "message: %s\n", r.StateMessage)
	}
	if !r.Final() && r.StepCount > 0 {
		fmt.Fprintf(&b, "step %d of %d: %s\n", r.StepIndex, r.StepCount, r.Step)
	}
	formatDBACommands(&b, r.Commands)
	if len(r.Warnings) > 0 {
		fmt.Fprintf(&b, "warnings: %s\n", strings.Join(r.Warnings, "; "))
	}
	if res := compactJSON(r.Result, dbaDetailsCap); res != "" {
		fmt.Fprintf(&b, "result: %s\n", res)
	}
	return strings.TrimRight(b.String(), "\n")
}

// capDBAOutput keeps at most max bytes for the model: the start of a view,
// the end of a tail. cut reports a cut.
func capDBAOutput(text string, fromEnd bool, max int) (string, bool) {
	if len(text) <= max {
		return text, false
	}
	if fromEnd {
		s := text[len(text)-max:]
		if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
			s = s[i+1:]
		}
		return s, true
	}
	s := text[:max]
	if i := strings.LastIndexByte(s, '\n'); i > 0 {
		s = s[:i+1]
	}
	return s, true
}

// --- fb_agent_dba_run ---

// agentDBARunArg is the fb_agent_dba_run argument schema.
type agentDBARunArg struct {
	Agent    string         `json:"agent,omitempty" jsonschema:"agent registry id (optional when exactly one agent is configured)"`
	Db       string         `json:"db,omitempty" jsonschema:"registry database id: the audit key; a disruptive operation (Tier 2) needs it as the maintenance window key"`
	Op       string         `json:"op" jsonschema:"operation id from fb_agent_dba_ops, e.g. db_validate_online, db_gstat, db_sweep, db_shutdown, db_online, db_copy"`
	Database string         `json:"database,omitempty" jsonschema:"full path of the database as the agent knows it; empty for an operation of the server scope"`
	Params   map[string]any `json:"params,omitempty" jsonschema:"parameters of the operation (names and types from fb_agent_dba_ops); absent ones take their defaults"`
	Reason   string         `json:"reason,omitempty" jsonschema:"recorded by the agent"`
	Mode     string         `json:"mode,omitempty" jsonschema:"preview = dry run"`
}

// requestAgentDBARun is the pre-gate body of fb_agent_dba_run: the check,
// the tier from its class, the impact text, then the gate. Returns (text,
// denied).
func (gt *gatedTools) requestAgentDBARun(ctx context.Context, a agentDBARunArg) (string, bool) {
	if strings.TrimSpace(a.Op) == "" {
		return "fbagent bad-request: op is required (operation ids: fb_agent_dba_ops)", true
	}
	cl, acfg, err := gt.agentClient(a.Agent)
	if err != nil {
		return err.Error(), true
	}
	req := agentapi.DBARequest{Database: a.Database, Params: a.Params, RequestedBy: identity.Caller(ctx).Name, Reason: a.Reason}
	chk, err := cl.DBACheck(ctx, a.Op, req)
	if err != nil {
		var r *agentapi.DBARefusal
		if errors.As(err, &r) {
			return formatDBARefusal(r) + "\nNothing was run.", true
		}
		return err.Error(), true
	}
	title, _ := cl.DBAOpTitle(ctx, a.Op) // only for the text
	checkText := formatDBACheck(title, chk)
	if chk.Blocked {
		return checkText + "\nNothing was run. Fix what the block lines say, then call fb_agent_dba_run again.", true
	}
	tier, msg := dbaClassTier(chk.Class)
	if msg != "" {
		return checkText + "\nREFUSED: " + msg, true
	}
	impact := fmt.Sprintf("Run a one-time DBA operation through fbagent %q.\n%s\nThe agent checks again before it starts. requested_by records the confirming identity.\nDynamic tier: %d (class %s).",
		acfg.ID, checkText, tier, chk.Class)
	argsMap := map[string]any{
		"agent": a.Agent, "op": a.Op, "database": a.Database, "params": a.Params, "reason": a.Reason,
		"class": chk.Class, "confirm_token": chk.ConfirmToken, "confirm_name": chk.ConfirmName,
	}
	reply, denied := gt.requestGatedMeta(ctx, policy.ToolMeta{Name: agentDBARunTool, Tier: tier, Scope: "instance"}, impact, a.Db, argsMap, a.Mode)
	if denied {
		reply = strings.TrimRight(reply, "\n") + "\n\nThe check of the agent:\n" + checkText
		if tier == 3 {
			reply += "\nA destructive operation is Tier 3 in fbmcp: run it from the Tools page of the HQbird console."
		}
	}
	return reply, denied
}

// runAgentDBA is the fb_agent_dba_run executor: run with the token of the
// check; on a refusal one new check and one more run; then a short wait.
func (gt *gatedTools) runAgentDBA(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
	if prog == nil {
		prog = func(float64, string) {}
	}
	cl, acfg, err := gt.agentClient(agentStr(args["agent"]))
	if err != nil {
		return "", err
	}
	op := agentStr(args["op"])
	req := agentapi.DBARequest{
		Database:     agentStr(args["database"]),
		Params:       agentParams(args["params"]),
		RequestedBy:  agentStr(args["_grant_identity"]),
		Reason:       agentStr(args["reason"]),
		RequestID:    agentStr(args["_grant_request_id"]),
		ConfirmToken: agentStr(args["confirm_token"]),
		ConfirmName:  agentStr(args["confirm_name"]),
	}
	start, err := cl.DBARun(ctx, op, req)
	var refused *agentapi.DBARefusal
	if errors.As(err, &refused) {
		// The token lives 5 minutes and the approval may come later, or the
		// state changed: check again once.
		prog(0.1, "the agent refused the run ("+refused.Reason+"): checking again")
		chk, cerr := cl.DBACheck(ctx, op, agentapi.DBARequest{Database: req.Database, Params: req.Params, RequestedBy: req.RequestedBy, Reason: req.Reason})
		switch {
		case cerr != nil:
			return "", fmt.Errorf("%s\nthe new check failed: %v", formatDBARefusal(refused), cerr)
		case chk.Blocked:
			return "", fmt.Errorf("%s\nthe new check blocks it; nothing was run:\n%s", formatDBARefusal(refused), formatDBACheck("", chk))
		case chk.Class != agentStr(args["class"]):
			return "", fmt.Errorf("%s\nthe class changed from %s to %s; nothing was run. Request the action again", formatDBARefusal(refused), agentStr(args["class"]), chk.Class)
		}
		req.ConfirmToken, req.ConfirmName = chk.ConfirmToken, chk.ConfirmName
		start, err = cl.DBARun(ctx, op, req)
		if errors.As(err, &refused) {
			return "", fmt.Errorf("%s\nnothing was run", formatDBARefusal(refused))
		}
	}
	if err != nil {
		return "", err
	}
	prog(0.2, "run "+start.RunID+" started")
	run := start.Run
	if run.RunID == "" {
		run.RunID = start.RunID
	}
	run = gt.waitDBARun(ctx, cl, run)
	var b strings.Builder
	fmt.Fprintf(&b, "agent %q: %s", acfg.ID, formatDBARun(run))
	if !run.Final() {
		fmt.Fprintf(&b, "\nStill running. Follow it with fb_agent_dba_runs run_id=%q and fb_agent_dba_output run_id=%q; stop it with fb_agent_dba_cancel.", run.RunID, run.RunID)
		return b.String(), nil
	}
	if out, err := cl.GetDBAOutput(ctx, run.RunID, dbaRunTailLines); err == nil && strings.TrimSpace(out.Text) != "" {
		tail, _ := capDBAOutput(out.Text, true, dbaDetailsCap)
		fmt.Fprintf(&b, "\nlast lines of the output:\n%s", strings.TrimRight(tail, "\n"))
	}
	fmt.Fprintf(&b, "\nThe whole output: fb_agent_dba_output run_id=%q.", run.RunID)
	if run.State == agentapi.DBAStateFailed {
		return "", errors.New(b.String())
	}
	return b.String(), nil
}

// waitDBARun polls a run for a short time. It returns the last state it saw.
func (gt *gatedTools) waitDBARun(ctx context.Context, cl *agentapi.Client, run agentapi.DBARun) agentapi.DBARun {
	if run.Final() {
		return run
	}
	wctx, cancel := context.WithTimeout(ctx, dbaQuickWait)
	defer cancel()
	tick := time.NewTicker(dbaPollEvery)
	defer tick.Stop()
	for {
		select {
		case <-wctx.Done():
			return run
		case <-tick.C:
		}
		cur, _, err := cl.GetDBARun(wctx, run.RunID)
		if err != nil {
			return run
		}
		run = cur
		if run.Final() {
			return run
		}
	}
}

// --- registration ---

func registerAgentDBA(server *mcp.Server, gt *gatedTools) {
	registerAgentRead(server, gt, "fb_agent_tools_running", "Firebird tools (gbak, nbackup, gstat, gfix, isql...) that run on the host of the fbagent now, from the process table (GET /v1/dba/tools; no attach). Stop one with fb_agent_dba_run op=tool_stop or op=gbak_stop.",
		func(ctx context.Context, cl *agentapi.Client, acfg config.Agent, a agentArg) (string, error) {
			raw, err := cl.DBATools(ctx)
			if err != nil {
				return "", err
			}
			return prettyJSON(raw), nil
		})

	type opsArg struct {
		agentArg
		Database string `json:"database,omitempty" jsonschema:"full path of a database as the agent knows it: each operation then carries its exact commands with the default parameters"`
	}
	registerAgentRead(server, gt, "fb_agent_dba_ops", dbaAbout+" The catalog of the operations (GET /v1/dba/ops): id, class (read, normal, disruptive, destructive, critical), scope, title, help and parameters; with database, the default commands of each.",
		func(ctx context.Context, cl *agentapi.Client, acfg config.Agent, a opsArg) (string, error) {
			raw, err := cl.DBAOps(ctx, strings.TrimSpace(a.Database))
			if err != nil {
				return "", err
			}
			return prettyJSON(raw), nil
		})

	type databaseArg struct {
		agentArg
		Database string `json:"database" jsonschema:"full path of the database as the agent knows it"`
		Tables   bool   `json:"tables,omitempty" jsonschema:"also list the user tables (the agent attaches to the database)"`
	}
	registerAgentRead(server, gt, "fb_agent_dba_database", "state of one database for the DBA operations (GET /v1/dba/database; from the header page, the files and the agent's data, no attach): exists, size, shutdown mode, nbackup state, read only, forced writes, transaction counters, connections, last copy and validation. tables=true adds the user tables (POST /v1/dba/tables, attaches).",
		func(ctx context.Context, cl *agentapi.Client, acfg config.Agent, a databaseArg) (string, error) {
			db := strings.TrimSpace(a.Database)
			if db == "" {
				return "", fmt.Errorf("fbagent bad-request: database is required (the full path as the agent knows it)")
			}
			raw, err := cl.DBADatabase(ctx, db)
			if err != nil {
				return "", err
			}
			out := prettyJSON(raw)
			if a.Tables {
				tabs, err := cl.DBATables(ctx, db)
				if err != nil {
					return out + "\n\ntables: " + err.Error(), nil
				}
				out += "\n\ntables:\n" + prettyJSON(tabs)
			}
			return out, nil
		})

	type dbaRunsArg struct {
		agentArg
		RunID    string `json:"run_id,omitempty" jsonschema:"one run (takes precedence over the filters)"`
		Op       string `json:"op,omitempty" jsonschema:"filter: operation id"`
		Database string `json:"database,omitempty" jsonschema:"filter: full path of the database"`
		Active   bool   `json:"active,omitempty" jsonschema:"only the runs that still run"`
		Limit    int    `json:"limit,omitempty" jsonschema:"max runs returned, newest first"`
	}
	registerAgentRead(server, gt, "fb_agent_dba_runs", "runs of the DBA operations (GET /v1/dba/runs): one run (run_id) with its state, step, commands and their outcome, or a filtered list, newest first. The output text: fb_agent_dba_output.",
		func(ctx context.Context, cl *agentapi.Client, acfg config.Agent, a dbaRunsArg) (string, error) {
			if a.RunID != "" {
				_, raw, err := cl.GetDBARun(ctx, a.RunID)
				if err != nil {
					return "", err
				}
				return prettyJSON(raw), nil
			}
			raw, err := cl.GetDBARuns(ctx, agentapi.DBARunFilter{Active: a.Active, Op: a.Op, Database: a.Database, Limit: a.Limit})
			if err != nil {
				return "", err
			}
			return prettyJSON(raw), nil
		})

	type checkArg struct {
		agentArg
		Op       string         `json:"op" jsonschema:"operation id from fb_agent_dba_ops"`
		Database string         `json:"database,omitempty" jsonschema:"full path of the database as the agent knows it; empty for an operation of the server scope"`
		Params   map[string]any `json:"params,omitempty" jsonschema:"parameters of the operation; absent ones take their defaults"`
	}
	registerAgentRead(server, gt, "fb_agent_dba_check", dbaAbout+" The check of one operation (POST /v1/dba/ops/{op}/check; changes nothing, may attach): the check lines (ok, warn, block), the class and the exact commands. Run it with fb_agent_dba_run.",
		func(ctx context.Context, cl *agentapi.Client, acfg config.Agent, a checkArg) (string, error) {
			if strings.TrimSpace(a.Op) == "" {
				return "", fmt.Errorf("fbagent bad-request: op is required (operation ids: fb_agent_dba_ops)")
			}
			chk, err := cl.DBACheck(ctx, a.Op, agentapi.DBARequest{Database: a.Database, Params: a.Params, RequestedBy: identity.Caller(ctx).Name})
			if err != nil {
				var r *agentapi.DBARefusal
				if errors.As(err, &r) {
					return "", fmt.Errorf("%s\n%s", err.Error(), formatDBARefusal(r))
				}
				return "", err
			}
			title, _ := cl.DBAOpTitle(ctx, a.Op)
			out := formatDBACheck(title, chk)
			tier, msg := dbaClassTier(chk.Class)
			switch {
			case chk.Blocked:
				out += "\nNext: fix what the block lines say; fb_agent_dba_run will refuse until then."
			case msg != "":
				out += "\nNext: " + msg
			default:
				out += fmt.Sprintf("\nNext: fb_agent_dba_run op=%q with the same database and params (Tier %d).", chk.Op, tier)
			}
			return out, nil
		})

	type outputArg struct {
		agentArg
		RunID string `json:"run_id" jsonschema:"run id from fb_agent_dba_run or fb_agent_dba_runs"`
		Tail  int    `json:"tail,omitempty" jsonschema:"only the last N lines"`
	}
	registerAgentRead(server, gt, "fb_agent_dba_output", fmt.Sprintf("what the tools of a DBA run printed (GET /v1/dba/runs/{id}/output): the start of it, or with tail the last N lines. At most %d KB is returned; the reply says when it was cut.", dbaOutputCap>>10),
		func(ctx context.Context, cl *agentapi.Client, acfg config.Agent, a outputArg) (string, error) {
			if a.RunID == "" {
				return "", fmt.Errorf("fbagent bad-request: run_id is required")
			}
			if a.Tail < 0 {
				return "", fmt.Errorf("fbagent bad-request: tail must be >= 0")
			}
			out, err := cl.GetDBAOutput(ctx, a.RunID, a.Tail)
			if err != nil {
				return "", err
			}
			text, cut := capDBAOutput(out.Text, a.Tail > 0, dbaOutputCap)
			if text == "" {
				text = "(no output yet)\n"
			}
			var notes []string
			if out.Bytes >= 0 {
				notes = append(notes, fmt.Sprintf("output file: %d bytes", out.Bytes))
			}
			if cut || out.Truncated {
				if a.Tail > 0 {
					notes = append(notes, fmt.Sprintf("cut: only the last %d KB are shown", dbaOutputCap>>10))
				} else {
					notes = append(notes, fmt.Sprintf("cut: only the start is shown; use tail=N for the end (the whole file is on the agent: GET /v1/dba/runs/%s/output?download=1)", a.RunID))
				}
			}
			if len(notes) > 0 {
				text = strings.TrimRight(text, "\n") + "\n\n[" + strings.Join(notes, "; ") + "]"
			}
			return text, nil
		})

	// fb_agent_dba_cancel — Tier 1: DELETE a running DBA run.
	gt.registerToolEx(server, policy.ToolMeta{Name: agentDBACancelTool, Tier: 1, Scope: "instance"},
		"Cancel a running fbagent DBA run (agent of %s). Steps marked always (an unlock, a return to online) still run; an operation that cannot be stopped, or a run that ended, answers 409 conflict.",
		func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
			cl, acfg, err := gt.agentClient(agentStr(args["agent"]))
			if err != nil {
				return "", err
			}
			runID := agentStr(args["run_id"])
			if runID == "" {
				return "", fmt.Errorf("run_id is required")
			}
			raw, err := cl.CancelDBARun(ctx, runID)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("agent %q DBA run %s cancel:\n%s", acfg.ID, runID, prettyJSON(raw)), nil
		}, nil)

	// fb_agent_dba_run — dynamic tier from the class of the check.
	mcp.AddTool(server, &mcp.Tool{Name: agentDBARunTool, Description: dbaAbout + " Runs one operation: fbmcp calls the check first and shows its lines and exact commands to the approver; a blocked check runs nothing. The tier comes from the class of the check: read/normal Tier 1 (fb_confirm), disruptive Tier 2 (maintenance window on db, operator approves), destructive Tier 3 (disabled), critical refused (use fb_agent_stop/start/restart). After the start: fb_agent_dba_runs and fb_agent_dba_output."},
		func(ctx context.Context, req *mcp.CallToolRequest, a agentDBARunArg) (*mcp.CallToolResult, any, error) {
			msg, denied := gt.requestAgentDBARun(ctx, a)
			if denied {
				return errText(msg)
			}
			return text(msg), nil, nil
		})
	gt.execs[agentDBARunTool] = gt.runAgentDBA
}
