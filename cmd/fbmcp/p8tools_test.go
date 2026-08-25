package main

import (
	"strings"
	"testing"

	"github.com/aleks/fbmcp/internal/config"
)

// phase8_plan D3.3 acceptance: resource builders never emit secret values
// (the config holds environment-variable names only, and the builders
// enumerate explicit non-secret fields).
func TestResourcesRedactSecrets(t *testing.T) {
	cfg := &config.Config{
		Listen: "localhost:8443",
	}
	cfg.State.Dir = t.TempDir()
	reg := registryResource(cfg)
	pol := policyResource()
	conf := configResource(cfg)
	for name, out := range map[string]string{"registry": reg, "policy": pol, "config": conf} {
		for _, banned := range []string{"masterkey", "ISC_PASSWORD", "FBMCP_DEV_PW"} {
			if strings.Contains(strings.ToLower(out), strings.ToLower(banned)) {
				t.Fatalf("%s resource leaks %q", name, banned)
			}
		}
	}
	if !strings.Contains(pol, "fb_ping tier=0") || !strings.Contains(pol, "fb_db_drop tier=3") {
		t.Fatalf("policy resource missing tools: %q", pol)
	}
}
