package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/aleks/fbmcp/internal/rcmapi"
)

// fb_cluster_unlink takes a server out of the cluster through RCM (plan
// docs/rcm-cluster-next.ru.md, stage D): the members of its agent and their
// links leave the cluster document (as the RCM Cluster page removes a
// member), then RCM revokes the agent. fbagent 3.1.0+ leaves the cluster by
// itself when RCM takes its last member away or revokes it: it stops and
// unregisters its node service, closes the node port, deletes its cluster
// identity and is free to be linked again. Without the revoke RCM would
// refuse the next link of the host (stale_agent).

// unlinkPlan is what fb_cluster_unlink changes.
type unlinkPlan struct {
	doc     rcmapi.ClusterDoc
	agentID string
	members []rcmapi.DocMember
	links   []rcmapi.DocLink
	newDoc  []byte
}

func (p unlinkPlan) memberIDs() string {
	ids := make([]string, 0, len(p.members))
	for _, m := range p.members {
		ids = append(ids, m.MemberID)
	}
	return strings.Join(ids, ",")
}

// unlinkPlanOf reads the cluster and finds the members of the server.
func unlinkPlanOf(ctx context.Context, cl *rcmapi.Client, args map[string]any) (unlinkPlan, error) {
	var p unlinkPlan
	node := strings.TrimSpace(agentStr(args["node"]))
	agent := strings.TrimSpace(agentStr(args["agent"]))
	if (node == "") == (agent == "") {
		return p, errors.New("give node (a node id from fb_cluster_status view=nodes) or agent (fb_cluster_status view=agents), one of them")
	}
	doc, err := cl.TheCluster(ctx)
	if err != nil {
		return p, err
	}
	p.doc = doc
	p.agentID = agent
	if node != "" {
		m, ok := doc.Member(node)
		if !ok {
			return p, fmt.Errorf("node %s is not a member of cluster %s (fb_cluster_status view=nodes)", node, doc.ClusterID)
		}
		p.agentID = m.AgentID
	}
	p.members = doc.AgentMembers(p.agentID)
	for _, m := range p.members {
		if m.Role == rcmapi.RoleRCM {
			return p, fmt.Errorf("agent %s runs the RCM of cluster %s (member %s): the RCM host cannot leave its own cluster", p.agentID, doc.ClusterID, m.MemberID)
		}
	}
	if len(p.members) == 0 {
		agents, err := cl.Agents(ctx)
		if err != nil {
			return p, err
		}
		for _, a := range agents {
			if a.AgentID != p.agentID {
				continue
			}
			if a.Revoked {
				return p, fmt.Errorf("agent %s has no members and is revoked already: nothing to unlink", p.agentID)
			}
			return p, nil
		}
		return p, fmt.Errorf("RCM knows no agent %s (fb_cluster_status view=agents)", p.agentID)
	}
	ids := map[string]bool{}
	for _, m := range p.members {
		ids[m.MemberID] = true
	}
	p.newDoc, p.links, err = doc.Without(ids)
	return p, err
}

// unlinkText describes the plan.
func unlinkText(p unlinkPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "cluster %s, revision %d: agent %s\n", p.doc.ClusterID, p.doc.Revision, p.agentID)
	if len(p.members) == 0 {
		b.WriteString("the agent has no members left: RCM only revokes it\n")
	}
	for _, m := range p.members {
		fmt.Fprintf(&b, "- remove member %s: %s, %s, node port %d\n", m.MemberID, m.Role, strings.Join(m.Addresses, " "), m.ListenPort)
	}
	for _, l := range p.links {
		fmt.Fprintf(&b, "- remove link %s -> %s\n", l.From, l.To)
	}
	return b.String()
}

const unlinkEffect = "Then RCM revokes the agent. The agent on that host (fbagent 3.1.0+) stops and unregisters its node service, closes the node port and forgets the cluster; the database files stay. A removed replica gets no more changes from its master.\n"

func (gt *gatedTools) clusterUnlinkPreview(ctx context.Context, dbID string, args map[string]any) string {
	cl, _, err := gt.rcmClient("")
	if err != nil {
		return "RCM: " + err.Error()
	}
	p, err := unlinkPlanOf(ctx, cl, args)
	if err != nil {
		return "The request will be refused when confirmed: " + err.Error()
	}
	args["_revision"] = strconv.FormatInt(p.doc.Revision, 10)
	args["_members"] = p.memberIDs()
	s := "Unlink — " + unlinkText(p)
	if len(p.members) > 0 {
		v, err := cl.ValidateCluster(ctx, p.doc.ClusterID, p.newDoc)
		switch {
		case err != nil:
			s += "RCM check failed: " + err.Error() + "\n"
		case len(v.Errors) > 0:
			s += "The request will be refused when confirmed: RCM refuses the new cluster document: " + strings.Join(v.Errors, "; ") + "\n"
			if hasReplicaWithoutMaster(v.Errors) {
				s += "A master with replicas cannot leave: unlink its replicas first.\n"
			}
		default:
			s += "RCM check: ok\n"
		}
		for _, w := range v.Warnings {
			s += "RCM warning: " + w + "\n"
		}
	}
	return s + unlinkEffect
}

func hasReplicaWithoutMaster(errs []string) bool {
	for _, e := range errs {
		if strings.Contains(e, "has no link from a master") {
			return true
		}
	}
	return false
}

func (gt *gatedTools) clusterUnlinkExec(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
	cl, rc, err := gt.rcmClient(clusterActor(args))
	if err != nil {
		return "", err
	}
	// The refusal of the plan comes first: a request that was refused at
	// request time has no pins either.
	p, err := unlinkPlanOf(ctx, cl, args)
	if err != nil {
		return "", fmt.Errorf("%w; nothing changed", rcmDown(err, rc))
	}
	pinned := agentStr(args["_revision"])
	if pinned == "" {
		return "", errors.New("the request has no cluster revision (RCM did not answer when it was requested); nothing changed. Request fb_cluster_unlink again")
	}
	if strconv.FormatInt(p.doc.Revision, 10) != pinned || p.memberIDs() != agentStr(args["_members"]) {
		return "", fmt.Errorf("cluster %s changed after the request (revision %s, now %d); nothing changed. Check fb_cluster_status view=nodes, then request fb_cluster_unlink again", p.doc.ClusterID, pinned, p.doc.Revision)
	}
	var out strings.Builder
	out.WriteString(unlinkText(p))
	if len(p.members) > 0 {
		if prog != nil {
			prog(0.3, "unlink: cluster document")
		}
		stored, err := cl.ReplaceCluster(ctx, p.doc.ClusterID, p.doc.Revision, p.newDoc)
		if err != nil {
			if rcmapi.IsCode(err, "conflict") {
				return "", fmt.Errorf("cluster %s changed after the request: %v; nothing changed. Request fb_cluster_unlink again", p.doc.ClusterID, err)
			}
			return "", fmt.Errorf("%w; nothing changed", rcmDown(err, rc))
		}
		fmt.Fprintf(&out, "members %s removed: cluster %s revision %d\n", p.memberIDs(), stored.ClusterID, stored.Revision)
	}
	if prog != nil {
		prog(0.7, "unlink: revoke agent")
	}
	if err := cl.RevokeAgent(ctx, p.agentID); err != nil {
		return "", fmt.Errorf("%sRCM did not revoke agent %s: %v. Request fb_cluster_unlink args={agent:%q} again: it only revokes", out.String(), p.agentID, rcmDown(err, rc), p.agentID)
	}
	fmt.Fprintf(&out, "agent %s revoked\n", p.agentID)
	return out.String() + "Next: on that host fb_agent_cluster shows state free within about two minutes (the agent stops its node and forgets the cluster). To link it again: fb_agent_cluster_pairing on that host, fb_cluster_probe, then fb_cluster_link with replica_of; the replica gets a new node id.", nil
}
