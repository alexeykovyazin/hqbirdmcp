// Phase 11 D6 — fb_agent_restart/stop/start hardened to the HQbirdRCM
// contract (hqcluster-node internal/fbctl/controller.go):
//
//	preflight GET /v1/instances/{id}: identity match, refuse state "unknown"
//	→ POST {requested_by, reason, coalesce:true, timeout_sec}
//	→ poll the operation; failed + error.work_continues → poll the instance,
//	  never re-issue; failed with Windows 1056 → one retry after 2s
//	→ post-check: state running (stopped for stop) + TCP probe on the port.
//
// Two RCM gaps closed here: timeout_sec is sent (agent and fbmcp agree on
// the budget) and work_continues is honoured. When hqclusternode manages the
// host, engine control belongs to the node (restart window, pending_restart);
// the call is refused unless args.cluster_override=true.
package main

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/aleks/fbmcp/internal/agentapi"
	"github.com/aleks/fbmcp/internal/agentdiscover"
	"github.com/aleks/fbmcp/internal/config"
)

// Test seams.
var (
	clusterNodePresent = agentdiscover.HasClusterNode
	engineRetryDelay   = 2 * time.Second
	enginePollInterval = 500 * time.Millisecond
	tcpProbe           = func(ctx context.Context, port int) error {
		d := net.Dialer{Timeout: 2 * time.Second}
		for {
			c, err := d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			if err == nil {
				c.Close()
				return nil
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("port %d never accepted a connection: %w", port, err)
			case <-time.After(time.Second):
			}
		}
	}
)

// enginePreflight checks the agent answers for the configured instance and
// that its state allows engine control.
func enginePreflight(ctx context.Context, cl *agentapi.Client, acfg config.Agent) (agentapi.Instance, error) {
	id := acfg.InstanceIDOrDefault()
	inst, err := cl.GetInstance(ctx, id)
	if err != nil {
		return inst, fmt.Errorf("fbagent preflight: %w (fix the agents: entry instance_id/token; never fall back to another loopback port)", err)
	}
	if !strings.EqualFold(inst.InstanceID, id) {
		return inst, fmt.Errorf("fbagent preflight: agent reports instance_id %q, configured %q (wrong agent on %s?)", inst.InstanceID, id, acfg.BaseURL)
	}
	if inst.State == "unknown" || inst.State == "" {
		return inst, fmt.Errorf("fbagent preflight: instance %s state is %q; investigate (fb_agent_status, fb_service_status) before engine control", id, inst.State)
	}
	return inst, nil
}

func clusterRefusal(args map[string]any) string {
	if !clusterNodePresent() {
		return ""
	}
	if b, _ := args["cluster_override"].(bool); b {
		return ""
	}
	return "hqclusternode manages this host: engine restarts must go through the cluster node (restart window, pending_restart, replication.conf apply). Pass args.cluster_override=true only when the node is stopped or you accept bypassing it"
}

// agentEngineExec builds the executor for restart/stop/start (ADR-033 async
// polling, RCM contract above).
func (gt *gatedTools) agentEngineExec(action string) executor {
	return func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
		if msg := clusterRefusal(args); msg != "" {
			return "", fmt.Errorf("%s", msg)
		}
		cl, acfg, err := gt.agentClient(agentStr(args["agent"]))
		if err != nil {
			return "", err
		}
		budget := time.Duration(acfg.TimeoutOrDefault()) * time.Second
		wctx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()

		inst, err := enginePreflight(wctx, cl, acfg)
		if err != nil {
			return "", err
		}
		prog(0.05, fmt.Sprintf("preflight ok: %s %s (port %d, state %s)", inst.InstanceID, inst.ServiceName, inst.Port, inst.State))

		requestedBy := agentStr(args["_grant_identity"])
		if requestedBy == "" {
			requestedBy = "fbmcp"
		}
		coalesce := true
		if b, ok := args["coalesce"].(bool); ok {
			coalesce = b
		}
		// The agent's budget must end before ours so its verdict reaches us.
		agentSecs := acfg.TimeoutOrDefault() - 10
		if agentSecs < 30 {
			agentSecs = 30
		}
		req := agentapi.EngineOpRequest{RequestedBy: requestedBy, Reason: agentStr(args["reason"]), Coalesce: &coalesce, TimeoutSec: agentSecs}

		var op agentapi.Operation
		var notes []string
		for attempt := 1; attempt <= 2; attempt++ {
			resp, err := cl.EngineOp(wctx, acfg.InstanceIDOrDefault(), action, req)
			if err != nil {
				return "", err
			}
			prog(0.1, fmt.Sprintf("operation %s accepted (attempt %d)", resp.OperationID, attempt))
			op, err = cl.PollOperation(wctx, resp.OperationID, enginePollInterval, prog)
			if err != nil {
				return "", err
			}
			if op.State != "failed" {
				break
			}
			if op.Error != nil && op.Error.WorkContinues {
				// The work is still on its way: wait for the instance, never
				// issue a second operation that would fight the first.
				notes = append(notes, "agent deadline passed with work_continues=true; waited for the instance instead of re-issuing")
				if err := waitInstanceState(wctx, cl, acfg, wantState(action)); err != nil {
					return "", fmt.Errorf("agent %s: %s\n%w (do not re-issue; poll fb_agent_status)", action, agentapi.FormatOperation(op), err)
				}
				op.State = "succeeded (after work_continues)"
				break
			}
			if attempt == 1 && action != "stop" && op.Error != nil && strings.Contains(op.Error.Message, "1056") {
				notes = append(notes, "Windows 1056 (start raced the stop): retried once")
				select {
				case <-wctx.Done():
					return "", wctx.Err()
				case <-time.After(engineRetryDelay):
				}
				continue
			}
			return "", fmt.Errorf("agent %s failed: %s", action, agentapi.FormatOperation(op))
		}
		if strings.HasPrefix(op.State, "failed") {
			return "", fmt.Errorf("agent %s failed after retry: %s", action, agentapi.FormatOperation(op))
		}

		// Post-check: the instance state is the agent's own verdict; the TCP
		// probe proves the listener is really up before callers use it.
		prog(0.8, "post-check")
		post, err := cl.GetInstance(wctx, acfg.InstanceIDOrDefault())
		if err != nil {
			return "", fmt.Errorf("post-%s get instance: %w", action, err)
		}
		if post.State != wantState(action) {
			return "", fmt.Errorf("post-%s: instance state %q, want %q", action, post.State, wantState(action))
		}
		port := post.Port
		if port < 1 {
			port = inst.Port
		}
		probe := "skipped (stop)"
		if action != "stop" && port > 0 {
			if err := tcpProbe(wctx, port); err != nil {
				return "", fmt.Errorf("post-%s: %w", action, err)
			}
			probe = fmt.Sprintf("127.0.0.1:%d accepts connections", port)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "agent %s via agent %q:\n%s\npost-check: state %s; tcp %s", action, acfg.ID, agentapi.FormatOperation(op), post.State, probe)
		for _, n := range notes {
			fmt.Fprintf(&b, "\nnote: %s", n)
		}
		return b.String(), nil
	}
}

func wantState(action string) string {
	if action == "stop" {
		return "stopped"
	}
	return "running"
}

func waitInstanceState(ctx context.Context, cl *agentapi.Client, acfg config.Agent, want string) error {
	for {
		inst, err := cl.GetInstance(ctx, acfg.InstanceIDOrDefault())
		if err == nil && inst.State == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("instance did not reach %q within the budget (last state %q)", want, inst.State)
		case <-time.After(enginePollInterval):
		}
	}
}

// enginePreview adds the live preflight to the impact statement.
func (gt *gatedTools) enginePreview(ctx context.Context, dbID string, args map[string]any) string {
	var b strings.Builder
	if msg := clusterRefusal(args); msg != "" {
		fmt.Fprintf(&b, "WILL BE REFUSED: %s\n", msg)
	}
	cl, acfg, err := gt.agentClient(agentStr(args["agent"]))
	if err != nil {
		fmt.Fprintf(&b, "preflight: %v", err)
		return b.String()
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	inst, err := enginePreflight(pctx, cl, acfg)
	if err != nil {
		fmt.Fprintf(&b, "preflight FAILS now: %v", err)
		return b.String()
	}
	fmt.Fprintf(&b, "preflight: instance %s, service %s, port %d, state %s", inst.InstanceID, inst.ServiceName, inst.Port, inst.State)
	return b.String()
}
