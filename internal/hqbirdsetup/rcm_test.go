package hqbirdsetup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aleks/fbmcp/internal/config"
)

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverRCM(t *testing.T) {
	dir := t.TempDir()
	rcm := filepath.Join(dir, "hqbirdrcm", "rcm.json")
	agent := filepath.Join(dir, "fbagent-fb50", "agent_config.json")
	lines := []Line{{AgentConfig: agent}}

	if _, ok, _ := DiscoverRCM(rcm, lines); ok {
		t.Fatal("found without rcm.json")
	}
	if _, ok, _ := DiscoverRCM("", lines); ok {
		t.Fatal("found without a path")
	}
	writeFile(t, rcm, `{"operator_tls":true,"tls":{"ca_cert":"/opt/hqbirdrcm/certs/ca.crt"}}`)
	writeFile(t, filepath.Join(dir, "hqbirdrcm", "initial-admin.json"), `{"username":"rcmadmin","ha1":"x","must_change":true}`)
	writeFile(t, filepath.Join(dir, "fbagent-fb50", "clusterca", "host1", "rcm-initial-password"), "first-pw\n")
	h, ok, notes := DiscoverRCM(rcm, lines)
	if !ok || h.URL != "https://127.0.0.1:7444" || h.CAFile != "/opt/hqbirdrcm/certs/ca.crt" || h.User != "rcmadmin" || h.Password != "first-pw" || len(notes) != 0 {
		t.Fatalf("%+v %v %v", h, ok, notes)
	}

	writeFile(t, rcm, `{"operator_addr":"127.0.0.1:9444"}`)
	h, ok, _ = DiscoverRCM(rcm, nil)
	if !ok || h.URL != "http://127.0.0.1:9444" || h.CAFile != "" || h.Password != "" {
		t.Fatalf("%+v", h)
	}
}

func TestApplyRCM(t *testing.T) {
	host := RCMHost{URL: "https://127.0.0.1:7444", CAFile: "/ca.crt", User: "rcmadmin", Password: "first-pw"}
	none := func(string) bool { return false }
	have := func(string) bool { return true }

	// New: section + secret.
	pl := Plan{Config: &config.Config{}, Secrets: map[string]string{}}
	pl.ApplyRCM(Managed{}, host, true, none)
	if pl.Config.RCM.URL != host.URL || pl.Config.RCM.CAFile != "/ca.crt" || pl.Config.RCM.User != "" || !pl.Managed.RCM {
		t.Fatalf("%+v", pl.Config.RCM)
	}
	if pl.Secrets[config.DefaultRCMPasswordEnv] != "first-pw" || !contains(pl.Managed.Secrets, config.DefaultRCMPasswordEnv) {
		t.Fatalf("secrets %v managed %v", pl.Secrets, pl.Managed.Secrets)
	}

	// The secret exists (changed later): never overwritten, still managed.
	pl = Plan{Config: &config.Config{}, Secrets: map[string]string{}}
	pl.ApplyRCM(Managed{}, host, true, have)
	if _, ok := pl.Secrets[config.DefaultRCMPasswordEnv]; ok || !contains(pl.Managed.Secrets, config.DefaultRCMPasswordEnv) {
		t.Fatalf("secrets %v managed %v", pl.Secrets, pl.Managed.Secrets)
	}

	// The operator's section stays as it is.
	op := config.RCM{URL: "https://127.0.0.1:8444", User: "ops"}
	pl = Plan{Config: &config.Config{RCM: op}, Secrets: map[string]string{}}
	pl.ApplyRCM(Managed{}, host, true, none)
	if pl.Config.RCM != op || pl.Managed.RCM || len(pl.Secrets) != 0 {
		t.Fatalf("operator entry changed: %+v", pl.Config.RCM)
	}

	// Ours, and the RCM is gone: removed.
	pl = Plan{Config: &config.Config{RCM: config.RCM{URL: host.URL}}, Secrets: map[string]string{}}
	pl.ApplyRCM(Managed{RCM: true}, RCMHost{}, false, have)
	if pl.Config.RCM.On() || pl.Managed.RCM || len(pl.Changes) != 1 {
		t.Fatalf("%+v %v", pl.Config.RCM, pl.Changes)
	}
}
