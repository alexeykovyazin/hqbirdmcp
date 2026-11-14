// HQbird cluster services (fb_cluster_service*, fb_cluster_switchover,
// fb_cluster_failover, fb_cluster_rejoin, fb_cluster_resume,
// fb_cluster_operation_cancel, fb_cluster_router_sync): the logical
// databases RCM moves between hosts. fbmcp calls the operator API of the RCM
// of its host; RCM runs every check and every step. The tools match the
// Services pages of the RCM UI (hqcluster3 docs/rcm.md, "The Services
// pages").
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/policy"
	"github.com/aleks/fbmcp/internal/rcmapi"
)

type clusterServiceArg struct {
	clusterArg
	View    string `json:"view,omitempty" jsonschema:"list (default) | show | master | policy | operations | operation | routers | locate | check_switchover | check_failover | check_resume"`
	Service string `json:"service,omitempty" jsonschema:"service id (show, master, policy, check_*; operations: only this service)"`
	ID      string `json:"id,omitempty" jsonschema:"operation id (view=operation)"`
	Target  string `json:"target,omitempty" jsonschema:"check_switchover, check_failover: the host the master moves to (optional for a service of two hosts)"`
	Query   string `json:"query,omitempty" jsonschema:"for view locate: an alias, a service id, a db_id, a GUID or a path"`
}

// svcView is the part of a service view (RCM service.View) the tools print.
type svcView struct {
	ServiceID   string `json:"service_id"`
	Alias       string `json:"alias"`
	RouterPort  int    `json:"router_port"`
	Epoch       int64  `json:"epoch"`
	State       string `json:"state"`
	StateReason string `json:"state_reason"`
	Master      *struct {
		Host     string `json:"host"`
		NodeID   string `json:"node_id"`
		Firebird string `json:"firebird"`
		Path     string `json:"path"`
		Online   bool   `json:"online"`
		Recorded bool   `json:"recorded"`
	} `json:"master"`
	Replicas []struct {
		Host            string `json:"host"`
		NodeID          string `json:"node_id"`
		State           string `json:"state"`
		LagSec          int64  `json:"lag_sec"`
		PendingSegments *int64 `json:"pending_segments"`
		Online          bool   `json:"online"`
	} `json:"replicas"`
	Routers []struct {
		RouterID string `json:"router_id"`
		Endpoint string `json:"endpoint"`
		Epoch    int64  `json:"epoch"`
		Active   string `json:"active"`
	} `json:"routers"`
	Problems      []rcmapi.Problem `json:"problems"`
	PendingFences []struct {
		Host      string `json:"host"`
		NodeID    string `json:"node_id"`
		Mode      string `json:"mode"`
		LastError string `json:"last_error"`
	} `json:"pending_fences"`
	Members []struct {
		Host        string `json:"host"`
		MasterNode  string `json:"master_node"`
		ReplicaNode string `json:"replica_node"`
		Path        string `json:"path"`
		Priority    int    `json:"priority"`
	} `json:"members"`
	Policy *struct {
		AutoFailover     bool   `json:"auto_failover"`
		MaxDataLossSec   int    `json:"max_data_loss_sec"`
		FailoverDelaySec int    `json:"failover_delay_sec"`
		MinIntervalSec   int    `json:"min_interval_sec"`
		FenceMode        string `json:"fence_mode"`
	} `json:"policy"`
	AllowedActions     []string `json:"allowed_actions"`
	RunningOperationID string   `json:"running_operation_id"`
	UpdatedAt          string   `json:"updated_at"`
}

// svcSummary is a service in a few lines: where the master is, who follows
// it, what is wrong, and what an operator may start now.
func svcSummary(v svcView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "service %s: %s, epoch %d", v.ServiceID, v.State, v.Epoch)
	if v.RouterPort > 0 {
		fmt.Fprintf(&b, ", router port %d", v.RouterPort)
	}
	b.WriteString("\n")
	if v.StateReason != "" {
		fmt.Fprintf(&b, "  reason: %s\n", v.StateReason)
	}
	if m := v.Master; m != nil {
		fmt.Fprintf(&b, "  master: %s (node %s)%s%s, Firebird %s, %s\n", m.Host, m.NodeID, onlineText(m.Online),
			map[bool]string{true: "", false: ", not recorded yet"}[m.Recorded], orDash(m.Firebird), m.Path)
	} else {
		b.WriteString("  master: none found\n")
	}
	for _, r := range v.Replicas {
		behind := ""
		if r.PendingSegments != nil {
			behind = fmt.Sprintf(", %d segment(s) behind", *r.PendingSegments)
		}
		fmt.Fprintf(&b, "  replica: %s (node %s)%s, %s, lag %d s%s\n", r.Host, r.NodeID, onlineText(r.Online), r.State, r.LagSec, behind)
	}
	if len(v.Replicas) == 0 {
		b.WriteString("  replica: none follows the master\n")
	}
	for _, r := range v.Routers {
		fmt.Fprintf(&b, "  router: %s at %s, epoch %d, active host %s\n", r.RouterID, orDash(r.Endpoint), r.Epoch, orDash(r.Active))
	}
	for _, f := range v.PendingFences {
		fmt.Fprintf(&b, "  old master waits for its fence: %s (node %s), mode %s%s\n", f.Host, f.NodeID, f.Mode, afterColon(f.LastError))
	}
	for _, p := range v.Problems {
		fmt.Fprintf(&b, "  problem %s%s: %s\n", p.Code, inParens(p.NodeID), p.Message)
	}
	if v.RunningOperationID != "" {
		fmt.Fprintf(&b, "  operation runs: %s (fb_cluster_service view=operation id=%s)\n", v.RunningOperationID, v.RunningOperationID)
	}
	if v.AllowedActions != nil {
		fmt.Fprintf(&b, "  allowed now: %s\n", orDash(strings.Join(v.AllowedActions, ", ")))
	}
	return b.String()
}

func onlineText(on bool) string {
	if on {
		return ""
	}
	return " OFFLINE"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func inParens(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}

func afterColon(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}

// problemsText lists the problems of a check, one per line.
func problemsText(ps []rcmapi.Problem) string {
	var b strings.Builder
	for _, p := range ps {
		fmt.Fprintf(&b, "  %s%s: %s\n", p.Code, inParens(p.NodeID), p.Message)
	}
	return b.String()
}

// operationText is an operation with its steps and the line of the point of
// no return.
func operationText(op rcmapi.Operation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s of service %s: %s", op.Kind, op.ID, op.ServiceID, op.Status)
	if op.FromHost != "" || op.ToHost != "" {
		fmt.Fprintf(&b, ", %s -> %s", op.FromHost, op.ToHost)
	}
	if op.Epoch > 0 {
		fmt.Fprintf(&b, ", epoch %d", op.Epoch)
	}
	if op.Actor != "" {
		fmt.Fprintf(&b, ", started by %s", op.Actor)
	}
	b.WriteString("\n")
	if op.Error != "" {
		fmt.Fprintf(&b, "error: %s\n", op.Error)
	}
	for _, s := range op.Steps {
		if s.NoReturn {
			b.WriteString("  ---- point of no return: from here the operation cannot be cancelled ----\n")
		}
		took := ""
		if s.StartedAt != nil && s.EndedAt != nil {
			took = fmt.Sprintf(" (%s)", s.EndedAt.Sub(*s.StartedAt).Round(100_000_000))
		}
		fmt.Fprintf(&b, "  %-7s %s%s%s\n", s.State, s.ID, took, afterColon(s.Detail))
	}
	for _, w := range op.Warnings {
		fmt.Fprintf(&b, "warning: %s\n", w)
	}
	return b.String()
}

// operationNext says what an ended operation left to do.
func operationNext(op rcmapi.Operation) string {
	switch op.Status {
	case "ok":
		if op.Kind == "failover" {
			return "Next: the service works without a replica. When the old host is back, RCM fences it (fb_cluster_service view=show); then fb_cluster_rejoin."
		}
		return "Next: fb_cluster_service view=show to see the master and the replica."
	case "cancelled":
		return "Next: the service stayed on the old host."
	case "failed":
		failed := ""
		for _, s := range op.Steps {
			if s.State == "failed" {
				failed = s.ID
			}
		}
		switch {
		case op.Kind == "rejoin" || strings.HasPrefix(failed, "rejoin_"):
			return "Next: the service works without a replica. Fix the cause, then fb_cluster_rejoin."
		case op.PastNoReturn:
			return "Next: the service is failed. Check that the new master serves, then fb_cluster_resume."
		default:
			return "Next: the service stayed on the old host. Fix the cause and repeat."
		}
	}
	return ""
}

// refusal turns a refused check or start into the tool error: the problems
// of RCM, and the operation that runs on the service.
func refusal(what string, err error) error {
	r, ok := rcmapi.RefusalOf(err)
	if !ok {
		return err
	}
	msg := fmt.Sprintf("%s refused by RCM: %v\n%s", what, err, problemsText(r.Problems))
	if len(r.DataLoss) > 0 {
		msg += "data loss report:\n" + prettyJSON(r.DataLoss) + "\n"
	}
	if r.OperationID != "" {
		msg += fmt.Sprintf("Next: an operation runs on the service: fb_cluster_service view=operation id=%s", r.OperationID)
	}
	return fmt.Errorf("%s", strings.TrimRight(msg, "\n"))
}

func servicePath(id string, tail string) string {
	return "/v1/services/" + rcmapi.PathEscape(id) + tail
}

// needService reads the service id of a Tier 0 view.
func needService(a clusterServiceArg, view string) (string, error) {
	id := strings.TrimSpace(a.Service)
	if id == "" {
		return "", fmt.Errorf("view %s needs service (fb_cluster_service lists them)", view)
	}
	return id, nil
}

// checkText renders the answer of a check route of RCM.
func checkText(what string, raw json.RawMessage) string {
	var c struct {
		OK          bool             `json:"ok"`
		FromHost    string           `json:"from_host"`
		ToHost      string           `json:"to_host"`
		From        string           `json:"from"`
		To          string           `json:"to"`
		Epoch       int64            `json:"epoch"`
		Problems    []rcmapi.Problem `json:"problems"`
		Notes       []string         `json:"notes"`
		DataLoss    json.RawMessage  `json:"data_loss"`
		OperationID string           `json:"operation_id"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return what + ":\n" + prettyJSON(raw)
	}
	var b strings.Builder
	if c.OK {
		fmt.Fprintf(&b, "%s: the checks pass", what)
	} else {
		fmt.Fprintf(&b, "%s: REFUSED by the checks, nothing was changed", what)
	}
	switch {
	case c.FromHost != "" || c.ToHost != "":
		fmt.Fprintf(&b, " (master %s -> %s, epoch %d)", orDash(c.FromHost), orDash(c.ToHost), c.Epoch)
	case c.To != "":
		fmt.Fprintf(&b, " (state %s -> %s)", c.From, c.To)
	}
	b.WriteString("\n")
	b.WriteString(problemsText(c.Problems))
	for _, n := range c.Notes {
		fmt.Fprintf(&b, "  note: %s\n", n)
	}
	if len(c.DataLoss) > 0 && string(c.DataLoss) != "null" {
		fmt.Fprintf(&b, "what a failover may lose (the master may have written more after its last stats):\n%s\n", prettyJSON(c.DataLoss))
	}
	if c.OperationID != "" {
		fmt.Fprintf(&b, "an operation runs on the service: fb_cluster_service view=operation id=%s\n", c.OperationID)
	}
	return b.String()
}

// clusterService is the Tier 0 tool of the services: views and checks.
func clusterService(ctx context.Context, cl *rcmapi.Client, _ config.RCM, a clusterServiceArg) (string, error) {
	view := strings.ToLower(strings.TrimSpace(a.View))
	if view == "" {
		view = "list"
	}
	target := map[string]any{}
	if t := strings.TrimSpace(a.Target); t != "" {
		target["target_host"] = t
	}
	switch view {
	case "list":
		raw, err := cl.Do(ctx, http.MethodGet, "/v1/services", nil)
		if err != nil {
			return "", err
		}
		var doc struct {
			Services []svcView `json:"services"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return "GET /v1/services\n" + prettyJSON(raw), nil
		}
		if len(doc.Services) == 0 {
			return "RCM has no service yet.\nNext: fb_cluster_service_put defines one (hosts, nodes, the path of the database on each host).", nil
		}
		var b strings.Builder
		for _, v := range doc.Services {
			b.WriteString(svcSummary(v))
		}
		b.WriteString("\nNext: view=show service=<id> for the hosts, the rule and the history; view=check_switchover before fb_cluster_switchover.")
		return b.String(), nil
	case "show", "master", "policy":
		id, err := needService(a, view)
		if err != nil {
			return "", err
		}
		tail := map[string]string{"show": "", "master": "/master", "policy": "/policy"}[view]
		raw, err := cl.Do(ctx, http.MethodGet, servicePath(id, tail), nil)
		if err != nil {
			return "", err
		}
		head := ""
		var v svcView
		if view != "policy" && json.Unmarshal(raw, &v) == nil && v.ServiceID != "" {
			head = svcSummary(v) + "\n"
		}
		return fmt.Sprintf("%sGET %s\n%s", head, servicePath(id, tail), prettyJSON(raw)), nil
	case "operations":
		path := "/v1/operations"
		if id := strings.TrimSpace(a.Service); id != "" {
			path += "?service=" + url.QueryEscape(id)
		}
		raw, err := cl.Do(ctx, http.MethodGet, path, nil)
		if err != nil {
			return "", err
		}
		var doc struct {
			Operations []struct {
				rcmapi.Operation
				CreatedAt string `json:"created_at"`
			} `json:"operations"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return "GET " + path + "\n" + prettyJSON(raw), nil
		}
		ops := doc.Operations
		sort.SliceStable(ops, func(i, j int) bool { return ops[i].CreatedAt > ops[j].CreatedAt })
		var b strings.Builder
		fmt.Fprintf(&b, "%d operation(s), newest first (RCM keeps the last 100)\n", len(ops))
		for i, op := range ops {
			if i == 30 {
				fmt.Fprintf(&b, "… %d older\n", len(ops)-i)
				break
			}
			fmt.Fprintf(&b, "%s  %-10s %-9s %s  %s -> %s  epoch %d  by %s  %s%s\n", op.CreatedAt, op.Kind, op.Status, op.ServiceID,
				orDash(op.FromHost), orDash(op.ToHost), op.Epoch, orDash(op.Actor), op.ID, afterColon(op.Error))
		}
		b.WriteString("Next: view=operation id=<id> for the steps.")
		return b.String(), nil
	case "operation":
		id := strings.TrimSpace(a.ID)
		if id == "" {
			return "", fmt.Errorf("view operation needs id (view=operations lists them)")
		}
		op, _, err := cl.Operation(ctx, id)
		if err != nil {
			return "", err
		}
		out := operationText(op)
		if next := operationNext(op); next != "" {
			out += next
		} else if op.Running() && !op.PastNoReturn {
			out += "Next: it runs; fb_cluster_operation_cancel stops it before the point of no return."
		}
		return out, nil
	case "routers":
		raw, err := cl.Do(ctx, http.MethodGet, "/v1/routers", nil)
		if err != nil {
			return "", err
		}
		return "GET /v1/routers\n" + prettyJSON(raw) + "\nNext: fb_cluster_router_sync sends a new table version to every router.", nil
	case "locate":
		q := strings.TrimSpace(a.Query)
		if q == "" {
			return "", fmt.Errorf("view locate needs query: an alias, a service id, a db_id, a GUID or a path")
		}
		raw, err := cl.Do(ctx, http.MethodGet, "/v1/locate?db="+url.QueryEscape(q), nil)
		if err != nil {
			return "", err
		}
		return "GET /v1/locate (the master first)\n" + prettyJSON(raw), nil
	case "check_switchover", "check_failover", "check_resume":
		id, err := needService(a, view)
		if err != nil {
			return "", err
		}
		tail, what := "/switchover/check", "switchover of "+id
		switch view {
		case "check_failover":
			tail, what = "/failover/check", "failover of "+id
		case "check_resume":
			tail, what = "/resume?check=1", "resume of "+id
		}
		var body any
		if view != "check_resume" {
			body = target
		}
		raw, err := cl.Do(ctx, http.MethodPost, servicePath(id, tail), body)
		if err != nil {
			return "", refusal(what, err)
		}
		return checkText(what, raw), nil
	}
	return "", fmt.Errorf("unknown view %q; use one of: list, show, master, policy, operations, operation, routers, locate, check_switchover, check_failover, check_resume", a.View)
}

// --- Tier 1/2 executors ---

func serviceArg(args map[string]any) (string, error) {
	id := strings.TrimSpace(agentStr(args["service"]))
	if id == "" {
		return "", fmt.Errorf("args.service is required (fb_cluster_service lists the services)")
	}
	return id, nil
}

// newRequestID is the request id of a start: RCM answers the same operation
// to a repeated request with it.
func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "fbmcp-" + hex.EncodeToString(b)
}

// intArgs copies the whole-number args that are set into a request body.
func intArgs(args map[string]any, body map[string]any, names ...string) {
	for _, n := range names {
		if v, ok := agentInt(args[n]); ok {
			body[n] = v
		}
	}
}

// runOperation starts an operation of a service and, unless args.wait is
// false, waits for its end and prints its steps.
func (gt *gatedTools) runOperation(ctx context.Context, cl *rcmapi.Client, rc config.RCM, what, path string, body map[string]any, args map[string]any, prog func(float64, string)) (string, error) {
	raw, err := cl.Do(ctx, http.MethodPost, path, body)
	if err != nil {
		return "", rcmDown(refusal(what, err), rc)
	}
	var start struct {
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(raw, &start); err != nil || start.OperationID == "" {
		return fmt.Sprintf("%s:\n%s", what, prettyJSON(raw)), nil
	}
	if w, ok := args["wait"].(bool); ok && !w {
		return fmt.Sprintf("%s started: operation %s.\nNext: fb_cluster_service view=operation id=%s", what, start.OperationID, start.OperationID), nil
	}
	wctx, cancel := rcmBudget(ctx, rc)
	defer cancel()
	op, err := cl.WaitOperation(wctx, start.OperationID, clusterPoll, func(op rcmapi.Operation) {
		if prog != nil {
			prog(op.Progress(), op.Kind+": "+op.ActiveStep())
		}
	})
	if err != nil {
		return "", err
	}
	out := operationText(op) + operationNext(op)
	if op.Status != "ok" {
		return "", fmt.Errorf("%s", out)
	}
	return out, nil
}

// checkPreview is the preview of a start: the answer of the check route.
func (gt *gatedTools) checkPreview(ctx context.Context, args map[string]any, tail, what string) string {
	cl, rc, err := gt.rcmClient(clusterActor(args))
	if err != nil {
		return "Check failed: " + err.Error()
	}
	id, err := serviceArg(args)
	if err != nil {
		return "Check failed: " + err.Error()
	}
	var body any
	if !strings.HasPrefix(tail, "/resume") {
		b := map[string]any{}
		if t := strings.TrimSpace(agentStr(args["target_host"])); t != "" {
			b["target_host"] = t
		}
		body = b
	}
	raw, err := cl.Do(ctx, http.MethodPost, servicePath(id, tail), body)
	if err != nil {
		return "Check failed: " + rcmDown(refusal(what+" of "+id, err), rc).Error() + " (the request will be refused when confirmed)"
	}
	return checkText(what+" of "+id, raw)
}

func (gt *gatedTools) clusterSwitchoverPreview(ctx context.Context, _ string, args map[string]any) string {
	return gt.checkPreview(ctx, args, "/switchover/check", "switchover") +
		"A switchover shuts the database of the old master down until the new master takes over: applications cannot write during that time. No data is lost. Then the old master becomes a replica of the new one."
}

func (gt *gatedTools) clusterSwitchoverExec(ctx context.Context, _ string, args map[string]any, prog func(float64, string)) (string, error) {
	cl, rc, err := gt.rcmClient(clusterActor(args))
	if err != nil {
		return "", err
	}
	id, err := serviceArg(args)
	if err != nil {
		return "", err
	}
	body := map[string]any{"request_id": newRequestID()}
	if t := strings.TrimSpace(agentStr(args["target_host"])); t != "" {
		body["target_host"] = t
	}
	intArgs(args, body, "quiesce_timeout_sec", "apply_timeout_sec", "settle_sec", "force_sec")
	return gt.runOperation(ctx, cl, rc, "switchover of "+id, servicePath(id, "/switchover"), body, args, prog)
}

func (gt *gatedTools) clusterFailoverPreview(ctx context.Context, _ string, args map[string]any) string {
	return gt.checkPreview(ctx, args, "/failover/check", "failover") +
		"A failover makes the replica the master with the segments it has: what the lost master did not ship is lost. RCM fences the old master when its node answers again."
}

func (gt *gatedTools) clusterFailoverExec(ctx context.Context, _ string, args map[string]any, prog func(float64, string)) (string, error) {
	cl, rc, err := gt.rcmClient(clusterActor(args))
	if err != nil {
		return "", err
	}
	id, err := serviceArg(args)
	if err != nil {
		return "", err
	}
	if ok, _ := args["accept_data_loss"].(bool); !ok {
		return "", fmt.Errorf("failover of %s: args.accept_data_loss must be true: the operator accepts that the data the master did not ship is lost (fb_cluster_service view=check_failover shows what)", id)
	}
	body := map[string]any{"request_id": newRequestID(), "accept_data_loss": true}
	if t := strings.TrimSpace(agentStr(args["target_host"])); t != "" {
		body["target_host"] = t
	}
	intArgs(args, body, "apply_timeout_sec")
	return gt.runOperation(ctx, cl, rc, "failover of "+id, servicePath(id, "/failover"), body, args, prog)
}

func (gt *gatedTools) clusterRejoinExec(ctx context.Context, _ string, args map[string]any, prog func(float64, string)) (string, error) {
	cl, rc, err := gt.rcmClient(clusterActor(args))
	if err != nil {
		return "", err
	}
	id, err := serviceArg(args)
	if err != nil {
		return "", err
	}
	body := map[string]any{"request_id": newRequestID()}
	if h := strings.TrimSpace(agentStr(args["host"])); h != "" {
		body["host"] = h
	}
	return gt.runOperation(ctx, cl, rc, "rejoin of "+id, servicePath(id, "/rejoin"), body, args, prog)
}

func (gt *gatedTools) clusterResumePreview(ctx context.Context, _ string, args map[string]any) string {
	return gt.checkPreview(ctx, args, "/resume?check=1", "resume")
}

func (gt *gatedTools) clusterResumeExec(ctx context.Context, _ string, args map[string]any, _ func(float64, string)) (string, error) {
	cl, rc, err := gt.rcmClient(clusterActor(args))
	if err != nil {
		return "", err
	}
	id, err := serviceArg(args)
	if err != nil {
		return "", err
	}
	raw, err := cl.Do(ctx, http.MethodPost, servicePath(id, "/resume"), nil)
	if err != nil {
		return "", rcmDown(refusal("resume of "+id, err), rc)
	}
	return checkText("resume of "+id, raw) + "Next: fb_cluster_service view=show service=" + id, nil
}

func (gt *gatedTools) clusterOperationCancelExec(ctx context.Context, _ string, args map[string]any, _ func(float64, string)) (string, error) {
	cl, rc, err := gt.rcmClient(clusterActor(args))
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(agentStr(args["id"]))
	if id == "" {
		return "", fmt.Errorf("args.id is required: the operation id (fb_cluster_service view=operations)")
	}
	if _, err := cl.Do(ctx, http.MethodPost, "/v1/operations/"+rcmapi.PathEscape(id)+"/cancel", nil); err != nil {
		if rcmapi.IsCode(err, "past_no_return") {
			return "", fmt.Errorf("%v\nNext: the operation is past the point of no return and goes on to its end: fb_cluster_service view=operation id=%s", err, id)
		}
		return "", rcmDown(err, rc)
	}
	return fmt.Sprintf("cancel of operation %s requested: RCM stops it at its next check and brings the old master online again.\nNext: fb_cluster_service view=operation id=%s", id, id), nil
}

func (gt *gatedTools) clusterRouterSyncExec(ctx context.Context, _ string, args map[string]any, _ func(float64, string)) (string, error) {
	cl, rc, err := gt.rcmClient(clusterActor(args))
	if err != nil {
		return "", err
	}
	router := strings.TrimSpace(agentStr(args["router"]))
	if router == "" {
		// The table is one for all routers: any router of rcm.json names the sync.
		raw, err := cl.Do(ctx, http.MethodGet, "/v1/routers", nil)
		if err != nil {
			return "", rcmDown(err, rc)
		}
		var doc struct {
			Routers []struct {
				RouterID   string `json:"router_id"`
				Configured bool   `json:"configured"`
			} `json:"routers"`
		}
		_ = json.Unmarshal(raw, &doc)
		for _, r := range doc.Routers {
			if r.Configured {
				router = r.RouterID
				break
			}
		}
		if router == "" {
			return "", fmt.Errorf("RCM has no router in rcm.json: nothing to send the table to")
		}
	}
	raw, err := cl.Do(ctx, http.MethodPost, "/v1/routers/"+rcmapi.PathEscape(router)+"/sync", nil)
	if err != nil {
		return "", rcmDown(err, rc)
	}
	return fmt.Sprintf("new routing table version (every router takes it):\n%s\nNext: fb_cluster_service view=routers shows which routers have it.", prettyJSON(raw)), nil
}

// policyBody is the rule the request asks for: the rule RCM has now, with
// the args that are set put over it.
func policyBody(cur map[string]any, args map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"auto_failover", "max_data_loss_sec", "failover_delay_sec", "min_interval_sec", "fence_mode"} {
		if v, ok := cur[k]; ok {
			out[k] = v
		}
	}
	if v, ok := args["auto_failover"].(bool); ok {
		out["auto_failover"] = v
	}
	intArgs(args, out, "max_data_loss_sec", "failover_delay_sec", "min_interval_sec")
	if m := strings.ToLower(strings.TrimSpace(agentStr(args["fence_mode"]))); m != "" {
		out["fence_mode"] = m
	}
	return out
}

// currentPolicy reads the rule of a service.
func currentPolicy(ctx context.Context, cl *rcmapi.Client, id string) (map[string]any, json.RawMessage, error) {
	raw, err := cl.Do(ctx, http.MethodGet, servicePath(id, "/policy"), nil)
	if err != nil {
		return nil, nil, err
	}
	var doc struct {
		Policy map[string]any `json:"policy"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, raw, err
	}
	return doc.Policy, raw, nil
}

func (gt *gatedTools) clusterServicePolicyPreview(ctx context.Context, _ string, args map[string]any) string {
	cl, rc, err := gt.rcmClient(clusterActor(args))
	if err != nil {
		return "The rule cannot be read: " + err.Error()
	}
	id, err := serviceArg(args)
	if err != nil {
		return "The rule cannot be read: " + err.Error()
	}
	cur, _, err := currentPolicy(ctx, cl, id)
	if err != nil {
		return "The rule cannot be read: " + rcmDown(err, rc).Error()
	}
	now, _ := json.Marshal(cur)
	next, _ := json.Marshal(policyBody(cur, args))
	return fmt.Sprintf("Rule of the automatic failover of %s\n  now:  %s\n  then: %s\nRCM starts a failover by itself only when the master is silent, a direct call fails, most routers do not reach it and the replica has not seen it. After one automatic failover the rule turns itself off.", id, now, next)
}

func (gt *gatedTools) clusterServicePolicyExec(ctx context.Context, _ string, args map[string]any, _ func(float64, string)) (string, error) {
	cl, rc, err := gt.rcmClient(clusterActor(args))
	if err != nil {
		return "", err
	}
	id, err := serviceArg(args)
	if err != nil {
		return "", err
	}
	cur, _, err := currentPolicy(ctx, cl, id)
	if err != nil {
		return "", rcmDown(err, rc)
	}
	raw, err := cl.Do(ctx, http.MethodPut, servicePath(id, "/policy"), policyBody(cur, args))
	if err != nil {
		if rcmapi.IsCode(err, "no_witnesses") {
			return "", fmt.Errorf("%v\nNext: an automatic failover needs witnesses: a router_port of the service (fb_cluster_service_put) and routers in rcm.json (fb_cluster_service view=routers)", err)
		}
		return "", rcmDown(err, rc)
	}
	return fmt.Sprintf("rule of the automatic failover of %s saved:\n%s", id, prettyJSON(raw)), nil
}

// serviceDefinition is the body of PUT /v1/services/{id} from the args: no
// policy (RCM keeps the rule of an existing service).
func serviceDefinition(args map[string]any) (map[string]any, error) {
	members, ok := args["members"].([]any)
	if !ok || len(members) == 0 {
		return nil, fmt.Errorf("args.members must be a list with one object per host: {host, master_node, replica_node, path, priority?}")
	}
	body := map[string]any{"members": members}
	if a := strings.TrimSpace(agentStr(args["alias"])); a != "" {
		body["alias"] = a
	}
	if p, ok := agentInt(args["router_port"]); ok {
		body["router_port"] = p
	}
	return body, nil
}

// definitionErrors lists the errors of a refused definition one by one.
func definitionErrors(err error) string {
	var e *rcmapi.Error
	if !errors.As(err, &e) {
		return err.Error()
	}
	var doc struct {
		Errors []string `json:"errors"`
	}
	if json.Unmarshal(e.Body, &doc) == nil && len(doc.Errors) > 0 {
		return fmt.Sprintf("rcm %d %s:\n  %s", e.Status, e.Code, strings.Join(doc.Errors, "\n  "))
	}
	return err.Error()
}

// putHeaders are the preconditions of a definition: new = only create,
// if_match = the updated_at the caller read (fb_cluster_service view=show).
func putHeaders(args map[string]any) http.Header {
	h := http.Header{}
	if n, _ := args["new"].(bool); n {
		h.Set("If-None-Match", "*")
	}
	if m := strings.TrimSpace(agentStr(args["if_match"])); m != "" {
		h.Set("If-Match", m)
	}
	return h
}

func (gt *gatedTools) clusterServicePutPreview(ctx context.Context, _ string, args map[string]any) string {
	cl, rc, err := gt.rcmClient(clusterActor(args))
	if err != nil {
		return "Check failed: " + err.Error()
	}
	id, err := serviceArg(args)
	if err != nil {
		return "Check failed: " + err.Error()
	}
	body, err := serviceDefinition(args)
	if err != nil {
		return "Check failed: " + err.Error() + " (the request will be refused when confirmed)"
	}
	raw, err := cl.DoHeader(ctx, http.MethodPut, servicePath(id, "?check=1"), body, putHeaders(args))
	if err != nil {
		return "RCM refuses this definition (the request will be refused when confirmed): " + definitionErrors(rcmDown(err, rc))
	}
	var doc struct {
		Exists bool `json:"exists"`
	}
	_ = json.Unmarshal(raw, &doc)
	if doc.Exists {
		return "RCM accepts the definition: it replaces the hosts, the alias and the router port of service " + id + "; the rule of the automatic failover, the epoch and the history stay."
	}
	return "RCM accepts the definition: service " + id + " is new. RCM records its master when exactly one host reports the file as a live master."
}

func (gt *gatedTools) clusterServicePutExec(ctx context.Context, _ string, args map[string]any, _ func(float64, string)) (string, error) {
	cl, rc, err := gt.rcmClient(clusterActor(args))
	if err != nil {
		return "", err
	}
	id, err := serviceArg(args)
	if err != nil {
		return "", err
	}
	body, err := serviceDefinition(args)
	if err != nil {
		return "", err
	}
	raw, err := cl.DoHeader(ctx, http.MethodPut, servicePath(id, ""), body, putHeaders(args))
	if err != nil {
		switch {
		case rcmapi.IsCode(err, "conflict"):
			return "", fmt.Errorf("%v\nNext: read the service again (fb_cluster_service view=show service=%s) and repeat with its updated_at as if_match", err, id)
		case rcmapi.IsCode(err, "invalid_service"):
			return "", fmt.Errorf("%s", definitionErrors(err))
		}
		return "", rcmDown(refusal("definition of "+id, err), rc)
	}
	var v svcView
	head := ""
	if json.Unmarshal(raw, &v) == nil && v.ServiceID != "" {
		head = svcSummary(v)
	}
	return fmt.Sprintf("service %s saved.\n%sNext: fb_cluster_service view=show service=%s; fb_cluster_service_policy for the automatic failover.", id, head, id), nil
}

func (gt *gatedTools) clusterServiceDeletePreview(ctx context.Context, _ string, args map[string]any) string {
	cl, rc, err := gt.rcmClient(clusterActor(args))
	if err != nil {
		return "The service cannot be read: " + err.Error()
	}
	id, err := serviceArg(args)
	if err != nil {
		return "The service cannot be read: " + err.Error()
	}
	raw, err := cl.Do(ctx, http.MethodGet, servicePath(id, ""), nil)
	if err != nil {
		return "The service cannot be read: " + rcmDown(err, rc).Error() + " (the request will be refused when confirmed)"
	}
	var v svcView
	_ = json.Unmarshal(raw, &v)
	s := svcSummary(v) +
		"What stays: the databases and their files, the replication between the hosts, the roles of the nodes, the alias in databases.conf.\n" +
		"What is lost: the history of the masters and the epoch, the rule of the automatic failover, and the port on the routers: applications that connect through a router lose the connection.\n"
	if len(v.PendingFences) > 0 {
		s += "WARNING: an old master of this service still waits for its fence. After the delete RCM does not fence it: when its host comes back, its database accepts writes as a second master.\n"
	}
	return s
}

func (gt *gatedTools) clusterServiceDeleteExec(ctx context.Context, _ string, args map[string]any, _ func(float64, string)) (string, error) {
	cl, rc, err := gt.rcmClient(clusterActor(args))
	if err != nil {
		return "", err
	}
	id, err := serviceArg(args)
	if err != nil {
		return "", err
	}
	if _, err := cl.Do(ctx, http.MethodDelete, servicePath(id, ""), nil); err != nil {
		return "", rcmDown(refusal("delete of "+id, err), rc)
	}
	return "service " + id + " deleted from RCM. The databases, the replication and the roles of the nodes stay as they are.", nil
}

func registerClusterServiceTools(server *mcp.Server, gt *gatedTools) {
	registerClusterRead(server, gt, "fb_cluster_service",
		"HQbird cluster services (a logical database RCM moves between hosts): list (default: master, replicas, routers, problems, what may start now), show, master, policy, operations, operation (its steps), routers, locate (query), and the checks that change nothing: check_switchover, check_failover, check_resume.",
		clusterService)

	gt.registerToolOptDB(server, policy.ToolMeta{Name: "fb_cluster_service_put", Tier: 1, Scope: "instance"},
		"Create a cluster service or replace its definition: hosts with their nodes and the path of the database, alias, router port (audit key: db %s). The request shows whether RCM accepts the definition.",
		gt.clusterServicePutExec, gt.clusterServicePutPreview)
	gt.registerToolOptDB(server, policy.ToolMeta{Name: "fb_cluster_service_policy", Tier: 1, Scope: "instance"},
		"Change the rule of the automatic failover of a cluster service; an arg that is left out keeps its value (audit key: db %s). The request shows the rule now and then.",
		gt.clusterServicePolicyExec, gt.clusterServicePolicyPreview)
	gt.registerToolOptDB(server, policy.ToolMeta{Name: "fb_cluster_resume", Tier: 1, Scope: "instance"},
		"Leave the failed state of a cluster service once its master serves again (audit key: db %s). The request shows the checks of RCM.",
		gt.clusterResumeExec, gt.clusterResumePreview)
	gt.registerToolOptDB(server, policy.ToolMeta{Name: "fb_cluster_operation_cancel", Tier: 1, Scope: "instance"},
		"Cancel a running switchover or failover before its point of no return: the old master is brought online again and the service stays on it (audit key: db %s).",
		gt.clusterOperationCancelExec, nil)
	gt.registerToolOptDB(server, policy.ToolMeta{Name: "fb_cluster_router_sync", Tier: 1, Scope: "instance"},
		"Send a new version of the routing table to every router: each one writes its HAProxy file again and applies the states (audit key: db %s).",
		gt.clusterRouterSyncExec, nil)
	gt.registerToolOptDB(server, policy.ToolMeta{Name: "fb_cluster_switchover", Tier: 2, Scope: "instance"},
		"Planned switchover of a cluster service: the master moves to another host with no data loss, then the old master becomes its replica; waits for the end (window key: db %s). Applications cannot write while the old master is shut down. The request shows the checks of RCM.",
		gt.clusterSwitchoverExec, gt.clusterSwitchoverPreview)
	gt.registerToolOptDB(server, policy.ToolMeta{Name: "fb_cluster_failover", Tier: 2, Scope: "instance"},
		"Failover of a cluster service whose master host is lost: a replica becomes the master with the segments it has; what the master did not ship is LOST (window key: db %s). The request shows the checks of RCM and the data loss report.",
		gt.clusterFailoverExec, gt.clusterFailoverPreview)
	gt.registerToolOptDB(server, policy.ToolMeta{Name: "fb_cluster_rejoin", Tier: 2, Scope: "instance"},
		"Rejoin a host of a cluster service as the replica of the master (after a failover, or a switchover whose rejoin failed): RCM copies the master database into the file of that host; the old file is kept as <name>.pre-reinit-<time>.fdb (window key: db %s).",
		gt.clusterRejoinExec, nil)
	gt.registerToolOptDB(server, policy.ToolMeta{Name: "fb_cluster_service_delete", Tier: 2, Scope: "instance"},
		"Delete a cluster service from RCM: only the record of RCM goes; the routers close its port (window key: db %s). The request shows what stays and what is lost.",
		gt.clusterServiceDeleteExec, gt.clusterServiceDeletePreview)
}
