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

	"github.com/aleks/fbmcp/internal/config"
)

// pairingStub is fbagent's /v1/cluster/pairing: it keeps the window and
// records the POST bodies.
type pairingStub struct {
	mu    sync.Mutex
	open  bool
	posts []map[string]any
	gone  bool // an agent without the route
}

func (s *pairingStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gone || r.URL.Path != "/v1/cluster/pairing" {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no route"}}`))
		return
	}
	if r.Method == http.MethodPost {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		s.posts = append(s.posts, body)
		s.open, _ = body["on"].(bool)
	}
	if s.open {
		_, _ = w.Write([]byte(`{"state":"open","expires":"2026-11-12T12:00:00Z","port":7450,"fingerprint":"sha256:abcd","rcm_allow":[],"standing":"free","message":"The agent service opens port 7450 within a minute."}`))
		return
	}
	_, _ = w.Write([]byte(`{"state":"closed","reason":"window_closed","port":7450,"rcm_allow":[],"standing":"free","message":"The pairing window is closed."}`))
}

func (s *pairingStub) lastPost() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.posts) == 0 {
		return nil
	}
	return s.posts[len(s.posts)-1]
}

func newPairingGT(t *testing.T) (*gatedTools, *mcp.ClientSession, *pairingStub) {
	t.Helper()
	stub := &pairingStub{}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	gt, server := newAgentGT(t, func(a *config.Agent) { a.BaseURL = srv.URL })
	srvConn, cliConn := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	go server.Run(ctx, &mcp.IOTransport{Reader: srvConn, Writer: srvConn})
	t.Cleanup(func() { cliConn.Close() })
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "pairing-test", Version: "0"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: cliConn, Writer: cliConn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return gt, sess, stub
}

func callPairing(t *testing.T, sess *mcp.ClientSession, args map[string]any) (string, bool) {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: agentPairingTool, Arguments: map[string]any{"args": args}})
	if err != nil {
		t.Fatal(err)
	}
	tc, _ := res.Content[0].(*mcp.TextContent)
	return tc.Text, res.IsError
}

func TestAgentClusterPairingStatusIsTier0(t *testing.T) {
	gt, sess, stub := newPairingGT(t)
	before := len(gt.st.Pending())
	for _, args := range []map[string]any{{}, {"action": "status"}} {
		out, isErr := callPairing(t, sess, args)
		if isErr || !strings.Contains(out, "closed (window_closed)") || !strings.Contains(out, "standing: free") {
			t.Fatalf("status %v: err=%v %q", args, isErr, out)
		}
	}
	if len(gt.st.Pending()) != before || stub.lastPost() != nil {
		t.Fatal("status made a request or changed the window")
	}
	if out, isErr := callPairing(t, sess, map[string]any{"action": "status", "hours": 2}); !isErr || !strings.Contains(out, "hours goes with action=on") {
		t.Fatalf("status with hours: err=%v %q", isErr, out)
	}
	if out, isErr := callPairing(t, sess, map[string]any{"action": "open"}); !isErr || !strings.Contains(out, "action") {
		t.Fatalf("unknown action: err=%v %q", isErr, out)
	}
}

func TestAgentClusterPairingOnOffAreGated(t *testing.T) {
	gt, sess, stub := newPairingGT(t)
	before := len(gt.st.Pending())
	out, isErr := callPairing(t, sess, map[string]any{"action": "on", "hours": 2})
	if isErr || !strings.Contains(out, "In-band token") || !strings.Contains(out, "Now (agent ag1): closed (window_closed)") {
		t.Fatalf("on: err=%v %q", isErr, out)
	}
	if len(gt.st.Pending()) != before+1 || stub.lastPost() != nil {
		t.Fatal("on ran before the confirmation")
	}
	if out, isErr = callPairing(t, sess, map[string]any{"action": "off", "hours": 2}); !isErr || !strings.Contains(out, "hours goes with action=on") {
		t.Fatalf("off with hours: err=%v %q", isErr, out)
	}
	if out, isErr = callPairing(t, sess, map[string]any{"action": "on", "hours": 48}); !isErr {
		t.Fatalf("hours 48 accepted: %q", out)
	}
}

func TestAgentClusterPairingExec(t *testing.T) {
	gt, _, stub := newPairingGT(t)
	ctx := context.Background()
	prog := func(float64, string) {}
	out, err := gt.execs[agentPairingTool](ctx, "", map[string]any{
		"action": "on", "hours": float64(2), "rcm_allow": "10.0.0.0/8, rcm.lan", "_grant_identity": "ops", "reason": "link rcm-r",
	}, prog)
	if err != nil || !strings.Contains(out, "open until 2026-11-12T12:00:00Z") || !strings.Contains(out, "fingerprint: sha256:abcd") || !strings.Contains(out, "fb_cluster_probe") {
		t.Fatalf("on: %v %q", err, out)
	}
	body := stub.lastPost()
	if body["on"] != true || body["hours"] != float64(2) || body["requested_by"] != "ops" || body["reason"] != "link rcm-r" {
		t.Fatalf("on body = %v", body)
	}
	if allow, _ := body["rcm_allow"].([]any); len(allow) != 2 || allow[0] != "10.0.0.0/8" || allow[1] != "rcm.lan" {
		t.Fatalf("rcm_allow = %v", body["rcm_allow"])
	}

	out, err = gt.execs[agentPairingTool](ctx, "", map[string]any{"action": "off"}, prog)
	if err != nil || !strings.Contains(out, "closed (window_closed)") {
		t.Fatalf("off: %v %q", err, out)
	}
	body = stub.lastPost()
	if body["on"] != false || body["hours"] != nil || body["rcm_allow"] != nil {
		t.Fatalf("off body = %v", body)
	}

	stub.mu.Lock()
	stub.gone = true
	stub.mu.Unlock()
	if _, err := gt.execs[agentPairingTool](ctx, "", map[string]any{"action": "on"}, prog); err == nil || !strings.Contains(err.Error(), "fbagent --cluster-pairing on") {
		t.Fatalf("old agent: %v", err)
	}
}
