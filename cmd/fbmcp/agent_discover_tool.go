// Phase 11 D1/D2 — fbagent endpoint discovery and gated registration.
// fb_agent_discover is a Tier-0 LOCAL read (Windows SCM registry or Linux systemd units + files, no HTTP,
// no token access); fb_agent_register persists one agents: entry through
// the standard Tier-2 out-of-band flow (fb_db_register pattern, ADR-031
// rules enforced by config.Validate on the appended document).
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/agentapi"
	"github.com/aleks/fbmcp/internal/agentdiscover"
	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/policy"
)

// registeredAgentFor matches a candidate against the live agents: list by
// normalized base_url.
func registeredAgentFor(cfg *config.Config, baseURL string) string {
	norm := strings.TrimRight(strings.ToLower(baseURL), "/")
	for _, ag := range cfg.Agents {
		if strings.TrimRight(strings.ToLower(ag.BaseURL), "/") == norm {
			return ag.ID
		}
	}
	return ""
}

// suggestAgentID derives a registry id from the Firebird port.
func suggestAgentID(c agentdiscover.Candidate) string {
	if c.FirebirdPort > 0 {
		return fmt.Sprintf("agent_p%d", c.FirebirdPort)
	}
	return "agent_" + config.SuggestedDBID(c.Service)
}

// suggestTokenEnv derives the conventional env var name for the token.
func suggestTokenEnv(c agentdiscover.Candidate) string {
	if c.FirebirdPort > 0 {
		return fmt.Sprintf("FBMCP_AGENT_TOKEN_P%d", c.FirebirdPort)
	}
	return "FBMCP_AGENT_TOKEN"
}

func registerAgentDiscoverTools(server *mcp.Server, gt *gatedTools) {
	// --- fb_agent_discover (Tier 0) ---
	type discoverArg struct {
		Db string `json:"db,omitempty" jsonschema:"registry database id (audit key only)"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_agent_discover", Description: "Tier 0: discover co-located fbagent Local API endpoints from the Windows service registry or Linux systemd units + agent files (no HTTP, no token access); prints the register follow-up per candidate"}, func(ctx context.Context, req *mcp.CallToolRequest, a discoverArg) (*mcp.CallToolResult, any, error) {
		id := identity.Caller(ctx)
		if d := gt.eng.Evaluate(id, a.Db, "fb_agent_discover"); d.Outcome == "deny" {
			gt.aud.Log(audit.Entry{Identity: id.Name, Database: a.Db, Tool: "fb_agent_discover", Tier: 0, Decision: "denied", Detail: map[string]any{"reason": d.Reason}})
			return errText("DENIED: " + d.Reason)
		}
		cands, hostNotes, err := agentdiscover.Discover()
		if err != nil {
			gt.aud.Log(audit.Entry{Identity: id.Name, Database: a.Db, Tool: "fb_agent_discover", Tier: 0, Decision: "error", Detail: map[string]any{"reason": err.Error()}})
			return errText(err.Error())
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d fbagent candidate(s) found on this host:\n", len(cands))
		for _, c := range cands {
			fmt.Fprintf(&b, "\n- service: %s\n  base_url: %s\n  instance_id: %s\n  firebird_port: %d\n  config_path: %s\n", c.Service, c.BaseURL, c.InstanceID, c.FirebirdPort, c.ConfigPath)
			if already := registeredAgentFor(gt.live(), c.BaseURL); already != "" {
				fmt.Fprintf(&b, "  already registered as: %s\n", already)
			} else {
				envName := suggestTokenEnv(c)
				fmt.Fprintf(&b, "  to register:\n")
				fmt.Fprintf(&b, "    1. put the agent's local_api token into env %s (or: fbmcpctl secret set [--dir <secrets.dir>] %s)\n", envName, envName)
				fmt.Fprintf(&b, "    2. fb_agent_register {\"id\":%q,\"base_url\":%q,\"instance_id\":%q,\"token_env\":%q,\"config_path\":%q}\n",
					suggestAgentID(c), c.BaseURL, c.InstanceID, envName, c.ConfigPath)
			}
			for _, n := range c.Notes {
				fmt.Fprintf(&b, "  note: %s\n", n)
			}
		}
		for _, n := range hostNotes {
			fmt.Fprintf(&b, "\nhost note: %s\n", n)
		}
		b.WriteString("\nTokens are never read by discovery: the operator supplies them (env, secrets.dir file or OS keyring).\n")
		gt.aud.Log(audit.Entry{Identity: id.Name, Database: a.Db, Tool: "fb_agent_discover", Tier: 0, Decision: "allow", Detail: map[string]any{"candidates": len(cands)}})
		return text(b.String()), nil, nil
	})

	// --- fb_agent_register (Tier 2, out-of-band) ---
	type registerArg struct {
		Db         string `json:"db,omitempty" jsonschema:"registry database id — maintenance-window key (Tier 2)"`
		ID         string `json:"id" jsonschema:"agent registry id, e.g. agent_p3055"`
		BaseURL    string `json:"base_url" jsonschema:"loopback Local API URL from fb_agent_discover, e.g. http://127.0.0.1:13055"`
		TokenEnv   string `json:"token_env" jsonschema:"env var / keyring name holding the agent's Bearer token (value set by the operator beforehand)"`
		InstanceID string `json:"instance_id,omitempty" jsonschema:"agent instance id from discovery (default fb1)"`
		ConfigPath string `json:"config_path,omitempty" jsonschema:"agent_config.json path from discovery (enables the candidate-file config write path)"`
		Mode       string `json:"mode,omitempty" jsonschema:"preview = dry run"`
	}
	meta := policy.ToolMeta{Name: "fb_agent_register", Tier: 2, Scope: "instance"}
	gt.execs[meta.Name] = func(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
		if gt.live().SourcePath == "" {
			return "", fmt.Errorf("config source path unavailable; cannot persist registration")
		}
		ag, err := config.MaterializeAgent(config.AgentRegisterOptions{
			ID:         agentStr(args["id"]),
			BaseURL:    agentStr(args["base_url"]),
			TokenEnv:   agentStr(args["token_env"]),
			InstanceID: agentStr(args["instance_id"]),
			ConfigPath: agentStr(args["config_path"]),
		})
		if err != nil {
			return "", err
		}
		prog(0.3, "writing fbmcp.yaml")
		if err := config.RegisterAgent(gt.live().SourcePath, ag); err != nil {
			return "", err
		}
		out := fmt.Sprintf("registered agent %s (%s)", ag.ID, ag.BaseURL)
		if gt.reloader != nil {
			res, rerr := gt.reloader.Apply("fb_agent_register", liveJobID(gt, dbID, "fb_agent_register"))
			if rerr != nil {
				return "", fmt.Errorf("registered on disk but live reload failed: %w", rerr)
			}
			out += fmt.Sprintf("; live config updated (%s)", res.Class)
		}
		// Post-apply identity verification (configeditor's verifyAgentIdentity
		// pattern): best-effort — an unset token is reported, not fatal.
		prog(0.8, "verifying agent identity")
		out += "\n" + verifyRegisteredAgent(ctx, gt, ag)
		prog(1.0, "done")
		return out, nil
	}
	mcp.AddTool(server, &mcp.Tool{Name: meta.Name, Description: "Tier 2 (gated): persist one discovered fbagent into fbmcp.yaml agents: (out-of-band approval); verifies the agent identity after apply"}, func(ctx context.Context, req *mcp.CallToolRequest, a registerArg) (*mcp.CallToolResult, any, error) {
		impact := fmt.Sprintf("Persist fbagent %q into fbmcp.yaml agents: list.\nbase_url: %s\ninstance_id: %s\ntoken_env: %s\nconfig_path: %s\nThis grants the kernel a new credentialed local target (ADR-031 rules enforced).",
			a.ID, a.BaseURL, a.InstanceID, a.TokenEnv, a.ConfigPath)
		argsMap := map[string]any{"id": a.ID, "base_url": a.BaseURL, "token_env": a.TokenEnv, "instance_id": a.InstanceID, "config_path": a.ConfigPath}
		msg, denied := gt.requestGatedMeta(ctx, meta, impact, a.Db, argsMap, a.Mode)
		if denied {
			return errText(msg)
		}
		return text(msg), nil, nil
	})
}

// verifyRegisteredAgent checks GET /v1/instances against the registered
// instance_id (case-insensitive), mirroring HQ Monitor's verifyAgentIdentity.
func verifyRegisteredAgent(ctx context.Context, gt *gatedTools, ag config.Agent) string {
	tok, err := config.SecretFromEnv(ag.TokenEnv)
	if err != nil {
		return fmt.Sprintf("identity check skipped: token %s not set yet (%v) — set it, then run fb_agent_instances", ag.TokenEnv, err)
	}
	cl, err := agentapi.New(ag.BaseURL, tok)
	if err != nil {
		return "identity check skipped: " + err.Error()
	}
	raw, err := cl.GetInstances(ctx, "")
	if err != nil {
		return fmt.Sprintf("identity check FAILED (unreachable or denied): %v", err)
	}
	want := strings.ToLower(ag.InstanceIDOrDefault())
	if strings.Contains(strings.ToLower(string(raw)), `"`+want+`"`) {
		return fmt.Sprintf("identity check OK: agent reports instance %q", ag.InstanceIDOrDefault())
	}
	return fmt.Sprintf("identity check WARNING: instance %q not found in the agent's /v1/instances answer — a different agent may listen on %s (other-agent); fix instance_id before use", ag.InstanceIDOrDefault(), ag.BaseURL)
}
