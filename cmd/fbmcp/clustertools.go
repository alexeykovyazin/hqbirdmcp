// HQbird cluster tools (fb_cluster_*): fbmcp drives the cluster manager
// (hqbirdrcm) of its host through the RCM operator API — link the hosts,
// put databases under replication, initialize the replicas, verify them.
// Plan: docs/rcm-cluster-plan.ru.md.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/policy"
	"github.com/aleks/fbmcp/internal/rcmapi"
	"github.com/aleks/fbmcp/internal/secrets"
)

// clusterPoll is the poll interval of RCM jobs and commands.
var clusterPoll = 2 * time.Second

// clusterMaxText bounds one tool answer (RCM lists can be long).
const clusterMaxText = 60 << 10

// errRCMOff is the answer of every cluster tool without the rcm: section.
var errRCMOff = errors.New("the HQbird cluster manager is not configured: fbmcp.yaml has no rcm: section. On an HQbird host run `fbmcp ctl hqbird-install` (it writes the section and the password secret)")

// rcmClient builds the RCM client from the live config. actor names the
// fbmcp caller in the RCM audit (X-Admin-User).
func (gt *gatedTools) rcmClient(actor string) (*rcmapi.Client, config.RCM, error) {
	rc := gt.live().RCM
	if !rc.On() {
		return nil, rc, errRCMOff
	}
	pass, err := config.SecretFromEnv(rc.PasswordEnvOrDefault())
	if err != nil {
		return nil, rc, fmt.Errorf("rcm password: %v. Store it: fbmcp ctl secret set %s (the value comes from stdin)", err, rc.PasswordEnvOrDefault())
	}
	cl, err := rcmapi.New(rc.URL, rc.CAFile, rc.UserOrDefault(), pass)
	if err != nil {
		return nil, rc, err
	}
	if actor != "" {
		cl = cl.WithActor("fbmcp:" + actor)
	}
	return cl, rc, nil
}

// rcmBudget is the wait budget of one RCM job.
func rcmBudget(ctx context.Context, rc config.RCM) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, time.Duration(rc.TimeoutOrDefault())*time.Second)
}

// rcmDown adds the hint for an RCM that does not answer.
func rcmDown(err error, rc config.RCM) error {
	// Only a transport error (no answer) gets the hint: not an RCM answer,
	// not a wrong argument, not a wait budget.
	var ue *url.Error
	if err == nil || !errors.As(err, &ue) {
		return err
	}
	return fmt.Errorf("%v\nNext: the cluster manager does not answer at %s. Check `systemctl status hqbirdrcm`. After fb_cluster_link the RCM of a replica host is stopped: use fbmcp on the master host", err, rc.URL)
}

func clip(s string) string {
	if len(s) <= clusterMaxText {
		return s
	}
	return s[:clusterMaxText] + "\n… (cut; ask a narrower view)"
}

// splitHosts reads "a, b c" into a list.
func splitHosts(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == ';' || r == '\n' || r == '\t' })
}

// clusterArg is embedded by every Tier 0 cluster tool.
type clusterArg struct {
	Db string `json:"db,omitempty" jsonschema:"registry database id (audit key; optional)"`
}

// registerClusterRead wires one Tier 0 cluster tool (as registerAgentRead).
func registerClusterRead[A any](server *mcp.Server, gt *gatedTools, name, desc string, fn func(ctx context.Context, cl *rcmapi.Client, rc config.RCM, a A) (string, error)) {
	mcp.AddTool(server, &mcp.Tool{Name: name, Description: "Tier 0: " + desc}, func(ctx context.Context, req *mcp.CallToolRequest, a A) (*mcp.CallToolResult, any, error) {
		id := identity.Caller(ctx)
		b, _ := json.Marshal(a)
		var base clusterArg
		_ = json.Unmarshal(b, &base)
		if d := gt.eng.Evaluate(id, base.Db, name); d.Outcome == "deny" {
			gt.aud.Log(audit.Entry{Identity: id.Name, Database: base.Db, Tool: name, Tier: 0, Decision: "denied", Detail: map[string]any{"reason": d.Reason}})
			return errText("DENIED: " + d.Reason)
		}
		cl, rc, err := gt.rcmClient(id.Name)
		if err == nil {
			var out string
			out, err = fn(ctx, cl, rc, a)
			if err == nil {
				gt.aud.Log(audit.Entry{Identity: id.Name, Database: base.Db, Tool: name, Tier: 0, Decision: "allow", Detail: map[string]any{"rcm": rc.URL}})
				return text(clip(out)), nil, nil
			}
			err = rcmDown(err, rc)
		}
		gt.aud.Log(audit.Entry{Identity: id.Name, Database: base.Db, Tool: name, Tier: 0, Decision: "error", Detail: map[string]any{"reason": err.Error()}})
		return errText(err.Error())
	})
}

type clusterStatusArg struct {
	clusterArg
	View string `json:"view,omitempty" jsonschema:"overview (default) | nodes | databases | commands | command | alerts | releases | node | config | reinit | verify | link"`
	ID   string `json:"id,omitempty" jsonschema:"node id (node, config), command id (command), job id (reinit, verify), link job id (link; default latest)"`
}

type clusterProbeArg struct {
	clusterArg
	Hosts []string `json:"hosts" jsonschema:"address of each server: host or host:port of its pairing port (7450 by default)"`
}

type clusterVerifyArg struct {
	clusterArg
	MasterNode  string `json:"master_node" jsonschema:"node id of the master"`
	MasterDB    string `json:"master_db" jsonschema:"database id on the master"`
	ReplicaNode string `json:"replica_node" jsonschema:"node id of the replica"`
	ReplicaDB   string `json:"replica_db,omitempty" jsonschema:"database id on the replica (default: master_db)"`
}

// clusterViews maps a view to its RCM path; %s is the id.
var clusterViews = map[string]struct {
	path  string
	needs bool
}{
	"nodes":     {"/v1/nodes", false},
	"databases": {"/v1/databases", false},
	"commands":  {"/v1/commands", false},
	"alerts":    {"/v1/alerts", false},
	"releases":  {"/v1/admin/releases", false},
	"command":   {"/v1/commands/%s", true},
	"node":      {"/v1/nodes/%s", true},
	"config":    {"/v1/nodes/%s/config", true},
	"reinit":    {"/v1/reinit/%s", true},
	"verify":    {"/v1/verify/%s", true},
	"link":      {"/v1/admin/pair/link/%s", false},
}

func clusterViewNames() string {
	names := []string{"overview"}
	for k := range clusterViews {
		names = append(names, k)
	}
	sort.Strings(names[1:])
	return strings.Join(names, ", ")
}

func clusterStatus(ctx context.Context, cl *rcmapi.Client, _ config.RCM, a clusterStatusArg) (string, error) {
	view := strings.ToLower(strings.TrimSpace(a.View))
	if view == "" || view == "overview" {
		var b strings.Builder
		fmt.Fprintf(&b, "RCM %s\n", cl.BaseURL())
		for _, sec := range []struct{ title, path string }{
			{"health", "/v1/health"},
			{"cluster", "/v1/cluster/status"},
			{"nodes", "/v1/nodes"},
			{"databases", "/v1/databases"},
		} {
			raw, err := cl.Do(ctx, http.MethodGet, sec.path, nil)
			if err != nil {
				if sec.title == "health" {
					return "", err
				}
				fmt.Fprintf(&b, "\n== %s ==\nerror: %v\n", sec.title, err)
				continue
			}
			fmt.Fprintf(&b, "\n== %s ==\n%s\n", sec.title, prettyJSON(raw))
		}
		b.WriteString("\nNext: fb_cluster_probe hosts=[…] before fb_cluster_link; view=databases for the pair states.")
		return b.String(), nil
	}
	v, ok := clusterViews[view]
	if !ok {
		return "", fmt.Errorf("unknown view %q; use one of: %s", a.View, clusterViewNames())
	}
	path := v.path
	if strings.Contains(path, "%s") {
		id := strings.TrimSpace(a.ID)
		if id == "" && view == "link" {
			id = "latest"
		}
		if id == "" {
			return "", fmt.Errorf("view %s needs id", view)
		}
		path = fmt.Sprintf(path, rcmapi.PathEscape(id))
	}
	raw, err := cl.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("GET %s\n%s", path, prettyJSON(raw)), nil
}

// probeTable renders a probe answer for the operator.
func probeTable(p rcmapi.ProbeAnswer) string {
	var b strings.Builder
	fmt.Fprintf(&b, "cluster %s, RCM URL the agents join: %s, seats %d\n", p.ClusterID, p.RCMURL, p.Seats)
	for _, h := range p.Hosts {
		fmt.Fprintf(&b, "- %s: state %s", h.Input, h.State)
		if h.NodeName != "" || h.Hostname != "" {
			fmt.Fprintf(&b, ", node %s (host %s)", h.NodeName, h.Hostname)
		}
		if h.Platform != "" {
			fmt.Fprintf(&b, ", %s", h.Platform)
		}
		if h.FBAgentVersion != "" {
			fmt.Fprintf(&b, ", fbagent %s", h.FBAgentVersion)
		}
		if h.FirebirdVersion != "" {
			fmt.Fprintf(&b, ", Firebird %s", h.FirebirdVersion)
		}
		if h.Instance != "" {
			fmt.Fprintf(&b, ", instance %s", h.Instance)
		}
		if h.FirebirdPort != 0 {
			fmt.Fprintf(&b, ", Firebird port %d", h.FirebirdPort)
		}
		if h.NodePort != 0 {
			fmt.Fprintf(&b, ", node port %d", h.NodePort)
		}
		if h.RCMHost {
			b.WriteString(", RCM host")
		}
		if h.RCMLocal {
			b.WriteString(", own RCM (stopped and disabled when it joins)")
		}
		if len(h.Roles) > 0 {
			fmt.Fprintf(&b, ", roles %s", strings.Join(h.Roles, "+"))
		}
		if h.Fingerprint != "" {
			fmt.Fprintf(&b, "\n  fingerprint %s", h.Fingerprint)
		} else if h.State == rcmapi.HostMember {
			b.WriteString("\n  in this cluster: no fingerprint to compare")
		}
		if len(h.MemberIDs) > 0 {
			keys := make([]string, 0, len(h.MemberIDs))
			for k := range h.MemberIDs {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, fmt.Sprintf("master %s → %s", k, h.MemberIDs[k]))
			}
			fmt.Fprintf(&b, "\n  member id: %s", strings.Join(parts, "; "))
		}
		if h.Error != "" {
			fmt.Fprintf(&b, "\n  error %s: %s", h.Code, h.Error)
		}
		b.WriteString("\n")
	}
	for _, k := range p.Skipped {
		fmt.Fprintf(&b, "skipped ports of %s: %s\n", k.Input, k.Summary)
	}
	return b.String()
}

func clusterProbe(ctx context.Context, cl *rcmapi.Client, _ config.RCM, a clusterProbeArg) (string, error) {
	var hosts []string
	for _, h := range a.Hosts {
		hosts = append(hosts, splitHosts(h)...)
	}
	if len(hosts) == 0 {
		return "", fmt.Errorf("hosts is required: the address of each server (host or host:port of the pairing port)")
	}
	p, err := cl.Probe(ctx, hosts)
	if err != nil {
		return "", err
	}
	return probeTable(p) + "\nEach row is one agent (host:port of its pairing port). Compare each fingerprint with fb_agent_cluster_pairing on that host (or the line `Pairing:` of the installer). Next: fb_cluster_link with the rows to link; a new replica of a running pair needs replica_of.", nil
}

func clusterVerify(ctx context.Context, cl *rcmapi.Client, rc config.RCM, a clusterVerifyArg) (string, error) {
	if a.ReplicaDB == "" {
		a.ReplicaDB = a.MasterDB
	}
	if a.MasterNode == "" || a.MasterDB == "" || a.ReplicaNode == "" {
		return "", fmt.Errorf("master_node, master_db and replica_node are required (ids from fb_cluster_status view=databases)")
	}
	raw, err := cl.Do(ctx, http.MethodPost, "/v1/verify", map[string]string{
		"master_node": a.MasterNode, "master_db": a.MasterDB, "replica_node": a.ReplicaNode, "replica_db": a.ReplicaDB,
	})
	if err != nil {
		return "", err
	}
	var j rcmapi.Job
	if err := json.Unmarshal(raw, &j); err != nil || j.ID == "" {
		return "", fmt.Errorf("rcm verify: no job id in %s", strings.TrimSpace(string(raw)))
	}
	wctx, cancel := rcmBudget(ctx, rc)
	defer cancel()
	j, last, err := cl.WaitJob(wctx, "/v1/verify", j.ID, clusterPoll, nil)
	if err != nil {
		return "", fmt.Errorf("%v; follow it with fb_cluster_status view=verify id=%s", err, j.ID)
	}
	verdict := "the replica matches the master"
	if j.Status != "ok" {
		verdict = "the replica does NOT match the master (status " + j.Status + ")"
	}
	return fmt.Sprintf("verify %s: %s\n%s", j.ID, verdict, prettyJSON(last)), nil
}

// --- Tier 1/2 executors ---

// clusterActor is the fbmcp identity that confirmed the request.
func clusterActor(args map[string]any) string { return agentStr(args["_grant_identity"]) }

// waitCommandText waits for an RCM node command and renders the result. A
// command that does not succeed is an error (the job is failed).
func waitCommandText(ctx context.Context, cl *rcmapi.Client, rc config.RCM, what string, raw json.RawMessage) (string, error) {
	var start struct {
		CommandID string `json:"command_id"`
	}
	if err := json.Unmarshal(raw, &start); err != nil || start.CommandID == "" {
		return fmt.Sprintf("%s:\n%s", what, prettyJSON(raw)), nil
	}
	wctx, cancel := rcmBudget(ctx, rc)
	defer cancel()
	cmd, last, err := cl.WaitCommand(wctx, start.CommandID, clusterPoll)
	if err != nil {
		return "", err
	}
	if cmd.State != "succeeded" {
		return "", fmt.Errorf("%s: command %s ended %s: %s\n%s", what, cmd.ID, cmd.State, cmd.Error, prettyJSON(last))
	}
	return fmt.Sprintf("%s: command %s succeeded\n%s", what, cmd.ID, prettyJSON(last)), nil
}

func nodeArg(args map[string]any) (string, error) {
	n := strings.TrimSpace(agentStr(args["node"]))
	if n == "" {
		return "", fmt.Errorf("node is required (a node id from fb_cluster_status view=nodes)")
	}
	return n, nil
}

// newRCMPassword is 24 characters of letters and digits.
func newRCMPassword() (string, error) {
	const abc = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	out := make([]byte, 24)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(abc))))
		if err != nil {
			return "", err
		}
		out[i] = abc[n.Int64()]
	}
	return string(out), nil
}

// storeRCMPassword writes the secret where fbmcp reads it from. It refuses
// an environment variable: fbmcp cannot change it.
func storeRCMPassword(name, value string) error {
	_, src, err := secrets.Lookup(name)
	switch {
	case err == nil && src == secrets.SourceEnv:
		return fmt.Errorf("the RCM password comes from the environment variable %s; fbmcp cannot change it there", name)
	case err == nil && src == secrets.SourceKeyring:
		return secrets.Set(name, value)
	case secrets.Dir() != "":
		return secrets.WriteFile(secrets.Dir(), name, value)
	}
	return secrets.Set(name, value)
}

func (gt *gatedTools) clusterPasswordExec(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
	cl, rc, err := gt.rcmClient(clusterActor(args))
	if err != nil {
		return "", err
	}
	name := rc.PasswordEnvOrDefault()
	old, err := config.SecretFromEnv(name)
	if err != nil {
		return "", err
	}
	pw, err := newRCMPassword()
	if err != nil {
		return "", err
	}
	// Store first: when the secret cannot be written, RCM keeps the old
	// password and nothing is lost.
	if err := storeRCMPassword(name, pw); err != nil {
		return "", fmt.Errorf("nothing changed: %w", err)
	}
	_, err = cl.Do(ctx, http.MethodPost, "/v1/account/password", map[string]string{
		"current_password": old, "new_password": pw, "repeat_password": pw,
	})
	if err != nil {
		if rerr := storeRCMPassword(name, old); rerr != nil {
			return "", fmt.Errorf("RCM refused the change (%v) and fbmcp could not put the old password back into secret %s (%v): store it again with fbmcp ctl secret set %s", err, name, rerr, name)
		}
		return "", fmt.Errorf("RCM refused the change; the old password stays: %w", rcmDown(err, rc))
	}
	return fmt.Sprintf("the password of RCM user %q is changed to a new random value of 24 characters and stored in the fbmcp secret %s. It is not shown. To log in to the RCM UI, set your own password there (fbmcp then needs it: fbmcp ctl secret set %s).", rc.UserOrDefault(), name, name), nil
}

// clusterLinkPreview probes the hosts, shows the fingerprints and pins them
// into the request: the executor links only hosts with the same ones.
// memberPin is the pin of a row that is in the cluster at request time (the
// RCM host since RCM 2027.5.9): it has no fingerprint, and the confirmed
// run links it only when it is still a member.
const memberPin = "member"

func (gt *gatedTools) clusterLinkPreview(ctx context.Context, dbID string, args map[string]any) string {
	cl, _, err := gt.rcmClient("")
	if err != nil {
		return "Probe: " + err.Error()
	}
	p, err := cl.Probe(ctx, splitHosts(agentStr(args["hosts"])))
	if err != nil {
		return "Probe failed: " + err.Error() + " (the request will be refused when confirmed)"
	}
	pins := map[string]any{}
	for _, h := range p.Hosts {
		switch {
		case h.State == rcmapi.HostMember:
			pins[h.Input] = memberPin
		case h.Fingerprint != "":
			pins[h.Input] = h.Fingerprint
		}
	}
	args["_fingerprints"] = pins
	s := "Probe — compare each fingerprint with fb_agent_cluster_pairing on that host (or the line `Pairing:` of the installer); the link takes every row below and runs only with these fingerprints:\n" + probeTable(p)
	if msg := linkModeError(args); msg != "" {
		s += "The request will be refused when confirmed: " + msg + "\n"
	}
	s += "mode: " + linkMode(args) + "\n"
	if agentStr(args["rcm_url"]) == "" {
		s += "rcm_url: " + p.RCMURL + " (the RCM default)\n"
	}
	return s
}

func (gt *gatedTools) clusterLinkExec(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
	cl, rc, err := gt.rcmClient(clusterActor(args))
	if err != nil {
		return "", err
	}
	hosts := splitHosts(agentStr(args["hosts"]))
	pins, _ := args["_fingerprints"].(map[string]any)
	if len(pins) == 0 {
		return "", fmt.Errorf("the request has no probed fingerprints (the probe failed when it was requested); nothing linked. Request fb_cluster_link again")
	}
	p, err := cl.Probe(ctx, hosts)
	if err != nil {
		return "", rcmDown(err, rc)
	}
	if msg := linkModeError(args); msg != "" {
		return "", fmt.Errorf("%s; nothing linked", msg)
	}
	newMaster, _ := args["new_master"].(bool)
	req := rcmapi.LinkRequest{Master: agentStr(args["master"]), RCMURL: agentStr(args["rcm_url"]),
		NewMaster: newMaster, ReplicaOf: strings.TrimSpace(agentStr(args["replica_of"]))}
	for _, h := range p.Hosts {
		want := agentStr(pins[h.Input])
		switch {
		case h.State == rcmapi.HostError:
			return "", fmt.Errorf("host %s: %s %s; nothing linked", h.Input, h.Code, h.Error)
		case h.State == rcmapi.HostMember:
			// The RCM host (or an agent already in this cluster): RCM
			// knows it by its certificate, there is no fingerprint.
			if want != memberPin {
				return "", fmt.Errorf("host %s was not a member of the cluster when the link was requested; nothing linked. Request fb_cluster_link again", h.Input)
			}
			req.Hosts = append(req.Hosts, rcmapi.LinkHostIn{Host: h.Input})
			continue
		case want == "" || want == memberPin:
			return "", fmt.Errorf("host %s had no fingerprint when the link was requested; nothing linked. Request fb_cluster_link again", h.Input)
		case want != h.Fingerprint:
			return "", fmt.Errorf("the fingerprint of host %s changed after the request (was %s, now %s); nothing linked. Check the host, then request fb_cluster_link again", h.Input, want, h.Fingerprint)
		}
		req.Hosts = append(req.Hosts, rcmapi.LinkHostIn{Host: h.Input, Fingerprint: h.Fingerprint})
	}
	job, err := cl.Link(ctx, req)
	if err != nil {
		if rcmapi.IsCode(err, rcmapi.CodeLoneMaster) {
			return "", fmt.Errorf("%v\n%s", err, loneMasterNext)
		}
		return "", err
	}
	wctx, cancel := rcmBudget(ctx, rc)
	defer cancel()
	job, err = cl.WaitLink(wctx, job, clusterPoll, func(j rcmapi.LinkJob) {
		if prog != nil {
			prog(0.5, "link: "+j.Step)
		}
	})
	if err != nil {
		return "", err
	}
	out := linkText(job)
	if job.State != rcmapi.LinkDone {
		// RCM 2027.5.9 refuses lone_master in the job, not in the request.
		if job.Code == rcmapi.CodeLoneMaster {
			out += loneMasterNext
		}
		return "", errors.New(out)
	}
	return out + "Next: fb_cluster_status view=nodes (node ids), then fb_cluster_replica_folder on the replica node and fb_cluster_enroll on the master node.", nil
}

// loneMasterNext is the way out of lone_master: one free row into a
// cluster that has a master.
const loneMasterNext = "Next: request fb_cluster_link again with replica_of=<master member id> (fb_cluster_status view=nodes) to add a replica of that master, or with new_master=true for a master of its own."

// linkModeError checks master, new_master and replica_of as RCM does.
func linkModeError(args map[string]any) string {
	newMaster, _ := args["new_master"].(bool)
	if strings.TrimSpace(agentStr(args["replica_of"])) != "" && (newMaster || strings.TrimSpace(agentStr(args["master"])) != "") {
		return "replica_of names the master: give no master and no new_master with it"
	}
	return ""
}

// linkMode says what the link makes of the rows.
func linkMode(args map[string]any) string {
	if r := strings.TrimSpace(agentStr(args["replica_of"])); r != "" {
		return "every row gets a replica of master member " + r
	}
	m := strings.TrimSpace(agentStr(args["master"]))
	if m == "" {
		m = "the first row"
	}
	s := "the master is " + m + ", the other rows get its replica"
	if newMaster, _ := args["new_master"].(bool); newMaster {
		s += " (new_master: a master of its own beside the masters the cluster has)"
	}
	return s
}

func linkText(j rcmapi.LinkJob) string {
	var b strings.Builder
	fmt.Fprintf(&b, "link job %s: %s (step %s), master %s, RCM %s\n", j.ID, j.State, j.Step, j.Master, j.RCMURL)
	if j.ReplicaOf != "" {
		fmt.Fprintf(&b, "every row is a replica of %s\n", j.ReplicaOf)
	}
	if j.NewMaster {
		b.WriteString("new master allowed (another instance pair)\n")
	}
	if j.Error != "" {
		fmt.Fprintf(&b, "error %s: %s\n", j.Code, j.Error)
	}
	for _, e := range j.Errors {
		fmt.Fprintf(&b, "error: %s\n", e)
	}
	for _, h := range j.Hosts {
		fmt.Fprintf(&b, "- %s: %s, role %s", h.Input, h.State, h.Role)
		if h.MemberID != "" {
			fmt.Fprintf(&b, ", member %s", h.MemberID)
		}
		if h.Service != "" {
			fmt.Fprintf(&b, ", service %s", h.Service)
		}
		if h.NodePort != 0 {
			fmt.Fprintf(&b, ", node port %d", h.NodePort)
		}
		if h.Error != "" {
			fmt.Fprintf(&b, ", error %s", h.Error)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (gt *gatedTools) clusterReplicaFolderExec(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
	cl, rc, err := gt.rcmClient(clusterActor(args))
	if err != nil {
		return "", err
	}
	node, err := nodeArg(args)
	if err != nil {
		return "", err
	}
	root := strings.TrimSpace(agentStr(args["root"]))
	if root == "" {
		root = "/databases/replica/" + node
	}
	base := "/v1/nodes/" + rcmapi.PathEscape(node)
	raw, err := cl.Do(ctx, http.MethodPost, base+"/databases/root/prepare", map[string]string{"root": root})
	if err != nil {
		return "", rcmDown(err, rc)
	}
	cfgRaw, err := cl.Do(ctx, http.MethodPut, base+"/config", map[string]any{"databases": map[string]any{"root": root}})
	if err != nil {
		return "", fmt.Errorf("the folder %s is ready, but setting databases.root failed: %w", root, err)
	}
	return fmt.Sprintf("node %s: folder %s prepared and set as databases.root.\nprepare:\n%s\nconfig:\n%s", node, root, prettyJSON(raw), prettyJSON(cfgRaw)), nil
}

// enrollBody is the enroll request of the tool args.
func enrollBody(args map[string]any) map[string]any {
	body := map[string]any{"root": strings.TrimSpace(agentStr(args["root"]))}
	for _, k := range []string{"recursive", "scansync"} {
		if v, ok := args[k].(bool); ok {
			body[k] = v
		}
	}
	if t := agentStr(args["template"]); t != "" {
		body["template"] = t
	}
	return body
}

func (gt *gatedTools) clusterEnrollPreview(ctx context.Context, dbID string, args map[string]any) string {
	node, err := nodeArg(args)
	if err != nil {
		return ""
	}
	cl, _, err := gt.rcmClient("")
	if err != nil {
		return "Dry run: " + err.Error()
	}
	body := enrollBody(args)
	body["dry_run"] = true
	raw, err := cl.Do(ctx, http.MethodPost, "/v1/nodes/"+rcmapi.PathEscape(node)+"/databases/enroll", body)
	if err != nil {
		return "Dry run failed: " + err.Error()
	}
	return "Dry run (what the node would enroll):\n" + clip(prettyJSON(raw))
}

func (gt *gatedTools) clusterEnrollExec(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
	cl, rc, err := gt.rcmClient(clusterActor(args))
	if err != nil {
		return "", err
	}
	node, err := nodeArg(args)
	if err != nil {
		return "", err
	}
	raw, err := cl.Do(ctx, http.MethodPost, "/v1/nodes/"+rcmapi.PathEscape(node)+"/databases/enroll", enrollBody(args))
	if err != nil {
		return "", rcmDown(err, rc)
	}
	out, err := waitCommandText(ctx, cl, rc, "enroll on node "+node, raw)
	if err != nil {
		return "", err
	}
	return out + "\nNext: the master Firebird must restart to load the replication settings (fb_cluster_firebird_restart), then fb_cluster_publication.", nil
}

func (gt *gatedTools) clusterPublicationExec(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
	cl, rc, err := gt.rcmClient(clusterActor(args))
	if err != nil {
		return "", err
	}
	node, err := nodeArg(args)
	if err != nil {
		return "", err
	}
	path := "/v1/nodes/" + rcmapi.PathEscape(node) + "/publication/sync"
	what := "publication sync on node " + node
	if db := strings.TrimSpace(agentStr(args["db_id"])); db != "" {
		path = "/v1/nodes/" + rcmapi.PathEscape(node) + "/databases/" + rcmapi.PathEscape(db) + "/publication"
		what = "publication of " + db + " on node " + node
	}
	raw, err := cl.Do(ctx, http.MethodPost, path, map[string]any{})
	if err != nil {
		return "", rcmDown(err, rc)
	}
	return waitCommandText(ctx, cl, rc, what, raw)
}

func (gt *gatedTools) clusterFirebirdRestartExec(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
	cl, rc, err := gt.rcmClient(clusterActor(args))
	if err != nil {
		return "", err
	}
	node, err := nodeArg(args)
	if err != nil {
		return "", err
	}
	// ignore_window: the fbmcp maintenance window and the operator approval
	// are the gate here.
	raw, err := cl.Do(ctx, http.MethodPost, "/v1/nodes/"+rcmapi.PathEscape(node)+"/firebird/restart", map[string]any{"ignore_window": true})
	if err != nil {
		return "", rcmDown(err, rc)
	}
	return waitCommandText(ctx, cl, rc, "Firebird restart on node "+node, raw)
}

// reinitBody copies the reinit options of the tool args.
func reinitBody(args map[string]any) map[string]any {
	body := map[string]any{"ignore_window": true}
	if v := agentStr(args["to"]); v != "" {
		body["to"] = v
	}
	if v := agentStr(args["mode"]); v != "" {
		body["mode"] = strings.ToLower(v)
	}
	for _, k := range []string{"remove_old_replica", "allow_restart", "force", "hold_on_long_transactions"} {
		if v, ok := args[k].(bool); ok {
			body[k] = v
		}
	}
	return body
}

func (gt *gatedTools) clusterReinitExec(ctx context.Context, dbID string, args map[string]any, prog func(float64, string)) (string, error) {
	cl, rc, err := gt.rcmClient(clusterActor(args))
	if err != nil {
		return "", err
	}
	node, err := nodeArg(args)
	if err != nil {
		return "", err
	}
	body := reinitBody(args)
	db := strings.TrimSpace(agentStr(args["db_id"]))
	if db == "" || strings.EqualFold(db, "all") {
		body["node_id"] = node
		raw, err := cl.Do(ctx, http.MethodPost, "/v1/reinit", body)
		if err != nil {
			return "", rcmDown(err, rc)
		}
		return waitCommandText(ctx, cl, rc, "reinit of all databases of node "+node, raw)
	}
	body["db_id"] = db
	raw, err := cl.Do(ctx, http.MethodPost, "/v1/nodes/"+rcmapi.PathEscape(node)+"/reinit", body)
	if err != nil {
		return "", rcmDown(err, rc)
	}
	var start struct {
		ReinitID string `json:"reinit_id"`
	}
	if err := json.Unmarshal(raw, &start); err != nil || start.ReinitID == "" {
		return fmt.Sprintf("reinit of %s on node %s:\n%s", db, node, prettyJSON(raw)), nil
	}
	wctx, cancel := rcmBudget(ctx, rc)
	defer cancel()
	j, last, err := cl.WaitJob(wctx, "/v1/reinit", start.ReinitID, clusterPoll, func(j rcmapi.Job) {
		if prog != nil {
			prog(float64(j.Percent)/100, "reinit: "+j.Phase)
		}
	})
	if err != nil {
		return "", fmt.Errorf("%v; follow it with fb_cluster_status view=reinit id=%s", err, start.ReinitID)
	}
	if j.Status != "ok" {
		return "", fmt.Errorf("reinit %s of %s ended %s: %s %s\n%s", j.ID, db, j.Status, j.Code, j.Error, prettyJSON(last))
	}
	return fmt.Sprintf("reinit %s of %s on node %s: ok\n%s\nNext: fb_cluster_verify.", j.ID, db, node, prettyJSON(last)), nil
}

func registerClusterTools(server *mcp.Server, gt *gatedTools) {
	registerClusterRead(server, gt, "fb_cluster_status",
		"HQbird cluster manager (RCM) state: overview (health, cluster, nodes, databases) or one view: "+clusterViewNames()+". The RCM of this host answers; on a replica host it is stopped after the link.",
		clusterStatus)
	registerClusterRead(server, gt, "fb_cluster_probe",
		"Probe servers before fb_cluster_link: pairing state, node name, versions, certificate fingerprint and the member ids the link would give.",
		clusterProbe)
	registerClusterRead(server, gt, "fb_cluster_verify",
		"Compare COUNT(*) of every table on the master and the replica database and wait for the verdict (ok | desynced | no_tables | timeout | failed).",
		clusterVerify)

	gt.registerToolOptDB(server, policy.ToolMeta{Name: "fb_cluster_rcm_password", Tier: 1, Scope: "instance"},
		"Set a new random password for the RCM user of fbmcp (removes the first-login must_change state) and store it in the fbmcp secret; the password is never shown (audit key: db %s).",
		gt.clusterPasswordExec, nil)
	gt.registerToolOptDB(server, policy.ToolMeta{Name: "fb_cluster_link", Tier: 1, Scope: "instance"},
		"Link HQbird servers into one cluster through the RCM of this host (audit key: db %s). The request probes the hosts and shows their certificate fingerprints; the link runs only with the same fingerprints.",
		gt.clusterLinkExec, gt.clusterLinkPreview)
	gt.registerToolOptDB(server, policy.ToolMeta{Name: "fb_cluster_replica_folder", Tier: 1, Scope: "instance"},
		"Create the replica folder on a cluster node and set it as its databases.root (audit key: db %s).",
		gt.clusterReplicaFolderExec, nil)
	gt.registerToolOptDB(server, policy.ToolMeta{Name: "fb_cluster_enroll", Tier: 1, Scope: "instance"},
		"Put the databases of a folder on a cluster node under cluster management (audit key: db %s). The request shows the dry run.",
		gt.clusterEnrollExec, gt.clusterEnrollPreview)
	gt.registerToolOptDB(server, policy.ToolMeta{Name: "fb_cluster_publication", Tier: 1, Scope: "instance"},
		"Enable and sync the replication publication on a cluster node: all databases, or one db_id (audit key: db %s).",
		gt.clusterPublicationExec, nil)
	gt.registerToolOptDB(server, policy.ToolMeta{Name: "fb_cluster_firebird_restart", Tier: 2, Scope: "instance"},
		"Restart Firebird on a cluster node through the cluster node service (window key: db %s). Connections to every database of that server break.",
		gt.clusterFirebirdRestartExec, nil)
	gt.registerToolOptDB(server, policy.ToolMeta{Name: "fb_cluster_reinit", Tier: 2, Scope: "instance"},
		"Initialize or re-initialize the replica of one database (db_id) or of all databases of a master node, and wait for the end (window key: db %s). Copies the master database to the replica node.",
		gt.clusterReinitExec, nil)
}
