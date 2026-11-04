package agentdiscover

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixtures: real output captured on clawnuc3 (Ubuntu, fbagent 2.79, 2026-11-03).
const (
	fxListUnits = "hqbird.service            loaded active running HQBird Agent\n" +
		"hqbirdfbagent.service     loaded active running HQbird Firebird Agent\n" +
		"hqbirdmonitor.service     loaded active running HQbird Monitor (local console for agent clawnuc3260831)\n"
	fxUnitFiles = "hqbirdfbagent.service enabled enabled\n"
	fxExecStart = "{ path=/opt/hqbird-fbagent/fbagent ; argv[]=/opt/hqbird-fbagent/fbagent --run /opt/hqbird-fbagent/agent_config.json ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }\n"
	fxExecOther = "{ path=/opt/hqbird/hqbird ; argv[]=/opt/hqbird/hqbird -d ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }\n"
	fxJournal   = "local_api listening on 127.0.0.1:13050 (instance_id=s2lab)\nother line\nlocal_api listening on 127.0.0.1:13050 (instance_id=clawnuc3260831)\n"
)

func fakeRunner(t *testing.T, execStart map[string]string, journal string) cmdRunner {
	return func(name string, args ...string) ([]byte, error) {
		cmd := name + " " + strings.Join(args, " ")
		switch {
		case strings.HasPrefix(cmd, "systemctl list-units"):
			return []byte(fxListUnits), nil
		case strings.HasPrefix(cmd, "systemctl list-unit-files") && strings.Contains(cmd, "clusternode"):
			return nil, nil
		case strings.HasPrefix(cmd, "systemctl list-unit-files"):
			return []byte(fxUnitFiles), nil
		case strings.HasPrefix(cmd, "systemctl show"):
			unit := args[len(args)-1]
			if v, ok := execStart[unit]; ok {
				return []byte(v), nil
			}
			return []byte(fxExecOther), nil
		case strings.HasPrefix(cmd, "journalctl"):
			return []byte(journal), nil
		}
		return nil, fmt.Errorf("unexpected command %q", cmd)
	}
}

func TestParseExecStart(t *testing.T) {
	bin, argv := parseExecStart(fxExecStart)
	if bin != "/opt/hqbird-fbagent/fbagent" || !isFbagentBinary(bin) {
		t.Fatalf("bin %q", bin)
	}
	if configPathFromImage(argv) != "/opt/hqbird-fbagent/agent_config.json" {
		t.Fatalf("argv %q → config %q", argv, configPathFromImage(argv))
	}
	if b, _ := parseExecStart(fxExecOther); isFbagentBinary(b) {
		t.Fatal("hqbird binary taken for fbagent")
	}
}

// clawnuc3 shape: the config is not readable here, the journal has the
// listen line → base_url and instance id come from the newest line.
func TestDiscoverSystemdClawnuc3(t *testing.T) {
	run := fakeRunner(t, map[string]string{"hqbirdfbagent.service": fxExecStart}, fxJournal)
	cands, notes, err := discoverSystemd(run)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || len(notes) != 0 {
		t.Fatalf("cands %+v notes %v", cands, notes)
	}
	c := cands[0]
	if c.Service != "hqbirdfbagent" || c.ConfigPath != "/opt/hqbird-fbagent/agent_config.json" {
		t.Fatalf("candidate %+v", c)
	}
	if c.BaseURL != "http://127.0.0.1:13050" || c.InstanceID != "clawnuc3260831" {
		t.Fatalf("from the journal: base_url %s instance %s", c.BaseURL, c.InstanceID)
	}
}

// R7 shape: fbmcp runs as firebird and reads the config (port + instance).
func TestDiscoverSystemdReadableConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "agent_config.json")
	os.WriteFile(cfg, []byte(`{"agent":{"id":"a1"},"firebird":{"port":3050},"local_api":{"instance_id":"nuc3-p3050","token":"never-decoded"}}`), 0o600)
	exec := strings.ReplaceAll(fxExecStart, "/opt/hqbird-fbagent/agent_config.json", filepath.ToSlash(cfg))
	run := fakeRunner(t, map[string]string{"hqbirdfbagent.service": exec}, "")
	cands, _, err := discoverSystemd(run)
	if err != nil || len(cands) != 1 {
		t.Fatalf("%v %+v", err, cands)
	}
	c := cands[0]
	if c.BaseURL != "http://127.0.0.1:13050" || c.InstanceID != "nuc3-p3050" || c.FirebirdPort != 3050 {
		t.Fatalf("candidate %+v", c)
	}
	for _, n := range c.Notes {
		if strings.Contains(n, "token") || strings.Contains(n, "never-decoded") {
			t.Fatalf("token leaked into a note: %q", n)
		}
	}
}
