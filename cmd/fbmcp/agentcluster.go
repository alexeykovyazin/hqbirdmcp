package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/agentapi"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/policy"
)

// fb_agent_cluster and fb_agent_cluster_rcm: the cluster routes of the
// host's fbagent (3.1.0+, capabilities cluster and cluster_rcm). Since kit
// 2027.2.29 the setup makes no RCM and opens no pairing window by default;
// these tools do on a running host what `--rcm on` does at install:
//
//   - fb_agent_cluster (Tier 0): GET /v1/cluster — the agent's cluster
//     state, its slot and ports, the pairing window, the RCM of the host;
//   - fb_agent_cluster_rcm (Tier 1, fb_confirm): POST /v1/cluster/rcm — the
//     agent makes the RCM of a new cluster on this host (fbagent
//     --cluster-init) as an operation. Firebird does not stop.
//
// fbmcp talks to the RCM with the RCM user and its password (Digest). The
// first password is in a root-only file on the host, so after the RCM is
// made `sudo fbmcp ctl hqbird-install` writes the rcm: section and the
// secret; then the fb_cluster_* tools appear.

const (
	agentClusterTool    = "fb_agent_cluster"
	agentClusterRCMTool = "fb_agent_cluster_rcm"
)

const agentClusterRCMImpact = "Make the HQbird cluster manager (RCM) of a new cluster on the host of the fbagent (fbagent --cluster-init: installs and starts hqbirdrcm, opens its ports 7443 and 7444 in the firewall, closes the pairing window of this agent; Firebird does not stop; audit key: db %s)."

// clusterRCMMinWait is the least wait for the cluster_rcm operation: the
// agent gives it 10 minutes.
const clusterRCMMinWait = 11 * time.Minute

// clusterIDRe is the agent's rule for cluster_id: a DNS label.
var clusterIDRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

const hqbirdInstallNext = "Next: on this host run `sudo fbmcp ctl hqbird-install` (it writes the rcm: section and the password secret from the RCM's first password file), reconnect the MCP client so that the fb_cluster_* tools appear, then fb_cluster_rcm_password and fb_cluster_status."

func registerAgentCluster(server *mcp.Server, gt *gatedTools) {
	type clusterArg struct {
		agentArg
	}
	registerAgentRead(server, gt, agentClusterTool, "cluster state of the host's fbagent (GET /v1/cluster, fbagent 3.1.0+): free / in_cluster / rcm_owner, the agent's slot, pairing port and node ports, the pairing window, and the RCM of the host (installed, staged, running, console URL). Says the next step: fb_agent_cluster_rcm on the master host, fb_agent_cluster_pairing on a replica host.",
		func(ctx context.Context, cl *agentapi.Client, acfg config.Agent, _ clusterArg) (string, error) {
			st, err := cl.GetCluster(ctx)
			if err != nil {
				return "", agentClusterErr(err)
			}
			return formatAgentCluster(acfg.ID, st, gt.live().RCM.On()), nil
		})

	gt.execs[agentClusterRCMTool] = gt.agentClusterRCMExec
	type rcmArg struct {
		Db   string         `json:"db,omitempty" jsonschema:"registry database id: the audit key (optional: a new host has no database)"`
		Mode string         `json:"mode,omitempty" jsonschema:"preview = dry run"`
		Args map[string]any `json:"args,omitempty"`
	}
	meta := policy.ToolMeta{Name: agentClusterRCMTool, Tier: 1, Scope: "instance"}
	mcp.AddTool(server, &mcp.Tool{Name: agentClusterRCMTool, Description: gatedDescription(meta.Name, meta.Tier, agentClusterRCMImpact)}, func(ctx context.Context, req *mcp.CallToolRequest, a rcmArg) (*mcp.CallToolResult, any, error) {
		if msg := clusterIDError(agentStr(a.Args["cluster_id"])); msg != "" {
			return errText("DENIED: " + msg)
		}
		msg, isErr := gt.requestGated(ctx, meta, agentClusterRCMImpact, a.Db, a.Args, a.Mode, gt.agentClusterRCMPreview)
		if isErr {
			return errText(msg)
		}
		return text(msg), nil, nil
	})
}

// clusterIDError checks cluster_id as the agent does; "" is fine (the agent
// takes the host name).
func clusterIDError(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || clusterIDRe.MatchString(id) {
		return ""
	}
	return "cluster_id must be a DNS label: a-z, 0-9 and -, at most 63 characters, not starting or ending with -"
}

// agentClusterErr adds the way out for an agent without the cluster API.
func agentClusterErr(err error) error {
	if strings.Contains(err.Error(), "fbagent not-found") {
		return fmt.Errorf("%w\nThis fbagent has no cluster API (it needs fbagent 3.1.0+: capabilities cluster and cluster_rcm; update fbagent). On the host: sudo fbagent --cluster-init makes the RCM, sudo -u firebird fbagent --cluster-pairing on opens the pairing window", err)
	}
	return err
}

// agentClusterRCMPreview shows the cluster state now and what the agent
// will refuse.
func (gt *gatedTools) agentClusterRCMPreview(ctx context.Context, dbID string, args map[string]any) string {
	cl, acfg, err := gt.agentClient(agentStr(args["agent"]))
	if err != nil {
		return "Now: unknown (" + err.Error() + ")"
	}
	st, err := cl.GetCluster(ctx)
	if err != nil {
		return "Now: unknown (" + agentClusterErr(err).Error() + ")"
	}
	s := "Now (agent " + acfg.ID + "): " + clusterLine(st)
	switch {
	case st.RCM.Installed:
		s += "\nThe host has an RCM already: the agent will refuse (rcm_owned). Use fb_agent_cluster to see it."
	case st.State == "in_cluster":
		s += "\nThe agent is in cluster " + st.ClusterID + ": the agent will refuse (in_cluster)."
	case !st.RCM.Staged:
		s += "\nhqbirdrcm is not in the agent's product cache: the agent will refuse (rcm_not_staged)."
	}
	if id := strings.TrimSpace(agentStr(args["cluster_id"])); id != "" {
		s += "\ncluster_id: " + id
	} else {
		s += "\ncluster_id: from the host name"
	}
	return s
}

// agentClusterRCMExec runs an approved POST /v1/cluster/rcm and waits for
// the operation.
func (gt *gatedTools) agentClusterRCMExec(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
	if msg := clusterIDError(agentStr(args["cluster_id"])); msg != "" {
		return "", fmt.Errorf("%s", msg)
	}
	cl, acfg, err := gt.agentClient(agentStr(args["agent"]))
	if err != nil {
		return "", err
	}
	resp, err := cl.MakeClusterRCM(ctx, agentapi.ClusterRCMRequest{
		ClusterID:   strings.TrimSpace(agentStr(args["cluster_id"])),
		RequestedBy: agentStr(args["_grant_identity"]),
		Reason:      agentStr(args["reason"]),
	})
	if err != nil {
		return "", agentClusterErr(err)
	}
	if prog != nil {
		prog(0.1, fmt.Sprintf("cluster_rcm operation %s accepted", resp.OperationID))
	}
	wait := time.Duration(acfg.TimeoutOrDefault()) * time.Second
	if wait < clusterRCMMinWait {
		wait = clusterRCMMinWait
	}
	pctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	op, err := cl.PollOperation(pctx, resp.OperationID, 3*time.Second, prog)
	if err != nil {
		return "", err
	}
	if op.State == "failed" {
		return "", fmt.Errorf("the RCM was not made (agent %q):\n%s", acfg.ID, agentapi.FormatOperation(op))
	}
	var res agentapi.ClusterRCMResult
	if len(op.Result) > 0 {
		_ = json.Unmarshal(op.Result, &res)
	}
	return formatClusterRCM(acfg.ID, op, res), nil
}

func formatClusterRCM(agentID string, op agentapi.Operation, r agentapi.ClusterRCMResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "agent %q made the RCM of cluster %s (operation %s)\n", agentID, r.ClusterID, op.OperationID)
	if r.RCMURL != "" {
		fmt.Fprintf(&b, "rcm_url (agents join here): %s\n", r.RCMURL)
	}
	if r.ConsoleURL != "" {
		fmt.Fprintf(&b, "console: %s\n", r.ConsoleURL)
	}
	if r.CAHash != "" {
		fmt.Fprintf(&b, "cluster CA hash: %s\n", r.CAHash)
	}
	if r.Login != "" {
		fmt.Fprintf(&b, "RCM user: %s\n", r.Login)
	}
	if r.PasswordFile != "" {
		fmt.Fprintf(&b, "first password: in %s on the host (not shown)\n", r.PasswordFile)
	}
	if r.Firewall != "" {
		fmt.Fprintf(&b, "firewall: %s\n", r.Firewall)
	}
	if r.Note != "" {
		b.WriteString(r.Note + "\n")
	}
	if len(op.Warnings) > 0 {
		fmt.Fprintf(&b, "warnings: %s\n", strings.Join(op.Warnings, "; "))
	}
	b.WriteString(hqbirdInstallNext)
	return b.String()
}

// clusterLine is the first line of the state: free, member or RCM owner.
func clusterLine(st agentapi.ClusterState) string {
	s := st.State
	if st.ClusterID != "" {
		s += " (cluster " + st.ClusterID + ")"
	}
	if st.RCMOwner {
		s += ", runs the RCM of the host"
	}
	if st.Reason != "" {
		s += ": " + st.Reason
	}
	return s
}

func formatAgentCluster(agentID string, st agentapi.ClusterState, rcmConfigured bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "agent %q cluster state: %s\n", agentID, clusterLine(st))
	if st.Source != "" {
		fmt.Fprintf(&b, "source: %s\n", st.Source)
	}
	slot := fmt.Sprintf("%d", st.Slot)
	if st.Slot < 0 {
		slot = "none (the ports of slot 0)"
	}
	fmt.Fprintf(&b, "slot: %s, pairing port %d, node ports %s\n", slot, st.PairingPort, intList(st.NodePorts))
	if st.Pairing != nil {
		fmt.Fprintf(&b, "pairing window: %s\n", pairingLine(*st.Pairing))
		if st.Pairing.Fingerprint != "" {
			fmt.Fprintf(&b, "fingerprint: %s\n", st.Pairing.Fingerprint)
		}
	}
	r := st.RCM
	switch {
	case r.Installed:
		fmt.Fprintf(&b, "RCM of the host: installed, running %t", r.Running)
		if r.ClusterID != "" {
			fmt.Fprintf(&b, ", cluster %s", r.ClusterID)
		}
		if r.ThisAgent {
			b.WriteString(", made by this agent")
		} else if r.OwnerAgentID != "" {
			fmt.Fprintf(&b, ", made by agent %s", r.OwnerAgentID)
		}
		b.WriteString("\n")
		if r.ConsoleURL != "" {
			fmt.Fprintf(&b, "console: %s\n", r.ConsoleURL)
		}
	case r.Staged:
		b.WriteString("RCM of the host: none (hqbirdrcm is staged: fb_agent_cluster_rcm can make it)\n")
	default:
		b.WriteString("RCM of the host: none (hqbirdrcm is not staged on this agent)\n")
	}
	for _, w := range st.Warnings {
		fmt.Fprintf(&b, "warning: %s\n", w)
	}
	b.WriteString(clusterNext(st, rcmConfigured))
	return b.String()
}

// clusterNext is the next step for the state.
func clusterNext(st agentapi.ClusterState, rcmConfigured bool) string {
	switch {
	case st.RCM.Installed && !rcmConfigured:
		return hqbirdInstallNext
	case st.RCM.Installed:
		return "Next: fb_cluster_status. To add a host: on that host fb_agent_cluster_pairing action=on, then here fb_cluster_probe and fb_cluster_link."
	case st.State == "in_cluster":
		return "This host is a member of cluster " + st.ClusterID + ". Manage the cluster with fbmcp on the host of its RCM (fb_cluster_*)."
	case st.RCM.Staged:
		return "Next: on the master host fb_agent_cluster_rcm (makes the RCM of a new cluster); on a replica host fb_agent_cluster_pairing action=on, then link it from the master host."
	default:
		return "Next: on a replica host fb_agent_cluster_pairing action=on, then link it from the master host. This agent cannot make an RCM: hqbirdrcm is not in its product cache."
	}
}

func intList(v []int) string {
	if len(v) == 0 {
		return "-"
	}
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = fmt.Sprintf("%d", n)
	}
	return strings.Join(parts, ", ")
}
