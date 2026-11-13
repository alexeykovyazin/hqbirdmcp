package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aleks/fbmcp/internal/config"
)

// unlinkRCM is an RCM with one cluster: the RCM host A0 (rcm + master MN)
// and the replica host A2 (replica RN + router RT).
type unlinkRCM struct {
	mu          sync.Mutex
	rev         int64
	members     []map[string]any
	links       []map[string]any
	puts        []map[string]any
	ifMatch     []string
	revokes     []string
	revokeFail  bool
	validateErr []string
	agents      []map[string]any
}

func newUnlinkRCM() *unlinkRCM {
	return &unlinkRCM{
		rev: 7,
		members: []map[string]any{
			{"member_id": "R0", "agent_id": "A0", "role": "rcm", "addresses": []string{"10.0.0.1"}, "listen_port": 7443},
			{"member_id": "MN", "agent_id": "A0", "role": "master", "addresses": []string{"10.0.0.1"}, "listen_port": 7470, "initial_config": map[string]any{"x": 1}},
			{"member_id": "RN", "agent_id": "A2", "role": "replica", "addresses": []string{"10.0.0.2"}, "listen_port": 7470},
			{"member_id": "RT", "agent_id": "A2", "role": "router", "addresses": []string{"10.0.0.2"}, "listen_port": 0},
		},
		links: []map[string]any{{"from": "MN", "to": "RN"}},
		agents: []map[string]any{
			{"agent_id": "A0", "revoked": false, "members": []string{"R0", "MN"}},
			{"agent_id": "A2", "revoked": false, "members": []string{"RN", "RT"}},
			{"agent_id": "A9", "revoked": false, "members": []string{}},
			{"agent_id": "A8", "revoked": true, "members": []string{}},
		},
	}
}

func (s *unlinkRCM) doc() map[string]any {
	return map[string]any{"contract": 1, "cluster_id": "c1", "pki": "rcm", "revision": s.rev,
		"members": s.members, "links": s.links, "future_field": map[string]any{"keep": true}}
}

func (s *unlinkRCM) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /v1/admin/clusters", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		reply(w, 200, map[string]any{"clusters": []map[string]any{{"cluster_id": "c1", "revision": s.rev, "members": len(s.members)}}})
	})
	mux.HandleFunc("GET /v1/admin/clusters/c1", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		reply(w, 200, s.doc())
	})
	mux.HandleFunc("POST /v1/admin/clusters/c1/validate", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		reply(w, 200, map[string]any{"errors": append([]string{}, s.validateErr...), "warnings": []string{}})
	})
	mux.HandleFunc("PUT /v1/admin/clusters/c1", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.puts = append(s.puts, in)
		s.ifMatch = append(s.ifMatch, r.Header.Get("If-Match"))
		if r.Header.Get("If-Match") != `"7"` {
			reply(w, 409, map[string]any{"error": map[string]any{"code": "conflict", "message": "revision mismatch"}})
			return
		}
		s.rev++
		in["revision"] = s.rev
		reply(w, 200, in)
	})
	mux.HandleFunc("GET /v1/admin/agents", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"agents": s.agents})
	})
	mux.HandleFunc("POST /v1/admin/agents/{id}/revoke", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.revokeFail {
			reply(w, 409, map[string]any{"error": map[string]any{"code": "agent_has_members", "message": "agent still has members"}})
			return
		}
		s.revokes = append(s.revokes, r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func newUnlinkGT(t *testing.T) (*gatedTools, *unlinkRCM) {
	t.Helper()
	stub := newUnlinkRCM()
	srv := httptest.NewServer(stub.handler(t))
	t.Cleanup(srv.Close)
	gt := newTestGT(t)
	cfg := *gt.live()
	cfg.RCM = config.RCM{URL: srv.URL, TimeoutSeconds: 10}
	gt.cfg.Swap(&cfg)
	t.Setenv(config.DefaultRCMPasswordEnv, "pw")
	return gt, stub
}

func TestClusterUnlinkReplica(t *testing.T) {
	gt, stub := newUnlinkGT(t)
	ctx := context.Background()
	args := map[string]any{"node": "RN"}
	pre := gt.clusterUnlinkPreview(ctx, "", args)
	for _, want := range []string{"agent A2", "remove member RN: replica", "remove member RT: router", "remove link MN -> RN", "RCM check: ok", "revokes the agent"} {
		if !strings.Contains(pre, want) {
			t.Fatalf("preview has no %q:\n%s", want, pre)
		}
	}
	if args["_revision"] != "7" || args["_members"] != "RN,RT" {
		t.Fatalf("pins %v", args)
	}
	out, err := gt.clusterUnlinkExec(ctx, "", args, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "members RN,RT removed: cluster c1 revision 8") || !strings.Contains(out, "agent A2 revoked") || !strings.Contains(out, "fb_agent_cluster") {
		t.Fatalf("out: %s", out)
	}
	if len(stub.puts) != 1 || stub.ifMatch[0] != `"7"` {
		t.Fatalf("puts %d if-match %v", len(stub.puts), stub.ifMatch)
	}
	put := stub.puts[0]
	ms := put["members"].([]any)
	if len(ms) != 2 || ms[0].(map[string]any)["member_id"] != "R0" || ms[1].(map[string]any)["member_id"] != "MN" {
		t.Fatalf("members %v", ms)
	}
	if ms[1].(map[string]any)["initial_config"] == nil || put["future_field"] == nil || put["pki"] != "rcm" {
		t.Fatalf("the PUT lost fields: %v", put)
	}
	if links := put["links"].([]any); len(links) != 0 {
		t.Fatalf("links %v", links)
	}
	if len(stub.revokes) != 1 || stub.revokes[0] != "A2" {
		t.Fatalf("revokes %v", stub.revokes)
	}
}

func TestClusterUnlinkChangedAfterRequest(t *testing.T) {
	gt, stub := newUnlinkGT(t)
	ctx := context.Background()
	args := map[string]any{"node": "RN"}
	gt.clusterUnlinkPreview(ctx, "", args)
	stub.rev = 9
	if _, err := gt.clusterUnlinkExec(ctx, "", args, nil); err == nil || !strings.Contains(err.Error(), "changed after the request") {
		t.Fatalf("want a refusal, got %v", err)
	}
	if _, err := gt.clusterUnlinkExec(ctx, "", map[string]any{"node": "RN"}, nil); err == nil || !strings.Contains(err.Error(), "no cluster revision") {
		t.Fatalf("want a refusal without pins, got %v", err)
	}
	if len(stub.puts) != 0 || len(stub.revokes) != 0 {
		t.Fatalf("a refused unlink reached RCM: puts %d revokes %v", len(stub.puts), stub.revokes)
	}
}

func TestClusterUnlinkRefusals(t *testing.T) {
	gt, stub := newUnlinkGT(t)
	ctx := context.Background()
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"node": "MN"}, "cannot leave its own cluster"},
		{map[string]any{"node": "XX"}, "not a member of cluster c1"},
		{map[string]any{"node": "RN", "agent": "A2"}, "one of them"},
		{map[string]any{}, "one of them"},
		{map[string]any{"agent": "A8"}, "revoked already"},
		{map[string]any{"agent": "A7"}, "knows no agent A7"},
	} {
		pre := gt.clusterUnlinkPreview(ctx, "", c.args)
		if !strings.Contains(pre, "will be refused") || !strings.Contains(pre, c.want) {
			t.Fatalf("%v: preview %s", c.args, pre)
		}
		c.args["_revision"] = "7"
		if _, err := gt.clusterUnlinkExec(ctx, "", c.args, nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%v: exec %v", c.args, err)
		}
	}
	if len(stub.puts) != 0 || len(stub.revokes) != 0 {
		t.Fatalf("a refused unlink reached RCM: puts %d revokes %v", len(stub.puts), stub.revokes)
	}
}

func TestClusterUnlinkRCMCheckFails(t *testing.T) {
	gt, stub := newUnlinkGT(t)
	stub.validateErr = []string{"replica RN has no link from a master"}
	pre := gt.clusterUnlinkPreview(context.Background(), "", map[string]any{"node": "RN"})
	if !strings.Contains(pre, "RCM refuses the new cluster document: replica RN has no link from a master") || !strings.Contains(pre, "unlink its replicas first") {
		t.Fatalf("preview %s", pre)
	}
}

// An agent with no members left (a host that left by fbagent --cluster-leave,
// or a revoke that failed after the PUT) is only revoked.
func TestClusterUnlinkRevokeOnly(t *testing.T) {
	gt, stub := newUnlinkGT(t)
	ctx := context.Background()
	args := map[string]any{"agent": "A9"}
	if pre := gt.clusterUnlinkPreview(ctx, "", args); !strings.Contains(pre, "RCM only revokes it") {
		t.Fatalf("preview %s", pre)
	}
	out, err := gt.clusterUnlinkExec(ctx, "", args, nil)
	if err != nil || !strings.Contains(out, "agent A9 revoked") {
		t.Fatalf("out %q err %v", out, err)
	}
	if len(stub.puts) != 0 || len(stub.revokes) != 1 {
		t.Fatalf("puts %d revokes %v", len(stub.puts), stub.revokes)
	}
}

func TestClusterUnlinkRevokeFails(t *testing.T) {
	gt, stub := newUnlinkGT(t)
	stub.revokeFail = true
	ctx := context.Background()
	args := map[string]any{"node": "RN"}
	gt.clusterUnlinkPreview(ctx, "", args)
	_, err := gt.clusterUnlinkExec(ctx, "", args, nil)
	if err == nil || !strings.Contains(err.Error(), "members RN,RT removed") || !strings.Contains(err.Error(), `args={agent:"A2"}`) {
		t.Fatalf("want the revoke hint, got %v", err)
	}
}
