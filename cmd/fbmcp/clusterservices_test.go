package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/policy"
)

// stubServices is the part of the RCM operator API the service tools call:
// one service "shop" on the hosts db1 (master) and db2.
type stubServices struct {
	mu sync.Mutex
	// state of the service and what its checks answer.
	state    string
	problems []map[string]any
	// running: an operation runs; polls until it ends.
	opPolls  int
	opStatus string // the status the operation ends with
	opKind   string
	// what the tools sent.
	starts  []map[string]any
	headers []http.Header
	policy  map[string]any
	puts    []map[string]any
	deleted []string
	cancels []string
	syncs   []string
	resumed int
}

func newStubServices() *stubServices {
	return &stubServices{state: "ok", opStatus: "ok",
		policy: map[string]any{"auto_failover": false, "max_data_loss_sec": 30.0, "failover_delay_sec": 90.0, "min_interval_sec": 3600.0, "fence_mode": "route"}}
}

func (s *stubServices) view() map[string]any {
	v := map[string]any{
		"service_id": "shop", "alias": "shop", "router_port": 3051, "epoch": 7, "state": s.state,
		"master":          map[string]any{"host": "db1", "node_id": "db1-m", "firebird": "db1.lan:3050", "path": "/data/shop.fdb", "online": true, "recorded": true},
		"replicas":        []any{map[string]any{"host": "db2", "node_id": "db2-r", "state": "IN_SYNC", "lag_sec": 0, "online": true, "pending_segments": 0}},
		"routers":         []any{map[string]any{"router_id": "app1-router", "endpoint": "app1.lan:3051", "epoch": 7, "active": "db1"}},
		"allowed_actions": []any{"switchover", "failover", "rejoin"},
		"updated_at":      "2026-11-14T20:29:01.42Z",
	}
	if s.state == "failed" {
		v["state_reason"] = "the Firebird of the master does not answer"
		v["allowed_actions"] = []any{"failover", "rejoin", "resume"}
	}
	return v
}

func (s *stubServices) operation() map[string]any {
	status := "running"
	steps := []any{
		map[string]any{"id": "check", "label": "CHECKS", "state": "done", "started_at": "2026-11-14T20:00:00Z", "ended_at": "2026-11-14T20:00:01Z"},
		map[string]any{"id": "quiesce", "label": "QUIESCE", "state": "active", "detail": "waiting"},
		map[string]any{"id": "turn_to_normal", "label": "TURN TO NORMAL", "state": "pending", "no_return": true},
	}
	op := map[string]any{"operation_id": "op-1", "kind": s.opKind, "service_id": "shop", "from_host": "db1", "to_host": "db2", "epoch": 8, "actor": "fbmcp:tester"}
	if s.opPolls >= 2 {
		status = s.opStatus
		last := "done"
		if status != "ok" {
			last = "failed"
			op["error"] = "the replica did not apply the last segment"
		}
		steps = []any{
			map[string]any{"id": "check", "label": "CHECKS", "state": "done"},
			map[string]any{"id": "quiesce", "label": "QUIESCE", "state": "done", "detail": "last archived segment 42"},
			map[string]any{"id": "turn_to_normal", "label": "TURN TO NORMAL", "state": last, "no_return": true},
		}
		op["warnings"] = []any{"warning: a note"}
	}
	s.opPolls++
	op["status"], op["steps"] = status, steps
	return op
}

func (s *stubServices) handler() http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	body := func(r *http.Request) map[string]any {
		m := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&m)
		return m
	}
	check := func(w http.ResponseWriter, extra map[string]any) {
		out := map[string]any{"service_id": "shop", "ok": len(s.problems) == 0, "from_host": "db1", "to_host": "db2", "epoch": 7, "problems": s.problems}
		if s.problems == nil {
			out["problems"] = []any{}
		}
		for k, v := range extra {
			out[k] = v
		}
		reply(w, 200, out)
	}
	start := func(kind string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			defer s.mu.Unlock()
			b := body(r)
			s.starts = append(s.starts, b)
			if len(s.problems) > 0 {
				reply(w, 409, map[string]any{"error": map[string]any{"code": kind + "_refused", "message": s.problems[0]["message"]},
					"problems": s.problems, "operation_id": "op-running"})
				return
			}
			s.opKind, s.opPolls = kind, 0
			reply(w, 202, map[string]any{"operation_id": "op-1"})
		}
	}
	mux.HandleFunc("GET /v1/services", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"services": []any{s.view()}})
	})
	mux.HandleFunc("GET /v1/services/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "shop" {
			reply(w, 404, map[string]any{"error": map[string]any{"code": "service_not_found", "message": "no service " + r.PathValue("id")}})
			return
		}
		reply(w, 200, s.view())
	})
	mux.HandleFunc("GET /v1/services/{id}/policy", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		reply(w, 200, map[string]any{"service_id": "shop", "policy": s.policy})
	})
	mux.HandleFunc("PUT /v1/services/{id}/policy", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.policy = body(r)
		reply(w, 200, map[string]any{"service_id": "shop", "policy": s.policy})
	})
	mux.HandleFunc("POST /v1/services/{id}/switchover/check", func(w http.ResponseWriter, r *http.Request) { check(w, nil) })
	mux.HandleFunc("POST /v1/services/{id}/failover/check", func(w http.ResponseWriter, r *http.Request) {
		check(w, map[string]any{"data_loss": map[string]any{"master_last_segment": 50, "received_segment": 47, "lost_segments_at_least": 3}})
	})
	mux.HandleFunc("POST /v1/services/{id}/switchover", start("switchover"))
	mux.HandleFunc("POST /v1/services/{id}/failover", start("failover"))
	mux.HandleFunc("POST /v1/services/{id}/rejoin", start("rejoin"))
	mux.HandleFunc("POST /v1/services/{id}/resume", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.URL.Query().Get("check") == "1" {
			reply(w, 200, map[string]any{"service_id": "shop", "ok": len(s.problems) == 0, "from": "failed", "to": "ok", "problems": s.problems})
			return
		}
		if len(s.problems) > 0 {
			reply(w, 409, map[string]any{"error": map[string]any{"code": "resume_refused", "message": s.problems[0]["message"]}, "problems": s.problems})
			return
		}
		s.resumed++
		s.state = "ok"
		reply(w, 200, map[string]any{"service_id": "shop", "ok": true, "from": "failed", "to": "ok", "epoch": 7, "problems": []any{}})
	})
	mux.HandleFunc("PUT /v1/services/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		b := body(r)
		s.headers = append(s.headers, r.Header.Clone())
		if members, _ := b["members"].([]any); len(members) < 2 {
			reply(w, 400, map[string]any{"error": map[string]any{"code": "invalid_service", "message": "a; b"},
				"errors": []any{"members: at least two hosts", "alias \"a b\": no spaces"}})
			return
		}
		if r.Header.Get("If-Match") == "stale" {
			reply(w, 409, map[string]any{"error": map[string]any{"code": "conflict", "message": "service shop changed since it was read"}})
			return
		}
		exists := r.PathValue("id") == "shop"
		if r.URL.Query().Get("check") == "1" {
			reply(w, 200, map[string]any{"ok": true, "service_id": r.PathValue("id"), "exists": exists, "errors": []any{}})
			return
		}
		s.puts = append(s.puts, b)
		v := s.view()
		v["service_id"] = r.PathValue("id")
		reply(w, 200, v)
	})
	mux.HandleFunc("DELETE /v1/services/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.deleted = append(s.deleted, r.PathValue("id"))
		reply(w, 200, map[string]any{"deleted": r.PathValue("id")})
	})
	mux.HandleFunc("GET /v1/operations", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"operations": []any{
			map[string]any{"operation_id": "op-old", "kind": "switchover", "service_id": "shop", "status": "ok", "from_host": "db2", "to_host": "db1", "epoch": 7, "actor": "rcmadmin", "created_at": "2026-11-13T10:00:00Z"},
			map[string]any{"operation_id": "op-new", "kind": "failover", "service_id": "shop", "status": "failed", "from_host": "db1", "to_host": "db2", "epoch": 8, "actor": "system:autofailover", "error": "x", "created_at": "2026-11-14T10:00:00Z"},
		}})
	})
	mux.HandleFunc("GET /v1/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		reply(w, 200, s.operation())
	})
	mux.HandleFunc("POST /v1/operations/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		id := r.PathValue("id")
		if id == "late" {
			reply(w, 409, map[string]any{"error": map[string]any{"code": "past_no_return", "message": "the new master of the service serves already"}})
			return
		}
		s.cancels = append(s.cancels, id)
		reply(w, 202, map[string]any{"operation_id": id, "cancel_requested": true})
	})
	mux.HandleFunc("GET /v1/routers", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"routers": []any{
			map[string]any{"router_id": "ghost", "configured": false},
			map[string]any{"router_id": "app1-router", "configured": true, "table_version": 7},
		}})
	})
	mux.HandleFunc("POST /v1/routers/{id}/sync", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.syncs = append(s.syncs, r.PathValue("id"))
		reply(w, 200, map[string]any{"router_id": r.PathValue("id"), "table_version": 8})
	})
	mux.HandleFunc("GET /v1/locate", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"query": r.URL.Query().Get("db"), "matches": []any{map[string]any{"node_id": "db1-m", "master": true}}})
	})
	return mux
}

func newServiceGT(t *testing.T) (*gatedTools, *stubServices) {
	t.Helper()
	old := clusterPoll
	clusterPoll = 5 * time.Millisecond
	t.Cleanup(func() { clusterPoll = old })
	stub := newStubServices()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	gt := newTestGT(t)
	cfg := *gt.live()
	cfg.RCM = config.RCM{URL: srv.URL, TimeoutSeconds: 10}
	gt.cfg.Swap(&cfg)
	t.Setenv(config.DefaultRCMPasswordEnv, "pw")
	return gt, stub
}

func mustContain(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%s lacks %q:\n%s", what, w, got)
		}
	}
}

// The Tier 0 tool: the list says where the master is and what may start;
// the checks change nothing and name the problems of RCM.
func TestClusterServiceViewsAndChecks(t *testing.T) {
	gt, stub := newServiceGT(t)
	ctx := context.Background()
	cl, rc, err := gt.rcmClient("tester")
	if err != nil {
		t.Fatal(err)
	}
	view := func(a clusterServiceArg) string {
		t.Helper()
		out, err := clusterService(ctx, cl, rc, a)
		if err != nil {
			t.Fatalf("view %q: %v", a.View, err)
		}
		return out
	}
	mustContain(t, "list", view(clusterServiceArg{}),
		"service shop: ok, epoch 7, router port 3051", "master: db1 (node db1-m), Firebird db1.lan:3050", "replica: db2 (node db2-r), IN_SYNC, lag 0 s, 0 segment(s) behind",
		"router: app1-router at app1.lan:3051, epoch 7, active host db1", "allowed now: switchover, failover, rejoin")
	mustContain(t, "show", view(clusterServiceArg{View: "show", Service: "shop"}), "service shop: ok", "GET /v1/services/shop", `"updated_at"`)
	mustContain(t, "policy", view(clusterServiceArg{View: "policy", Service: "shop"}), `"fence_mode": "route"`)
	ops := view(clusterServiceArg{View: "operations", Service: "shop"})
	mustContain(t, "operations", ops, "2 operation(s), newest first", "system:autofailover", "op-old")
	if strings.Index(ops, "op-new") > strings.Index(ops, "op-old") {
		t.Errorf("the operations are not newest first:\n%s", ops)
	}
	mustContain(t, "routers", view(clusterServiceArg{View: "routers"}), "app1-router")
	mustContain(t, "locate", view(clusterServiceArg{View: "locate", Query: "shop"}), "db1-m")

	mustContain(t, "check_switchover", view(clusterServiceArg{View: "check_switchover", Service: "shop"}), "switchover of shop: the checks pass (master db1 -> db2, epoch 7)")
	mustContain(t, "check_failover", view(clusterServiceArg{View: "check_failover", Service: "shop"}), "the checks pass", "what a failover may lose", `"lost_segments_at_least": 3`)
	stub.problems = []map[string]any{{"code": "not_in_sync", "node_id": "db2-r", "message": "the replica on db2-r is LAGGING"}}
	mustContain(t, "refused check", view(clusterServiceArg{View: "check_switchover", Service: "shop"}),
		"REFUSED by the checks, nothing was changed", "not_in_sync (db2-r): the replica on db2-r is LAGGING")
	if len(stub.starts) != 0 {
		t.Errorf("a check started an operation: %v", stub.starts)
	}

	for _, bad := range []clusterServiceArg{{View: "show"}, {View: "operation"}, {View: "locate"}, {View: "nope"}} {
		if _, err := clusterService(ctx, cl, rc, bad); err == nil {
			t.Errorf("view %q with no argument: no error", bad.View)
		}
	}
}

// A switchover: the request carries a request id and the timeouts, the tool
// waits for the operation and prints its steps with the point of no return.
func TestClusterSwitchoverWaitsAndPrintsSteps(t *testing.T) {
	gt, stub := newServiceGT(t)
	ctx := context.Background()
	mustContain(t, "preview", gt.clusterSwitchoverPreview(ctx, "", map[string]any{"service": "shop"}), "the checks pass", "applications cannot write")

	var last string
	out, err := gt.clusterSwitchoverExec(ctx, "", map[string]any{"service": "shop", "target_host": "db2", "quiesce_timeout_sec": 60.0}, func(_ float64, s string) { last = s })
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "result", out, "switchover op-1 of service shop: ok, db1 -> db2, epoch 8", "---- point of no return", "done    quiesce: last archived segment 42", "warning: warning: a note", "Next:")
	if last != "switchover: quiesce" {
		t.Errorf("progress %q", last)
	}
	b := stub.starts[0]
	if rid, _ := b["request_id"].(string); !strings.HasPrefix(rid, "fbmcp-") || len(rid) < 20 {
		t.Errorf("request id %v", b["request_id"])
	}
	if b["target_host"] != "db2" || b["quiesce_timeout_sec"] != 60.0 || b["apply_timeout_sec"] != nil {
		t.Errorf("body %v", b)
	}

	// wait=false returns at once with the operation id.
	out, err = gt.clusterSwitchoverExec(ctx, "", map[string]any{"service": "shop", "wait": false}, nil)
	if err != nil || !strings.Contains(out, "started: operation op-1") {
		t.Fatalf("wait=false: %q %v", out, err)
	}

	// An operation that fails is an error that says what is left to do.
	stub.opStatus = "failed"
	if _, err := gt.clusterSwitchoverExec(ctx, "", map[string]any{"service": "shop"}, nil); err == nil ||
		!strings.Contains(err.Error(), "switchover op-1 of service shop: failed") || !strings.Contains(err.Error(), "the replica did not apply") {
		t.Fatalf("a failed operation: %v", err)
	}

	// A refused start: the problems of RCM and the operation that runs.
	stub.problems = []map[string]any{{"code": "service_busy", "message": "service shop is switching"}}
	_, err = gt.clusterSwitchoverExec(ctx, "", map[string]any{"service": "shop"}, nil)
	if err == nil || !strings.Contains(err.Error(), "service_busy: service shop is switching") || !strings.Contains(err.Error(), "view=operation id=op-running") {
		t.Fatalf("a refused start: %v", err)
	}
	if _, err := gt.clusterSwitchoverExec(ctx, "", map[string]any{}, nil); err == nil {
		t.Error("a switchover without a service: no error")
	}
}

// A failover runs only with accept_data_loss true; the preview shows what
// may be lost.
func TestClusterFailoverNeedsAcceptDataLoss(t *testing.T) {
	gt, stub := newServiceGT(t)
	ctx := context.Background()
	mustContain(t, "preview", gt.clusterFailoverPreview(ctx, "", map[string]any{"service": "shop"}), "what a failover may lose", `"lost_segments_at_least": 3`, "is lost")
	for _, args := range []map[string]any{{"service": "shop"}, {"service": "shop", "accept_data_loss": false}, {"service": "shop", "accept_data_loss": "true"}} {
		if _, err := gt.clusterFailoverExec(ctx, "", args, nil); err == nil || !strings.Contains(err.Error(), "accept_data_loss must be true") {
			t.Errorf("failover with %v: %v", args, err)
		}
	}
	if len(stub.starts) != 0 {
		t.Fatalf("a failover without the acceptance reached RCM: %v", stub.starts)
	}
	out, err := gt.clusterFailoverExec(ctx, "", map[string]any{"service": "shop", "accept_data_loss": true, "apply_timeout_sec": 90.0}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "result", out, "failover op-1 of service shop: ok", "fb_cluster_rejoin")
	if b := stub.starts[0]; b["accept_data_loss"] != true || b["apply_timeout_sec"] != 90.0 {
		t.Errorf("body %v", b)
	}
	if err := validateArgs("fb_cluster_failover", map[string]any{"service": "shop"}); err == nil {
		t.Error("the args check lets a failover without accept_data_loss through")
	}
}

func TestClusterRejoinResumeCancelSync(t *testing.T) {
	gt, stub := newServiceGT(t)
	ctx := context.Background()

	out, err := gt.clusterRejoinExec(ctx, "", map[string]any{"service": "shop", "host": "db1"}, nil)
	if err != nil || !strings.Contains(out, "rejoin op-1 of service shop: ok") || stub.starts[0]["host"] != "db1" {
		t.Fatalf("rejoin: %q %v %v", out, err, stub.starts)
	}

	stub.state = "failed"
	stub.problems = []map[string]any{{"code": "firebird_not_answering", "node_id": "db1-m", "message": "no answer for 90 s"}}
	mustContain(t, "resume preview", gt.clusterResumePreview(ctx, "", map[string]any{"service": "shop"}), "REFUSED by the checks", "firebird_not_answering (db1-m)")
	if _, err := gt.clusterResumeExec(ctx, "", map[string]any{"service": "shop"}, nil); err == nil || !strings.Contains(err.Error(), "firebird_not_answering") || stub.resumed != 0 {
		t.Fatalf("a refused resume: %v", err)
	}
	stub.problems = nil
	out, err = gt.clusterResumeExec(ctx, "", map[string]any{"service": "shop"}, nil)
	if err != nil || !strings.Contains(out, "the checks pass (state failed -> ok)") || stub.resumed != 1 {
		t.Fatalf("resume: %q %v", out, err)
	}

	out, err = gt.clusterOperationCancelExec(ctx, "", map[string]any{"id": "op-1"}, nil)
	if err != nil || !strings.Contains(out, "cancel of operation op-1 requested") || len(stub.cancels) != 1 {
		t.Fatalf("cancel: %q %v", out, err)
	}
	if _, err := gt.clusterOperationCancelExec(ctx, "", map[string]any{"id": "late"}, nil); err == nil || !strings.Contains(err.Error(), "past the point of no return") {
		t.Fatalf("a late cancel: %v", err)
	}

	// Without a router the first router of rcm.json names the sync.
	out, err = gt.clusterRouterSyncExec(ctx, "", map[string]any{}, nil)
	if err != nil || !strings.Contains(out, `"table_version": 8`) || len(stub.syncs) != 1 || stub.syncs[0] != "app1-router" {
		t.Fatalf("router sync: %q %v %v", out, err, stub.syncs)
	}
}

// The rule: an arg that is left out keeps the value RCM has.
func TestClusterServicePolicyMergesTheRule(t *testing.T) {
	gt, stub := newServiceGT(t)
	ctx := context.Background()
	args := map[string]any{"service": "shop", "auto_failover": true, "max_data_loss_sec": 10.0, "fence_mode": "SHUTDOWN"}
	mustContain(t, "preview", gt.clusterServicePolicyPreview(ctx, "", args), `now:  {"auto_failover":false`, `then: {"auto_failover":true`, `"max_data_loss_sec":10`)
	if _, err := gt.clusterServicePolicyExec(ctx, "", args, nil); err != nil {
		t.Fatal(err)
	}
	p := stub.policy
	if p["auto_failover"] != true || p["max_data_loss_sec"] != 10.0 || p["failover_delay_sec"] != 90.0 || p["min_interval_sec"] != 3600.0 || p["fence_mode"] != "shutdown" {
		t.Errorf("the rule RCM got: %v", p)
	}
	if err := validateArgs("fb_cluster_service_policy", map[string]any{"service": "shop", "fence_mode": "kill"}); err == nil {
		t.Error("the args check lets an unknown fence_mode through")
	}
}

// A definition: the request asks RCM to check it; the errors come one by
// one; new and if_match become the preconditions of the PUT.
func TestClusterServicePutAndDelete(t *testing.T) {
	gt, stub := newServiceGT(t)
	ctx := context.Background()
	members := []any{
		map[string]any{"host": "db1", "master_node": "db1-m", "replica_node": "db1-r", "path": "/data/shop.fdb"},
		map[string]any{"host": "db2", "master_node": "db2-m", "replica_node": "db2-r", "path": "/data/shop.fdb"},
	}
	mustContain(t, "preview of an edit", gt.clusterServicePutPreview(ctx, "", map[string]any{"service": "shop", "members": members}), "RCM accepts the definition", "the epoch and the history stay")
	mustContain(t, "preview of a new service", gt.clusterServicePutPreview(ctx, "", map[string]any{"service": "shop2", "members": members, "new": true}), "service shop2 is new")
	mustContain(t, "preview of a bad definition", gt.clusterServicePutPreview(ctx, "", map[string]any{"service": "shop", "members": members[:1]}),
		"RCM refuses this definition", "members: at least two hosts", `alias "a b": no spaces`)
	if len(stub.puts) != 0 {
		t.Fatalf("a preview stored a definition: %v", stub.puts)
	}

	out, err := gt.clusterServicePutExec(ctx, "", map[string]any{"service": "shop", "members": members, "router_port": 3051.0, "alias": "shop", "if_match": "2026-11-14T20:29:01.42Z"}, nil)
	if err != nil || !strings.Contains(out, "service shop saved") {
		t.Fatalf("put: %q %v", out, err)
	}
	if b := stub.puts[0]; b["router_port"] != 3051.0 || b["alias"] != "shop" || b["policy"] != nil {
		t.Errorf("body %v", b)
	}
	if h := stub.headers[len(stub.headers)-1]; h.Get("If-Match") != "2026-11-14T20:29:01.42Z" || h.Get("If-None-Match") != "" {
		t.Errorf("headers of an edit: %v", h)
	}
	if _, err := gt.clusterServicePutExec(ctx, "", map[string]any{"service": "shop2", "members": members, "new": true}, nil); err != nil {
		t.Fatal(err)
	}
	if h := stub.headers[len(stub.headers)-1]; h.Get("If-None-Match") != "*" {
		t.Errorf("headers of a new service: %v", h)
	}
	if _, err := gt.clusterServicePutExec(ctx, "", map[string]any{"service": "shop", "members": members, "if_match": "stale"}, nil); err == nil || !strings.Contains(err.Error(), "updated_at as if_match") {
		t.Errorf("a stale if_match: %v", err)
	}
	if _, err := gt.clusterServicePutExec(ctx, "", map[string]any{"service": "shop", "members": members[:1]}, nil); err == nil || !strings.Contains(err.Error(), "members: at least two hosts") {
		t.Errorf("a bad definition: %v", err)
	}
	if _, err := gt.clusterServicePutExec(ctx, "", map[string]any{"service": "shop"}, nil); err == nil {
		t.Error("a definition without members: no error")
	}

	mustContain(t, "delete preview", gt.clusterServiceDeletePreview(ctx, "", map[string]any{"service": "shop"}), "service shop: ok", "What stays:", "What is lost:")
	out, err = gt.clusterServiceDeleteExec(ctx, "", map[string]any{"service": "shop"}, nil)
	if err != nil || !strings.Contains(out, "service shop deleted from RCM") || len(stub.deleted) != 1 {
		t.Fatalf("delete: %q %v", out, err)
	}
}

// The tiers of the service tools: reads and checks need no confirmation, a
// change of a definition or a rule one, and an operation that moves the
// master (or loses the record) the window and an approval out of band.
func TestClusterServiceToolTiers(t *testing.T) {
	want := map[string]int{
		"fb_cluster_service": 0, "fb_cluster_service_put": 1, "fb_cluster_service_policy": 1, "fb_cluster_resume": 1,
		"fb_cluster_operation_cancel": 1, "fb_cluster_router_sync": 1,
		"fb_cluster_switchover": 2, "fb_cluster_failover": 2, "fb_cluster_rejoin": 2, "fb_cluster_service_delete": 2,
	}
	metas := map[string]policy.ToolMeta{}
	for _, m := range policy.SystemTools() {
		metas[m.Name] = m
	}
	for name, tier := range want {
		m, ok := metas[name]
		if !ok || m.Tier != tier || m.Scope != "instance" {
			t.Errorf("%s: meta %+v ok %t, want tier %d, instance", name, m, ok, tier)
		}
		if tier > 0 {
			if _, ok := gatedArgSpecs[name]; !ok {
				t.Errorf("%s has no args spec", name)
			}
		}
	}
}
