package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/agentapi"
	"github.com/aleks/fbmcp/internal/config"
)

// clusterAgentStub is fbagent's /v1/cluster and /v1/cluster/rcm: the state
// of the host and the cluster_rcm operation.
type clusterAgentStub struct {
	mu     sync.Mutex
	state  agentapi.ClusterState
	posts  []map[string]any
	failOp bool
	gone   bool // an agent without the cluster API (before 3.1.0)
}

func (s *clusterAgentStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s.mu.Lock()
	defer s.mu.Unlock()
	reply := func(code int, v any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case s.gone:
		reply(http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "no route"}})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/cluster":
		reply(http.StatusOK, s.state)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/cluster/rcm":
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		s.posts = append(s.posts, body)
		reply(http.StatusAccepted, map[string]any{"operation_id": "op-rcm", "status_url": "/v1/operations/op-rcm"})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/operations/op-rcm":
		if s.failOp {
			reply(http.StatusOK, map[string]any{"operation_id": "op-rcm", "kind": "cluster_rcm", "state": "failed",
				"error": map[string]any{"code": "rcm_not_staged", "message": "hqbirdrcm is not in the product cache"}})
			return
		}
		reply(http.StatusOK, map[string]any{"operation_id": "op-rcm", "kind": "cluster_rcm", "state": "succeeded",
			"result": agentapi.ClusterRCMResult{ClusterID: "c1", RCMURL: "https://10.0.0.1:7443", ConsoleURL: "https://10.0.0.1:7444",
				CAHash: "sha256:cafe", Login: "rcmadmin", PasswordFile: "/opt/hqbird/clusterca/c1/rcm-initial-password"}})
	default:
		reply(http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": r.URL.Path}})
	}
}

func (s *clusterAgentStub) lastPost() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.posts) == 0 {
		return nil
	}
	return s.posts[len(s.posts)-1]
}

func newAgentClusterGT(t *testing.T) (*gatedTools, *mcp.ClientSession, *clusterAgentStub) {
	t.Helper()
	stub := &clusterAgentStub{state: agentapi.ClusterState{State: "free", Source: "none", Slot: 0, PairingPort: 7450,
		NodePorts: []int{7460, 7461}, RCM: agentapi.ClusterRCMInfo{Staged: true}}}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	gt, server := newAgentGT(t, func(a *config.Agent) { a.BaseURL = srv.URL })
	srvConn, cliConn := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	go server.Run(ctx, &mcp.IOTransport{Reader: srvConn, Writer: srvConn})
	t.Cleanup(func() { cliConn.Close() })
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "cluster-agent-test", Version: "0"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: cliConn, Writer: cliConn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return gt, sess, stub
}

func callTool(t *testing.T, sess *mcp.ClientSession, name string, arguments map[string]any) (string, bool) {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	tc, _ := res.Content[0].(*mcp.TextContent)
	return tc.Text, res.IsError
}

func TestAgentClusterRead(t *testing.T) {
	gt, sess, stub := newAgentClusterGT(t)
	before := len(gt.st.Pending())
	out, isErr := callTool(t, sess, agentClusterTool, map[string]any{})
	if isErr || !strings.Contains(out, `agent "ag1" cluster state: free`) || !strings.Contains(out, "pairing port 7450, node ports 7460, 7461") ||
		!strings.Contains(out, "hqbirdrcm is staged") || !strings.Contains(out, "fb_agent_cluster_rcm") {
		t.Fatalf("free host: err=%v %q", isErr, out)
	}
	if len(gt.st.Pending()) != before || stub.lastPost() != nil {
		t.Fatal("the read made a request or a POST")
	}

	// The RCM is made and fbmcp has no rcm: section yet: hqbird-install.
	stub.mu.Lock()
	stub.state = agentapi.ClusterState{State: "rcm_owner", ClusterID: "c1", Source: "rcm", RCMOwner: true, Slot: -1, PairingPort: 7450,
		RCM: agentapi.ClusterRCMInfo{Installed: true, Running: true, ClusterID: "c1", ThisAgent: true, ConsoleURL: "https://h:7444"}}
	stub.mu.Unlock()
	out, isErr = callTool(t, sess, agentClusterTool, map[string]any{})
	if isErr || !strings.Contains(out, "rcm_owner (cluster c1), runs the RCM of the host") || !strings.Contains(out, "slot: none") ||
		!strings.Contains(out, "made by this agent") || !strings.Contains(out, "fbmcp ctl hqbird-install") {
		t.Fatalf("rcm owner: err=%v %q", isErr, out)
	}

	stub.mu.Lock()
	stub.gone = true
	stub.mu.Unlock()
	if out, isErr = callTool(t, sess, agentClusterTool, map[string]any{}); !isErr || !strings.Contains(out, "fbagent 3.1.0") {
		t.Fatalf("old agent: err=%v %q", isErr, out)
	}
}

func TestAgentClusterNext(t *testing.T) {
	owner := agentapi.ClusterState{State: "rcm_owner", RCM: agentapi.ClusterRCMInfo{Installed: true}}
	if s := clusterNext(owner, true); !strings.Contains(s, "fb_cluster_status") {
		t.Fatalf("owner with rcm: %q", s)
	}
	member := agentapi.ClusterState{State: "in_cluster", ClusterID: "c9"}
	if s := clusterNext(member, false); !strings.Contains(s, "member of cluster c9") {
		t.Fatalf("member: %q", s)
	}
	if s := clusterNext(agentapi.ClusterState{State: "free"}, false); !strings.Contains(s, "cannot make an RCM") {
		t.Fatalf("not staged: %q", s)
	}
}

func TestClusterIDError(t *testing.T) {
	for _, ok := range []string{"", "c1", "prod-eu", "a", strings.Repeat("a", 63)} {
		if msg := clusterIDError(ok); msg != "" {
			t.Errorf("%q refused: %s", ok, msg)
		}
	}
	for _, bad := range []string{"C1", "-c1", "c1-", "c_1", "c.1", strings.Repeat("a", 64)} {
		if clusterIDError(bad) == "" {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestAgentClusterRCMIsGated(t *testing.T) {
	gt, sess, stub := newAgentClusterGT(t)
	before := len(gt.st.Pending())
	if out, isErr := callTool(t, sess, agentClusterRCMTool, map[string]any{"args": map[string]any{"cluster_id": "Bad_ID"}}); !isErr || !strings.Contains(out, "DNS label") {
		t.Fatalf("bad cluster_id: err=%v %q", isErr, out)
	}
	out, isErr := callTool(t, sess, agentClusterRCMTool, map[string]any{"args": map[string]any{"cluster_id": "c1", "reason": "new cluster"}})
	if isErr || !strings.Contains(out, "In-band token") || !strings.Contains(out, "Now (agent ag1): free") || !strings.Contains(out, "cluster_id: c1") {
		t.Fatalf("request: err=%v %q", isErr, out)
	}
	if len(gt.st.Pending()) != before+1 || stub.lastPost() != nil {
		t.Fatal("the RCM was made before the confirmation")
	}

	// The preview names the refusal the agent will give.
	stub.mu.Lock()
	stub.state.RCM = agentapi.ClusterRCMInfo{}
	stub.mu.Unlock()
	if out, _ = callTool(t, sess, agentClusterRCMTool, map[string]any{"args": map[string]any{}}); !strings.Contains(out, "rcm_not_staged") || !strings.Contains(out, "cluster_id: from the host name") {
		t.Fatalf("not staged preview: %q", out)
	}
}

func TestAgentClusterRCMExec(t *testing.T) {
	gt, _, stub := newAgentClusterGT(t)
	ctx := context.Background()
	prog := func(float64, string) {}
	out, err := gt.execs[agentClusterRCMTool](ctx, "", map[string]any{"cluster_id": "c1", "_grant_identity": "ops", "reason": "new cluster"}, prog)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"made the RCM of cluster c1 (operation op-rcm)", "rcm_url (agents join here): https://10.0.0.1:7443",
		"cluster CA hash: sha256:cafe", "RCM user: rcmadmin", "in /opt/hqbird/clusterca/c1/rcm-initial-password on the host (not shown)", "sudo fbmcp ctl hqbird-install"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	body := stub.lastPost()
	if body["cluster_id"] != "c1" || body["requested_by"] != "ops" || body["reason"] != "new cluster" || len(body) != 3 {
		t.Fatalf("body = %v", body)
	}

	stub.mu.Lock()
	stub.failOp = true
	stub.mu.Unlock()
	if _, err := gt.execs[agentClusterRCMTool](ctx, "", map[string]any{}, prog); err == nil || !strings.Contains(err.Error(), "the RCM was not made") || !strings.Contains(err.Error(), "rcm_not_staged") {
		t.Fatalf("failed op: %v", err)
	}
	if _, err := gt.execs[agentClusterRCMTool](ctx, "", map[string]any{"cluster_id": "-x"}, prog); err == nil || !strings.Contains(err.Error(), "DNS label") {
		t.Fatalf("bad id at exec: %v", err)
	}
}
