package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aleks/fbmcp/internal/agentcfg"
	"github.com/aleks/fbmcp/internal/config"
)

// Phase 12 test W: with a wrong agent token the executor wrote the
// candidate, then got 401 on reload and said "live config unchanged" —
// but the agent promotes a pending candidate by itself. Nothing may be
// written when the token does not work.
func TestAgentConfigSetBadTokenWritesNothing(t *testing.T) {
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"denied","message":"unauthorized"}}`))
	}))
	t.Cleanup(deny.Close)
	live := []byte(`{"agent": {"id": "fb1"}, "firebird": {"port": 3053}, "scheduler": {"tasks": []}, "sweep": {"enabled": false}}`)
	path := filepath.Join(t.TempDir(), "agent_config.json")
	if err := os.WriteFile(path, live, 0o600); err != nil {
		t.Fatal(err)
	}
	gt, server := newAgentGT(t, func(a *config.Agent) { a.BaseURL = deny.URL; a.ConfigPath = path })
	registerAgentConfigSetTool(server, gt)
	args := map[string]any{"agent": "ag1", "family": "sweep", "patch": map[string]any{"enabled": true}, "base_sha": agentcfg.Fingerprint(live)}
	_, err := gt.execs["fb_agent_config_set"](context.Background(), "", args, func(float64, string) {})
	if err == nil || !strings.Contains(err.Error(), "nothing written") {
		t.Fatalf("err = %v, want a nothing-written refusal", err)
	}
	if p, _ := agentcfg.Pending(path); len(p) > 0 {
		t.Fatalf("candidate written despite the auth failure: %v", p)
	}
}

func TestAgentRefused(t *testing.T) {
	for _, c := range []struct {
		err  string
		want bool
	}{
		{"fbagent bad-request (400, invalid_config): bad cron", true},
		{"fbagent conflict (409, busy): reload running", true},
		{"fbagent auth (401, denied): unauthorized", false},
		{"fbagent service-error (500, x): boom", false},
		{`Post "http://127.0.0.1:13053/v1/config/reload": connection refused`, false},
	} {
		if got := agentRefused(errors.New(c.err)); got != c.want {
			t.Errorf("agentRefused(%q) = %v, want %v", c.err, got, c.want)
		}
	}
}
