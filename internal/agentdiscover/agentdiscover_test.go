package agentdiscover

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigPathFromImage(t *testing.T) {
	cases := map[string]string{
		`"C:\HQbird\fbagent-p3055\fbagent.exe" --run C:\HQbird\fbagent-p3055\agent_config.json`: `C:\HQbird\fbagent-p3055\agent_config.json`,
		`"C:\x\fbagent.exe" --run "C:\with space\agent_config.json"`:                            `C:\with space\agent_config.json`,
		`C:\x\fbagent.exe --run C:\x\agent_config.json --other`:                                 `C:\x\agent_config.json`,
		`C:\x\fbagent.exe`: "",
	}
	for in, want := range cases {
		if got := configPathFromImage(in); got != want {
			t.Errorf("configPathFromImage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEffectiveListen(t *testing.T) {
	cases := []struct {
		listen      string
		apiPort, fb int
		want        string
	}{
		{"127.0.0.1:9999", 0, 3055, "127.0.0.1:9999"},
		{"", 4000, 3055, "127.0.0.1:14000"},
		{"", 0, 3055, "127.0.0.1:13055"},
		{"", 0, 3053, "127.0.0.1:13053"},
		{"", 0, 0, "127.0.0.1:13055"},
	}
	for _, c := range cases {
		if got := effectiveListen(c.listen, c.apiPort, c.fb); got != c.want {
			t.Errorf("effectiveListen(%q,%d,%d) = %q, want %q", c.listen, c.apiPort, c.fb, got, c.want)
		}
	}
}

// fixture builds an agent dir with config + log and returns the service row.
func fixture(t *testing.T, cfgJSON, logLine string) serviceEntry {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent_config.json")
	if cfgJSON != "" {
		if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if logLine != "" {
		if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "logs", "firebird-agent_20261029.jsonl"), []byte(logLine+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return serviceEntry{Name: "HQbirdFBAgent-p3055", ImagePath: `"` + filepath.Join(dir, "fbagent.exe") + `" --run "` + cfgPath + `"`}
}

func TestBuildCandidateFromReadableConfig(t *testing.T) {
	svc := fixture(t, `{"agent":{"id":"host-p3055"},"firebird":{"port":3055},"local_api":{"enabled":true,"listen":"","instance_id":"host-p3055"}}`, "")
	c := buildCandidate(svc)
	if c.BaseURL != "http://127.0.0.1:13055" {
		t.Errorf("base_url = %q", c.BaseURL)
	}
	if c.InstanceID != "host-p3055" {
		t.Errorf("instance_id = %q", c.InstanceID)
	}
	if c.FirebirdPort != 3055 {
		t.Errorf("port = %d", c.FirebirdPort)
	}
	// The config holds a token field in real life — assert we never carry one.
	if strings.Contains(strings.ToLower(strings.Join(c.Notes, " ")), "token") {
		t.Errorf("notes mention a token: %v", c.Notes)
	}
}

func TestBuildCandidateLogLineWins(t *testing.T) {
	svc := fixture(t,
		`{"firebird":{"port":3055},"local_api":{"listen":""}}`,
		`{"msg":"local_api listening on 127.0.0.1:23999 (instance_id=honor-p3055)"}`)
	c := buildCandidate(svc)
	if c.BaseURL != "http://127.0.0.1:23999" {
		t.Errorf("log line should win: base_url = %q", c.BaseURL)
	}
	if c.InstanceID != "honor-p3055" {
		t.Errorf("instance_id = %q", c.InstanceID)
	}
}

func TestBuildCandidateUnreadableConfigFallsBackToServiceName(t *testing.T) {
	svc := fixture(t, "", "") // no config file at all
	c := buildCandidate(svc)
	if c.FirebirdPort != 3055 {
		t.Errorf("port from service suffix = %d", c.FirebirdPort)
	}
	if c.BaseURL != "http://127.0.0.1:13055" {
		t.Errorf("base_url = %q", c.BaseURL)
	}
	if c.InstanceID != "fb1" {
		t.Errorf("default instance_id = %q", c.InstanceID)
	}
	joined := strings.Join(c.Notes, " | ")
	if !strings.Contains(joined, "unreadable") || !strings.Contains(joined, "service name suffix") {
		t.Errorf("notes missing provenance: %v", c.Notes)
	}
}

func TestLocalAPIDisabledNoted(t *testing.T) {
	svc := fixture(t, `{"firebird":{"port":3055},"local_api":{"enabled":false}}`, "")
	c := buildCandidate(svc)
	if !strings.Contains(strings.Join(c.Notes, " "), "local_api.enabled=false") {
		t.Errorf("disabled API not noted: %v", c.Notes)
	}
}
