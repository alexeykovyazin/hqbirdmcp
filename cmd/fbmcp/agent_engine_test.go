package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/config"
)

// engineStub scripts the RCM-contract scenarios: the instance document, and
// per-attempt operation outcomes.
type engineStub struct {
	mu         sync.Mutex
	instID     string
	state      string   // pre/post state served by GET /v1/instances/fb1
	outcomes   []string // per POST: "ok", "1056", "work_continues", "fail"
	posts      int
	bodies     []map[string]any
	afterState string // state once an op finished (default running)
}

func (s *engineStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/instances/fb1":
		fmt.Fprintf(w, `{"instance_id":%q,"service_name":"FirebirdServerDefaultInstance","state":%q,"port":3050}`, s.instID, s.state)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/instances/fb1/"):
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		s.bodies = append(s.bodies, m)
		s.posts++
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, `{"operation_id":"op-%d"}`, s.posts)
	case strings.HasPrefix(r.URL.Path, "/v1/operations/op-"):
		var n int
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/v1/operations/op-"), "%d", &n)
		out := "ok"
		if n-1 < len(s.outcomes) {
			out = s.outcomes[n-1]
		}
		after := s.afterState
		if after == "" {
			after = "running"
		}
		switch out {
		case "ok":
			s.state = after
			fmt.Fprintf(w, `{"operation_id":"op-%d","kind":"restart","state":"succeeded"}`, n)
		case "1056":
			fmt.Fprintf(w, `{"operation_id":"op-%d","kind":"restart","state":"failed","error":{"code":"service_error","message":"start: The service has not been started (1056)"}}`, n)
		case "work_continues":
			s.state = after // the start finishes on its own
			fmt.Fprintf(w, `{"operation_id":"op-%d","kind":"restart","state":"failed","error":{"code":"timeout","message":"deadline","work_continues":true}}`, n)
		default:
			fmt.Fprintf(w, `{"operation_id":"op-%d","kind":"restart","state":"failed","error":{"code":"service_error","message":"access denied"}}`, n)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no route"}}`))
	}
}

func newEngineGT(t *testing.T, st *engineStub) *gatedTools {
	t.Helper()
	gt := newTestGT(t)
	srv := httptest.NewServer(st)
	t.Cleanup(srv.Close)
	t.Setenv("FBMCP_TEST_AGENT_TOKEN", "tok9")
	cfg := gt.live()
	cfg.Agents = []config.Agent{{ID: "ag1", BaseURL: srv.URL, TokenEnv: "FBMCP_TEST_AGENT_TOKEN", TimeoutSeconds: 40}}
	gt.cfg = config.NewHandle(cfg)
	oldC, oldR, oldP, oldT := clusterNodePresent, engineRetryDelay, enginePollInterval, tcpProbe
	clusterNodePresent = func() bool { return false }
	engineRetryDelay, enginePollInterval = 10*time.Millisecond, 10*time.Millisecond
	tcpProbe = func(context.Context, int) error { return nil }
	t.Cleanup(func() { clusterNodePresent, engineRetryDelay, enginePollInterval, tcpProbe = oldC, oldR, oldP, oldT })
	return gt
}

func TestEngineExecContract(t *testing.T) {
	prog := func(float64, string) {}
	ctx := context.Background()

	t.Run("happy path sends RCM body and post-checks", func(t *testing.T) {
		st := &engineStub{instID: "fb1", state: "running"}
		gt := newEngineGT(t, st)
		out, err := gt.agentEngineExec("restart")(ctx, "", map[string]any{"reason": "r", "_grant_identity": "op1"}, prog)
		if err != nil {
			t.Fatal(err)
		}
		b := st.bodies[0]
		if b["coalesce"] != true || b["requested_by"] != "op1" || b["reason"] != "r" || b["timeout_sec"] != float64(30) {
			t.Fatalf("body = %v", b)
		}
		if !contains(out, "post-check: state running") || !contains(out, "accepts connections") {
			t.Fatalf("out = %q", out)
		}
	})

	t.Run("instance mismatch refused before any POST", func(t *testing.T) {
		st := &engineStub{instID: "other", state: "running"}
		gt := newEngineGT(t, st)
		_, err := gt.agentEngineExec("restart")(ctx, "", map[string]any{}, prog)
		if err == nil || !contains(err.Error(), "wrong agent") || st.posts != 0 {
			t.Fatalf("err=%v posts=%d", err, st.posts)
		}
	})

	t.Run("unknown state refused", func(t *testing.T) {
		st := &engineStub{instID: "fb1", state: "unknown"}
		gt := newEngineGT(t, st)
		_, err := gt.agentEngineExec("restart")(ctx, "", map[string]any{}, prog)
		if err == nil || !contains(err.Error(), "investigate") || st.posts != 0 {
			t.Fatalf("err=%v posts=%d", err, st.posts)
		}
	})

	t.Run("1056 retried exactly once", func(t *testing.T) {
		st := &engineStub{instID: "fb1", state: "running", outcomes: []string{"1056", "ok"}}
		gt := newEngineGT(t, st)
		out, err := gt.agentEngineExec("restart")(ctx, "", map[string]any{}, prog)
		if err != nil || st.posts != 2 || !contains(out, "1056") {
			t.Fatalf("err=%v posts=%d out=%q", err, st.posts, out)
		}
		st2 := &engineStub{instID: "fb1", state: "running", outcomes: []string{"1056", "1056"}}
		gt2 := newEngineGT(t, st2)
		if _, err := gt2.agentEngineExec("restart")(ctx, "", map[string]any{}, prog); err == nil || st2.posts != 2 {
			t.Fatalf("second 1056 must fail without a third try: err=%v posts=%d", err, st2.posts)
		}
	})

	t.Run("work_continues polls the instance, never re-issues", func(t *testing.T) {
		st := &engineStub{instID: "fb1", state: "stopped", outcomes: []string{"work_continues"}}
		gt := newEngineGT(t, st)
		out, err := gt.agentEngineExec("start")(ctx, "", map[string]any{}, prog)
		if err != nil || st.posts != 1 || !contains(out, "work_continues") {
			t.Fatalf("err=%v posts=%d out=%q", err, st.posts, out)
		}
	})

	t.Run("plain failure is not retried", func(t *testing.T) {
		st := &engineStub{instID: "fb1", state: "running", outcomes: []string{"fail"}}
		gt := newEngineGT(t, st)
		if _, err := gt.agentEngineExec("restart")(ctx, "", map[string]any{}, prog); err == nil || st.posts != 1 {
			t.Fatalf("err=%v posts=%d", err, st.posts)
		}
	})

	t.Run("stop post-checks stopped, no tcp probe", func(t *testing.T) {
		st := &engineStub{instID: "fb1", state: "running", afterState: "stopped"}
		gt := newEngineGT(t, st)
		tcpProbe = func(context.Context, int) error { return fmt.Errorf("must not probe on stop") }
		out, err := gt.agentEngineExec("stop")(ctx, "", map[string]any{}, prog)
		if err != nil || !contains(out, "state stopped") {
			t.Fatalf("err=%v out=%q", err, out)
		}
	})

	t.Run("hqclusternode refuses unless overridden", func(t *testing.T) {
		st := &engineStub{instID: "fb1", state: "running"}
		gt := newEngineGT(t, st)
		clusterNodePresent = func() bool { return true }
		if _, err := gt.agentEngineExec("restart")(ctx, "", map[string]any{}, prog); err == nil || !contains(err.Error(), "hqclusternode") || st.posts != 0 {
			t.Fatalf("err=%v posts=%d", err, st.posts)
		}
		if _, err := gt.agentEngineExec("restart")(ctx, "", map[string]any{"cluster_override": true}, prog); err != nil {
			t.Fatalf("override refused: %v", err)
		}
	})
}
