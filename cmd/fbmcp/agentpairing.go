package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/agentapi"
	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/policy"
)

// fb_agent_cluster_pairing: the pairing window of the host's fbagent
// (GET/POST /v1/cluster/pairing, fbagent with capability cluster_pairing).
// While the window is open and the host is free, the agent listens on 7450
// and an RCM may invite the host into its cluster (fb_cluster_probe,
// fb_cluster_link). The setup opens the window for 24 h; after that this
// tool opens it again, as `sudo -u firebird fbagent --cluster-pairing on`
// on the host.
//
// One tool, two tiers: action=status reads at Tier 0 (policy + audit, no
// gate); on and off change the window at Tier 1 (fb_confirm). The registry
// lists the tool at Tier 1, so tools/list shows it to identities that may
// change the window.

const agentPairingTool = "fb_agent_cluster_pairing"

const agentPairingImpact = "Open or close the cluster pairing window of the host's fbagent (port 7450; an RCM on the network may then invite this host into its cluster; audit key: db %s)."

func registerAgentClusterPairing(server *mcp.Server, gt *gatedTools) {
	gt.execs[agentPairingTool] = gt.agentPairingExec
	type pairingArg struct {
		Db   string         `json:"db,omitempty" jsonschema:"registry database id: the audit key (optional)"`
		Mode string         `json:"mode,omitempty" jsonschema:"preview = dry run"`
		Args map[string]any `json:"args,omitempty"`
	}
	desc := "Cluster pairing window of the host's fbagent (port 7450, where an RCM invites the host into a cluster), as fbagent --cluster-pairing on|off|status. " +
		"action=status reads it: Tier 0, no confirmation. action=on opens it for hours (1-24, default 24), action=off closes it: Tier 1 (fb_confirm). " +
		gatedArgSpecs[agentPairingTool].Note + " args: " + argsSummary(gatedArgSpecs[agentPairingTool])
	mcp.AddTool(server, &mcp.Tool{Name: agentPairingTool, Description: desc}, func(ctx context.Context, req *mcp.CallToolRequest, a pairingArg) (*mcp.CallToolResult, any, error) {
		action := strings.ToLower(strings.TrimSpace(agentStr(a.Args["action"])))
		if action == "" || action == "status" {
			return gt.agentPairingRead(ctx, a.Db, a.Args)
		}
		if msg := agentPairingArgsError(action, a.Args); msg != "" {
			return errText("DENIED: " + msg)
		}
		meta := policy.ToolMeta{Name: agentPairingTool, Tier: 1, Scope: "instance"}
		msg, isErr := gt.requestGated(ctx, meta, agentPairingImpact, a.Db, a.Args, a.Mode, gt.agentPairingPreview)
		if isErr {
			return errText(msg)
		}
		return text(msg), nil, nil
	})
}

// agentPairingArgsError checks what the args spec cannot: hours and
// rcm_allow go with on only.
func agentPairingArgsError(action string, args map[string]any) string {
	if action != "off" {
		return ""
	}
	if _, ok := args["hours"]; ok {
		return "hours goes with action=on"
	}
	if _, ok := args["rcm_allow"]; ok {
		return "rcm_allow goes with action=on"
	}
	return ""
}

// agentPairingRead is the Tier 0 path: GET /v1/cluster/pairing.
func (gt *gatedTools) agentPairingRead(ctx context.Context, dbID string, args map[string]any) (*mcp.CallToolResult, any, error) {
	id := identity.Caller(ctx)
	if err := validateArgs(agentPairingTool, args); err != nil {
		gt.aud.Log(audit.Entry{Identity: id.Name, Database: dbID, Tool: agentPairingTool, Tier: 0, Decision: "denied", Detail: map[string]any{"reason": err.Error()}})
		return errText("DENIED: " + err.Error())
	}
	for _, k := range []string{"hours", "rcm_allow"} {
		if _, ok := args[k]; ok {
			msg := k + " goes with action=on; action=status only reads"
			gt.aud.Log(audit.Entry{Identity: id.Name, Database: dbID, Tool: agentPairingTool, Tier: 0, Decision: "denied", Detail: map[string]any{"reason": msg}})
			return errText("DENIED: " + msg)
		}
	}
	if d := gt.eng.EvaluateMeta(id, dbID, policy.ToolMeta{Name: agentPairingTool, Tier: 0, Scope: "instance"}); d.Outcome == "deny" {
		gt.aud.Log(audit.Entry{Identity: id.Name, Database: dbID, Tool: agentPairingTool, Tier: 0, Decision: "denied", Detail: map[string]any{"reason": d.Reason}})
		return errText("DENIED: " + d.Reason)
	}
	cl, acfg, err := gt.agentClient(agentStr(args["agent"]))
	if err == nil {
		var st agentapi.ClusterPairing
		st, err = cl.GetClusterPairing(ctx)
		if err == nil {
			gt.aud.Log(audit.Entry{Identity: id.Name, Database: dbID, Tool: agentPairingTool, Tier: 0, Decision: "allow", Detail: map[string]any{"agent": acfg.ID, "action": "status"}})
			return text(formatAgentPairing(acfg.ID, st)), nil, nil
		}
		err = agentPairingErr(err)
	}
	gt.aud.Log(audit.Entry{Identity: id.Name, Database: dbID, Tool: agentPairingTool, Tier: 0, Decision: "error", Detail: map[string]any{"reason": err.Error()}})
	return errText(err.Error())
}

// agentPairingPreview shows the window as it is now in the request.
func (gt *gatedTools) agentPairingPreview(ctx context.Context, dbID string, args map[string]any) string {
	cl, acfg, err := gt.agentClient(agentStr(args["agent"]))
	if err != nil {
		return "Now: unknown (" + err.Error() + ")"
	}
	st, err := cl.GetClusterPairing(ctx)
	if err != nil {
		return "Now: unknown (" + agentPairingErr(err).Error() + ")"
	}
	return "Now (agent " + acfg.ID + "): " + pairingLine(st)
}

// agentPairingExec runs an approved on/off.
func (gt *gatedTools) agentPairingExec(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
	cl, acfg, err := gt.agentClient(agentStr(args["agent"]))
	if err != nil {
		return "", err
	}
	action := strings.ToLower(strings.TrimSpace(agentStr(args["action"])))
	req := agentapi.ClusterPairingRequest{
		RequestedBy: agentStr(args["_grant_identity"]),
		Reason:      agentStr(args["reason"]),
	}
	switch action {
	case "on":
		req.On = true
		if h, ok := agentInt(args["hours"]); ok {
			req.Hours = h
		}
		if list := strings.TrimSpace(agentStr(args["rcm_allow"])); list != "" {
			for _, e := range strings.Split(list, ",") {
				if e = strings.TrimSpace(e); e != "" {
					req.RCMAllow = append(req.RCMAllow, e)
				}
			}
		}
	case "off":
	default:
		return "", fmt.Errorf("action must be on or off here, not %q", action)
	}
	st, err := cl.SetClusterPairing(ctx, req)
	if err != nil {
		return "", agentPairingErr(err)
	}
	return formatAgentPairing(acfg.ID, st), nil
}

// agentPairingErr adds the way out for an agent without the pairing API.
func agentPairingErr(err error) error {
	if strings.Contains(err.Error(), "fbagent not-found") {
		return fmt.Errorf("%w\nThis fbagent has no pairing API (no capability cluster_pairing; update fbagent). On the host: sudo -u firebird fbagent --cluster-pairing on", err)
	}
	return err
}

// pairingLine is the first line: open until X, or closed and why.
func pairingLine(st agentapi.ClusterPairing) string {
	switch {
	case st.State == "open":
		return fmt.Sprintf("open until %s (port %d)", st.Expires, st.Port)
	case st.Reason == "in_cluster":
		return fmt.Sprintf("closed: the host is in cluster %s", st.ClusterID)
	case st.Reason != "":
		return "closed (" + st.Reason + ")"
	default:
		return "closed"
	}
}

func formatAgentPairing(agentID string, st agentapi.ClusterPairing) string {
	var b strings.Builder
	fmt.Fprintf(&b, "agent %q pairing window: %s\n", agentID, pairingLine(st))
	if st.Fingerprint != "" {
		fmt.Fprintf(&b, "fingerprint: %s\n", st.Fingerprint)
	}
	if len(st.RCMAllow) > 0 {
		fmt.Fprintf(&b, "rcm_allow: %s\n", strings.Join(st.RCMAllow, ", "))
	} else {
		b.WriteString("rcm_allow: any RCM\n")
	}
	standing := st.Standing
	switch {
	case st.OwnCluster:
		standing += fmt.Sprintf(" (own cluster %s, RCM only)", st.ClusterID)
	case st.NotFreeReason != "":
		standing += " (" + st.NotFreeReason + ")"
	}
	fmt.Fprintf(&b, "standing: %s\n", standing)
	if st.Expires != "" && st.State != "open" {
		fmt.Fprintf(&b, "window open until: %s\n", st.Expires)
	}
	if st.Ended != "" {
		fmt.Fprintf(&b, "window ended: %s\n", st.Ended)
	}
	if st.ClaimedAt != "" {
		fmt.Fprintf(&b, "claimed: by %s at %s\n", st.ClaimedBy, st.ClaimedAt)
	}
	if st.Message != "" {
		b.WriteString(st.Message + "\n")
	}
	if st.State == "open" && st.Fingerprint != "" {
		b.WriteString("Next: fb_cluster_probe on the master shows this host and its fingerprint; compare them before fb_cluster_link.")
	}
	return strings.TrimRight(b.String(), "\n")
}
