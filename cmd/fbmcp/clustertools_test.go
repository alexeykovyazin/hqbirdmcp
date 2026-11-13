package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/policy"
	"github.com/aleks/fbmcp/internal/rcmapi"
	"github.com/aleks/fbmcp/internal/secrets"
)

// stubRCM is a small RCM operator API for the cluster tools.
type stubRCM struct {
	mu        sync.Mutex
	fp        map[string]string // host -> fingerprint
	links     []map[string]any
	reinitRun int
	reinitEnd string
	password  string
	enrolls   []map[string]any
	actors    []string
	// loneMaster: the cluster has a master, so a link without replica_of
	// or new_master is refused (RCM 2027.5.9).
	loneMaster bool
	// member: hosts that are in the cluster (the RCM host): the probe
	// gives them state member and no fingerprint (RCM 2027.5.9).
	member map[string]bool
}

func (s *stubRCM) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	body := func(r *http.Request) map[string]any {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		return m
	}
	mux.HandleFunc("POST /v1/admin/pair/probe", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		var hosts []map[string]any
		for _, h := range body(r)["hosts"].([]any) {
			hs := h.(string)
			if s.member[hs] {
				hosts = append(hosts, map[string]any{"input": hs, "host": hs, "port": 7450, "state": "member", "rcm_host": true, "node_name": hs + "-fb50"})
				continue
			}
			hosts = append(hosts, map[string]any{"input": hs, "host": hs, "port": 7450, "fingerprint": s.fp[hs], "state": "free", "node_name": hs + "-fb50"})
		}
		reply(w, 200, map[string]any{"cluster_id": "c1", "rcm_url": "https://10.0.0.1:7443", "seats": 2, "hosts": hosts, "ok": true})
	})
	mux.HandleFunc("POST /v1/admin/pair/link", func(w http.ResponseWriter, r *http.Request) {
		b := body(r)
		s.mu.Lock()
		s.links = append(s.links, b)
		lone := s.loneMaster
		s.mu.Unlock()
		if lone && b["replica_of"] == nil && b["new_master"] != true {
			reply(w, 409, map[string]any{"error": map[string]any{"code": "lone_master", "message": "cluster c1 has the master a-master"}})
			return
		}
		reply(w, 202, map[string]any{"id": "L1", "state": "running", "step": "certs"})
	})
	mux.HandleFunc("GET /v1/admin/pair/link/L1", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"id": "L1", "state": "done", "step": "done", "master": "a",
			"hosts": []map[string]any{{"input": "a", "state": "running", "role": "master", "member_id": "a-master"}, {"input": "b", "state": "running", "role": "replica"}}})
	})
	mux.HandleFunc("POST /v1/account/password", func(w http.ResponseWriter, r *http.Request) {
		b := body(r)
		s.mu.Lock()
		defer s.mu.Unlock()
		if b["current_password"] != s.password {
			reply(w, 403, map[string]any{"error": map[string]any{"code": "wrong_password", "message": "the current password is wrong"}})
			return
		}
		s.password = b["new_password"].(string)
		reply(w, 200, map[string]any{"changed": true})
	})
	mux.HandleFunc("POST /v1/nodes/MN/databases/enroll", func(w http.ResponseWriter, r *http.Request) {
		b := body(r)
		s.mu.Lock()
		s.enrolls = append(s.enrolls, b)
		s.actors = append(s.actors, r.Header.Get("X-Admin-User"))
		s.mu.Unlock()
		if b["dry_run"] == true {
			reply(w, 200, map[string]any{"databases": []string{"/databases/master/employee.fdb"}})
			return
		}
		reply(w, 202, map[string]any{"command_id": "c2"})
	})
	mux.HandleFunc("GET /v1/commands/c2", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"id": "c2", "state": "succeeded"})
	})
	mux.HandleFunc("POST /v1/nodes/MN/reinit", func(w http.ResponseWriter, r *http.Request) {
		b := body(r)
		if b["db_id"] != "D1" || b["ignore_window"] != true {
			t.Errorf("reinit body %v", b)
		}
		reply(w, 202, map[string]any{"command_id": "c3", "reinit_id": "r1"})
	})
	mux.HandleFunc("GET /v1/reinit/r1", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.reinitRun++
		if s.reinitRun < 3 {
			reply(w, 200, map[string]any{"id": "r1", "status": "running", "phase": "copy", "percent": 40})
			return
		}
		reply(w, 200, map[string]any{"id": "r1", "status": s.reinitEnd, "code": "x", "error": "boom"})
	})
	return mux
}

func newClusterGT(t *testing.T) (*gatedTools, *stubRCM) {
	t.Helper()
	old := clusterPoll
	clusterPoll = 5 * time.Millisecond
	t.Cleanup(func() { clusterPoll = old })
	stub := &stubRCM{fp: map[string]string{"a": "SHA256:aaa", "b": "SHA256:bbb"}, reinitEnd: "ok"}
	srv := httptest.NewServer(stub.handler(t))
	t.Cleanup(srv.Close)
	gt := newTestGT(t)
	cfg := *gt.live()
	cfg.RCM = config.RCM{URL: srv.URL, TimeoutSeconds: 10}
	gt.cfg.Swap(&cfg)
	return gt, stub
}

func TestClusterLinkPinsFingerprints(t *testing.T) {
	gt, stub := newClusterGT(t)
	t.Setenv(config.DefaultRCMPasswordEnv, "pw")
	ctx := context.Background()

	args := map[string]any{"hosts": "a, b", "master": "a"}
	pre := gt.clusterLinkPreview(ctx, "", args)
	if !strings.Contains(pre, "SHA256:aaa") || !strings.Contains(pre, "SHA256:bbb") || !strings.Contains(pre, "rcm_url: https://10.0.0.1:7443") {
		t.Fatalf("preview: %s", pre)
	}
	out, err := gt.clusterLinkExec(ctx, "", args, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a: running, role master, member a-master") {
		t.Fatalf("out: %s", out)
	}
	if len(stub.links) != 1 {
		t.Fatalf("links %d", len(stub.links))
	}
	hosts := stub.links[0]["hosts"].([]any)
	if hosts[1].(map[string]any)["fingerprint"] != "SHA256:bbb" || stub.links[0]["master"] != "a" {
		t.Fatalf("link body %v", stub.links[0])
	}

	// A fingerprint that changes after the request stops the link.
	stub.fp["b"] = "SHA256:evil"
	if _, err := gt.clusterLinkExec(ctx, "", args, nil); err == nil || !strings.Contains(err.Error(), "changed after the request") {
		t.Fatalf("want a refusal, got %v", err)
	}
	// No pins (the probe failed at request time): refused.
	if _, err := gt.clusterLinkExec(ctx, "", map[string]any{"hosts": "a,b"}, nil); err == nil || !strings.Contains(err.Error(), "no probed fingerprints") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if len(stub.links) != 1 {
		t.Fatalf("a refused link reached RCM: %d", len(stub.links))
	}
}

// Since RCM 2027.5.9 the RCM host is a member row without a fingerprint:
// the link takes it without a pin, and only while it is still a member.
func TestClusterLinkRCMHostRow(t *testing.T) {
	gt, stub := newClusterGT(t)
	t.Setenv(config.DefaultRCMPasswordEnv, "pw")
	ctx := context.Background()
	stub.member = map[string]bool{"a": true}

	args := map[string]any{"hosts": "a,b", "master": "a"}
	pre := gt.clusterLinkPreview(ctx, "", args)
	if !strings.Contains(pre, "- a: state member") || !strings.Contains(pre, "RCM host") || !strings.Contains(pre, "no fingerprint to compare") || !strings.Contains(pre, "SHA256:bbb") {
		t.Fatalf("preview: %s", pre)
	}
	if _, err := gt.clusterLinkExec(ctx, "", args, nil); err != nil {
		t.Fatal(err)
	}
	hosts := stub.links[0]["hosts"].([]any)
	if h := hosts[0].(map[string]any); h["host"] != "a" || h["fingerprint"] != "" {
		t.Fatalf("RCM host row %v", h)
	}
	if h := hosts[1].(map[string]any); h["fingerprint"] != "SHA256:bbb" {
		t.Fatalf("replica row %v", h)
	}

	// A member row that is free at run time is not linked without a pin.
	stub.member = nil
	if _, err := gt.clusterLinkExec(ctx, "", args, nil); err == nil || !strings.Contains(err.Error(), "host a had no fingerprint") {
		t.Fatalf("want a refusal, got %v", err)
	}
	// A free row that became a member is refused too.
	args = map[string]any{"hosts": "a,b", "master": "a"}
	_ = gt.clusterLinkPreview(ctx, "", args)
	stub.member = map[string]bool{"b": true}
	if _, err := gt.clusterLinkExec(ctx, "", args, nil); err == nil || !strings.Contains(err.Error(), "host b was not a member") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if len(stub.links) != 1 {
		t.Fatalf("a refused link reached RCM: %d", len(stub.links))
	}
}

func TestClusterLinkModes(t *testing.T) {
	for _, tc := range []struct {
		args map[string]any
		bad  bool
	}{
		{map[string]any{"hosts": "b"}, false},
		{map[string]any{"hosts": "b", "replica_of": "a-master"}, false},
		{map[string]any{"hosts": "a,b", "master": "a", "new_master": true}, false},
		{map[string]any{"hosts": "b", "replica_of": "a-master", "master": "b"}, true},
		{map[string]any{"hosts": "b", "replica_of": "a-master", "new_master": true}, true},
	} {
		if got := linkModeError(tc.args) != ""; got != tc.bad {
			t.Errorf("linkModeError(%v) = %v, want %v", tc.args, got, tc.bad)
		}
	}
	if s := linkMode(map[string]any{"replica_of": "a-master"}); !strings.Contains(s, "replica of master member a-master") {
		t.Errorf("linkMode replica_of: %q", s)
	}
	if s := linkMode(map[string]any{"new_master": true}); !strings.Contains(s, "the first row") || !strings.Contains(s, "new_master") {
		t.Errorf("linkMode new_master: %q", s)
	}

	gt, stub := newClusterGT(t)
	t.Setenv(config.DefaultRCMPasswordEnv, "pw")
	ctx := context.Background()
	stub.loneMaster = true

	// One free row into a cluster with a master: RCM refuses, the reply
	// names the way out.
	args := map[string]any{"hosts": "b"}
	_ = gt.clusterLinkPreview(ctx, "", args)
	if _, err := gt.clusterLinkExec(ctx, "", args, nil); err == nil || !strings.Contains(err.Error(), "lone_master") || !strings.Contains(err.Error(), "replica_of=") {
		t.Fatalf("want lone_master with a hint, got %v", err)
	}
	if b := stub.links[len(stub.links)-1]; b["replica_of"] != nil || b["new_master"] != nil {
		t.Fatalf("empty mode fields were sent: %v", b)
	}

	args = map[string]any{"hosts": "b", "replica_of": "a-master"}
	pre := gt.clusterLinkPreview(ctx, "", args)
	if !strings.Contains(pre, "mode: every row gets a replica of master member a-master") {
		t.Fatalf("preview: %s", pre)
	}
	if _, err := gt.clusterLinkExec(ctx, "", args, nil); err != nil {
		t.Fatal(err)
	}
	if b := stub.links[len(stub.links)-1]; b["replica_of"] != "a-master" || b["new_master"] != nil || agentStr(b["master"]) != "" {
		t.Fatalf("replica_of body: %v", b)
	}

	args = map[string]any{"hosts": "b", "new_master": true}
	_ = gt.clusterLinkPreview(ctx, "", args)
	if _, err := gt.clusterLinkExec(ctx, "", args, nil); err != nil {
		t.Fatal(err)
	}
	if b := stub.links[len(stub.links)-1]; b["new_master"] != true || b["replica_of"] != nil {
		t.Fatalf("new_master body: %v", b)
	}

	// A bad mix is refused before RCM.
	n := len(stub.links)
	args = map[string]any{"hosts": "b", "replica_of": "a-master", "master": "b"}
	_ = gt.clusterLinkPreview(ctx, "", args)
	if _, err := gt.clusterLinkExec(ctx, "", args, nil); err == nil || !strings.Contains(err.Error(), "nothing linked") {
		t.Fatalf("bad mix: %v", err)
	}
	if len(stub.links) != n {
		t.Fatal("a refused link reached RCM")
	}
}

func TestProbeTableRowsPerAgent(t *testing.T) {
	p := rcmapi.ProbeAnswer{ClusterID: "c1", RCMURL: "https://10.0.0.1:7443", Seats: 4,
		Hosts: []rcmapi.ProbeHost{
			{Input: "h2:7450", State: "free", Instance: "fb50", FirebirdPort: 3050, NodePort: 7460, Fingerprint: "SHA256:aaa"},
			{Input: "h2:7451", State: "free", Instance: "fb30", FirebirdPort: 3053, NodePort: 7462, RCMLocal: true},
		},
		Skipped: []rcmapi.ProbeSkipped{{Input: "h2", Closed: []int{7452, 7453}, Summary: "closed 7452-7453"}},
	}
	out := probeTable(p)
	for _, want := range []string{"- h2:7450: state free", "instance fb50, Firebird port 3050, node port 7460", "fingerprint SHA256:aaa",
		"- h2:7451: state free", "own RCM (stopped and disabled when it joins)", "skipped ports of h2: closed 7452-7453"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestLinkRequestJSON(t *testing.T) {
	b, _ := json.Marshal(rcmapi.LinkRequest{Hosts: []rcmapi.LinkHostIn{{Host: "b", Fingerprint: "SHA256:bbb"}}})
	if s := string(b); strings.Contains(s, "new_master") || strings.Contains(s, "replica_of") {
		t.Fatalf("empty mode fields in %s", s)
	}
	b, _ = json.Marshal(rcmapi.LinkRequest{NewMaster: true, ReplicaOf: "x"})
	if s := string(b); !strings.Contains(s, `"new_master":true`) || !strings.Contains(s, `"replica_of":"x"`) {
		t.Fatalf("mode fields missing in %s", s)
	}
}

func TestClusterPasswordStoresSecret(t *testing.T) {
	gt, stub := newClusterGT(t)
	dir := t.TempDir()
	oldDir := secrets.Dir()
	secrets.SetDir(dir)
	t.Cleanup(func() { secrets.SetDir(oldDir) })
	if err := secrets.WriteFile(dir, config.DefaultRCMPasswordEnv, "first-password"); err != nil {
		t.Fatal(err)
	}
	stub.password = "first-password"
	out, err := gt.clusterPasswordExec(context.Background(), "", map[string]any{"_grant_identity": "local"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := secrets.ReadFile(dir, config.DefaultRCMPasswordEnv)
	if got != stub.password || len(got) != 24 || got == "first-password" {
		t.Fatalf("secret %q, rcm %q", got, stub.password)
	}
	if strings.Contains(out, got) {
		t.Fatal("the new password is in the answer")
	}

	// RCM refuses: the old secret stays.
	stub.password = "something-else"
	if _, err := gt.clusterPasswordExec(context.Background(), "", map[string]any{}, nil); err == nil {
		t.Fatal("want an error")
	}
	if now, _ := secrets.ReadFile(dir, config.DefaultRCMPasswordEnv); now != got {
		t.Fatalf("secret changed to %q after a refusal", now)
	}

	// From the environment: fbmcp cannot change it.
	t.Setenv(config.DefaultRCMPasswordEnv, "from-env")
	if _, err := gt.clusterPasswordExec(context.Background(), "", map[string]any{}, nil); err == nil || !strings.Contains(err.Error(), "environment variable") {
		t.Fatalf("want the env refusal, got %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, config.DefaultRCMPasswordEnv)); strings.TrimSpace(string(b)) != got {
		t.Fatal("secret file changed")
	}
}

func TestClusterEnrollPreviewAndExec(t *testing.T) {
	gt, stub := newClusterGT(t)
	t.Setenv(config.DefaultRCMPasswordEnv, "pw")
	args := map[string]any{"node": "MN", "root": "/databases/master", "recursive": true, "_grant_identity": "alice"}
	pre := gt.clusterEnrollPreview(context.Background(), "", args)
	if !strings.Contains(pre, "employee.fdb") {
		t.Fatalf("preview %s", pre)
	}
	out, err := gt.clusterEnrollExec(context.Background(), "", args, nil)
	if err != nil || !strings.Contains(out, "command c2 succeeded") {
		t.Fatalf("out %q err %v", out, err)
	}
	if len(stub.enrolls) != 2 || stub.enrolls[1]["dry_run"] != nil || stub.enrolls[1]["recursive"] != true {
		t.Fatalf("enrolls %v", stub.enrolls)
	}
	if stub.actors[1] != "fbmcp:alice" {
		t.Fatalf("actor %q", stub.actors[1])
	}
}

func TestClusterReinitWaits(t *testing.T) {
	gt, stub := newClusterGT(t)
	t.Setenv(config.DefaultRCMPasswordEnv, "pw")
	var last string
	out, err := gt.clusterReinitExec(context.Background(), "", map[string]any{"node": "MN", "db_id": "D1"}, func(_ float64, s string) { last = s })
	if err != nil || !strings.Contains(out, "reinit r1 of D1 on node MN: ok") {
		t.Fatalf("out %q err %v", out, err)
	}
	if last != "reinit: copy" {
		t.Fatalf("progress %q", last)
	}
	stub.reinitRun, stub.reinitEnd = 0, "failed"
	if _, err := gt.clusterReinitExec(context.Background(), "", map[string]any{"node": "MN", "db_id": "D1"}, nil); err == nil || !strings.Contains(err.Error(), "ended failed") {
		t.Fatalf("want failure, got %v", err)
	}
}

func TestClusterToolsVisibleOnlyWithRCM(t *testing.T) {
	id := policy.Identity{Name: "local", MaxTier: 2}
	tiers := toolTiers(toolMeta)
	cfg := &config.Config{}
	if toolVisible("fb_cluster_status", id, cfg, tiers) {
		t.Fatal("visible without rcm")
	}
	cfg.RCM.URL = "https://127.0.0.1:7444"
	if !toolVisible("fb_cluster_status", id, cfg, tiers) || !toolVisible("fb_cluster_reinit", id, cfg, tiers) {
		t.Fatal("hidden with rcm")
	}
	if toolVisible("fb_cluster_reinit", policy.Identity{Name: "ro", MaxTier: 1}, cfg, tiers) {
		t.Fatal("tier 2 visible to a tier 1 identity")
	}
}

func TestClusterToolsWithoutRCM(t *testing.T) {
	gt := newTestGT(t)
	if _, _, err := gt.rcmClient("x"); err != errRCMOff {
		t.Fatalf("want errRCMOff, got %v", err)
	}
}
