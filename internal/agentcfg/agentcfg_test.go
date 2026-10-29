package agentcfg

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const liveDoc = `{
  "agent": {"id": "fb-3055", "auth_token": "SECRET-A"},
  "firebird": {"port": 3055, "credentials": {"password_encrypted": "c2VjcmV0"}},
  "local_api": {"token": "SECRET-B", "port": 3055},
  "future_section": {"keep": [1, 2, 3]},
  "scheduler": {"tasks": [
    {"name": "vbackup", "command": "", "args": [], "enabled": true, "schedule": "0 0 2 * * *", "timeout_seconds": 0},
    {"name": "sweep", "command": "", "enabled": false, "schedule": "0 0 3 * * *"}
  ]},
  "vbackup": {
    "agentBackupDir": "C:\\b",
    "individual": {"enabled": true, "jobs": [
      {"id": "db1", "name": "Main", "enabled": true, "cronExpression": "0 0 1 * * ?",
       "retention": {"maxBackups": 3},
       "spaceFactors": {"freeSpaceDbSizeFactor": 1.5},
       "postProcessing": {"enabled": false, "command": "C:\\x.cmd"},
       "copy": {"ntlmPassword": "SECRET-C"},
       "minFreeSpaceBytes": 9007199254740993}
    ]},
    "group": {"enabled": false, "jobs": []}
  },
  "sweep": {"enabled": true, "execution_window": {"weekdays": [1, 2], "start": "01:00"}}
}`

func TestApplyJobPatchPreservesEverythingElse(t *testing.T) {
	r, err := Apply([]byte(liveDoc), "vbackup", "db1", "", map[string]any{
		"cronExpression": "0 30 1 * * ?",
		"retention":      map[string]any{"maxBackups": float64(7)},
		"spaceFactors":   map[string]any{"freeSpaceDbSizeFactor": 2.25},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Target != "vbackup.individual.jobs[id=db1]" || len(r.Diff) != 3 {
		t.Fatalf("target/diff = %s %v", r.Target, r.Diff)
	}
	out := string(r.Doc)
	for _, keep := range []string{"SECRET-A", "SECRET-B", "SECRET-C", "c2VjcmV0", `"future_section"`, "9007199254740993", `"maxBackups": 7`, `"freeSpaceDbSizeFactor": 2.25`, `"0 30 1 * * ?"`} {
		if !strings.Contains(out, keep) {
			t.Errorf("candidate lost %s", keep)
		}
	}
	if bytes.HasPrefix(r.Doc, []byte{0xEF, 0xBB, 0xBF}) {
		t.Error("BOM written")
	}
	for _, d := range r.Diff {
		if strings.Contains(d, "SECRET") {
			t.Errorf("diff leaks a secret: %s", d)
		}
	}
}

func TestApplyRefusals(t *testing.T) {
	cases := []struct {
		name, family, job, task string
		patch                   map[string]any
		want                    string
	}{
		{"family not allowed", "local_api", "", "", map[string]any{"port": 1}, "not patchable"},
		{"sql_script excluded", "sql_script", "", "", map[string]any{"x": 1}, "not patchable"},
		{"unknown key", "vbackup", "db1", "", map[string]any{"cronExpresion": "x"}, "unknown setting"},
		{"type change", "vbackup", "db1", "", map[string]any{"enabled": "yes"}, "true/false"},
		{"float into int", "vbackup", "db1", "", map[string]any{"retention": map[string]any{"maxBackups": 2.5}}, "integer"},
		{"secret", "vbackup", "db1", "", map[string]any{"copy": map[string]any{"ntlmPassword": "x"}}, "operator-only"},
		{"identity", "vbackup", "db1", "", map[string]any{"id": "db2"}, "identity"},
		{"command", "vbackup", "db1", "", map[string]any{"postProcessing": map[string]any{"command": "evil"}}, "program"},
		{"enable command block", "vbackup", "db1", "", map[string]any{"postProcessing": map[string]any{"enabled": true}}, "runs a command"},
		{"replace jobs", "vbackup", "", "", map[string]any{"individual": map[string]any{"jobs": []any{}}}, "job lists"},
		{"null", "vbackup", "db1", "", map[string]any{"enabled": nil}, "null"},
		{"placeholder", "vbackup", "", "", map[string]any{"agentBackupDir": "***"}, "placeholder"},
		{"missing job", "vbackup", "nope", "", map[string]any{"enabled": false}, "not found"},
		{"no-op", "vbackup", "db1", "", map[string]any{"enabled": true}, "changes nothing"},
		{"task key", "scheduler", "", "vbackup", map[string]any{"command": "x"}, "not allowed"},
		{"task w/o name", "scheduler", "", "", map[string]any{"enabled": true}, "pass task"},
		{"task outside scheduler", "sweep", "", "sweep", map[string]any{"enabled": true}, "only to family scheduler"},
	}
	for _, c := range cases {
		_, err := Apply([]byte(liveDoc), c.family, c.job, c.task, c.patch)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestApplySchedulerTaskAndFamilyRoot(t *testing.T) {
	r, err := Apply([]byte(liveDoc), "scheduler", "", "sweep", map[string]any{"enabled": true, "schedule": "0 0 4 * * *"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Target != "scheduler.tasks[name=sweep]" || len(r.Diff) != 2 {
		t.Fatalf("%s %v", r.Target, r.Diff)
	}
	r, err = Apply([]byte(liveDoc), "sweep", "", "", map[string]any{"execution_window": map[string]any{"weekdays": []any{float64(6), float64(7)}}})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(r.Doc, &doc); err != nil {
		t.Fatal(err)
	}
	wd := doc["sweep"].(map[string]any)["execution_window"].(map[string]any)["weekdays"].([]any)
	if len(wd) != 2 || wd[0].(float64) != 6 {
		t.Fatalf("weekdays = %v", wd)
	}
}

func TestApplyRejectsFragment(t *testing.T) {
	if _, err := Apply([]byte(`{"vbackup":{"enabled":true}}`), "vbackup", "", "", map[string]any{"enabled": false}); err == nil {
		t.Fatal("fragment accepted as a base")
	}
}

func TestWriteCandidateAndPending(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "agent_config.json")
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	n1, err := WriteCandidate(cfg, []byte("{}\n"), now)
	if err != nil {
		t.Fatal(err)
	}
	n2, err := WriteCandidate(cfg, []byte("{}\n"), now)
	if err != nil {
		t.Fatal(err)
	}
	if n1 != "agent_config_pending_20261029_120000.json" || n2 != "agent_config_pending_20261029_120000-2.json" {
		t.Fatalf("names = %s %s", n1, n2)
	}
	p, _ := Pending(cfg)
	if len(p) != 2 {
		t.Fatalf("pending = %v", p)
	}
	_ = os.WriteFile(filepath.Join(dir, n1+".superseded"), []byte("x"), 0o644)
	p, _ = Pending(cfg)
	if len(p) != 1 || p[0] != n2 {
		t.Fatalf("pending after marker = %v", p)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left: %s", e.Name())
		}
	}
	if _, err := WriteCandidate(cfg, []byte{0xEF, 0xBB, 0xBF, '{', '}'}, now); err == nil {
		t.Error("BOM candidate accepted")
	}
}
