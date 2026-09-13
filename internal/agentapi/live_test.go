package agentapi

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// Live suite against a real fbagent Local API (phase 9M/9L). Opt-in:
//
//	FBMCP_AGENT_LIVE=1 \
//	FBMCP_AGENT_LIVE_URL=http://127.0.0.1:13055 \
//	FBMCP_AGENT_LIVE_TOKEN_ENV=FBMCP_AGENT_TOKEN \
//	go test ./internal/agentapi/ -run Live -v
//
// GET-only by rule (see phase9_live_test_plan.md §2): the cmd/fbmcp live
// suite is the one that mutates agent state, and `go test ./...` runs
// packages in parallel.
func liveClient(t *testing.T) *Client {
	t.Helper()
	if os.Getenv("FBMCP_AGENT_LIVE") != "1" {
		t.Skip("FBMCP_AGENT_LIVE not set")
	}
	base := os.Getenv("FBMCP_AGENT_LIVE_URL")
	if base == "" {
		base = "http://127.0.0.1:13055"
	}
	tokEnv := os.Getenv("FBMCP_AGENT_LIVE_TOKEN_ENV")
	if tokEnv == "" {
		tokEnv = "FBMCP_AGENT_TOKEN"
	}
	tok, err := os.ReadFile("../../.agent-token") // optional local convenience
	tokStr := os.Getenv(tokEnv)
	if tokStr == "" && err == nil {
		tokStr = string(tok)
	}
	if tokStr == "" {
		t.Skipf("no token in %s", tokEnv)
	}
	cl, err := New(base, tokStr)
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

func TestLiveInstancesAndHealth(t *testing.T) {
	cl := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, err := cl.GetInstances(ctx, "")
	if err != nil {
		t.Fatalf("instances: %v", err)
	}
	t.Logf("instances: %s", raw)
	// The list is a bare JSON array on the lab (probed 2026-09-13) — the
	// client is RawMessage-based precisely so either shape decodes.
	var bare []map[string]any
	var wrapped struct {
		Instances []map[string]any `json:"instances"`
	}
	switch {
	case json.Unmarshal(raw, &bare) == nil && len(bare) > 0:
		t.Logf("bare-array list, first instance state=%v", bare[0]["state"])
	case json.Unmarshal(raw, &wrapped) == nil && len(wrapped.Instances) > 0:
		t.Logf("wrapped list, first instance state=%v", wrapped.Instances[0]["state"])
	default:
		t.Fatalf("instances list in neither bare-array nor wrapped shape: %s", raw)
	}
	health, err := cl.GetStatus(ctx, "", "health")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	t.Logf("health: %s", health)
}

// TestLiveUpdateSidecarShape accepts either documented outcome: 404 while
// the firebird-update state sidecar does not exist, or a parsable state
// document once it does.
func TestLiveUpdateSidecarShape(t *testing.T) {
	cl := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	inst := os.Getenv("FBMCP_AGENT_LIVE_INSTANCE")
	if inst == "" {
		inst = "fb-master-3055"
	}
	raw, err := cl.GetUpdateStatus(ctx, inst)
	if err != nil {
		if strings.Contains(err.Error(), "fbagent not-found") {
			t.Log("sidecar absent (documented 404) — negative shape holds")
			return
		}
		t.Fatalf("update status: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("sidecar present but unparsable: %v", err)
	}
	t.Logf("sidecar present: %s", raw)
}

func TestLiveArtifactListing(t *testing.T) {
	cl := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, err := cl.GetArtifactList(ctx, "server", "*")
	if err != nil {
		t.Fatalf("artifact list: %v", err)
	}
	t.Logf("server artifacts: %s", raw)
}

func TestLiveDBAccessInventory(t *testing.T) {
	cl := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, err := cl.GetDBAccess(ctx, liveInstance())
	if err != nil {
		t.Fatalf("database-access: %v", err)
	}
	t.Logf("dbaccess: %s", raw)
	// auth round-trip: a bad token must produce the auth marker
	bad, err := New(liveBase(t), "definitely-wrong")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.GetDBAccess(ctx, liveInstance()); err == nil || !strings.Contains(err.Error(), "fbagent auth") {
		t.Fatalf("bad token err = %v", err)
	}
}

func liveInstance() string {
	if v := os.Getenv("FBMCP_AGENT_LIVE_INSTANCE"); v != "" {
		return v
	}
	return "fb-master-3055"
}

func liveBase(t *testing.T) string {
	base := os.Getenv("FBMCP_AGENT_LIVE_URL")
	if base == "" {
		base = "http://127.0.0.1:13055"
	}
	return base
}
