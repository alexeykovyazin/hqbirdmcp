package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func logSome(t *testing.T, dir string, n int) string {
	t.Helper()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := l.Log(Entry{
			Identity: "tester", Database: "spike5", Tool: "fb_demo",
			Tier: 0, Decision: "allow",
			Detail: map[string]interface{}{"i": i, "note": "ok"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(dir, "audit.jsonl")
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestChainVerifies(t *testing.T) {
	dir := t.TempDir()
	p := logSome(t, dir, 5)
	if line, err := Verify(p); err != nil || line != 0 {
		t.Fatalf("clean chain failed: line=%d err=%v", line, err)
	}
}

func TestResumeChain(t *testing.T) {
	dir := t.TempDir()
	logSome(t, dir, 3)
	// reopen and append (server restart)
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Log(Entry{Identity: "x", Tool: "t", Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if line, err := Verify(filepath.Join(dir, "audit.jsonl")); err != nil || line != 0 {
		t.Fatalf("resumed chain failed: line=%d err=%v", line, err)
	}
}

func TestTamperDetected(t *testing.T) {
	dir := t.TempDir()
	p := logSome(t, dir, 4)
	b, _ := os.ReadFile(p)
	lines := strings.Split(string(b), "\n")
	// tamper line 2's detail
	lines[1] = strings.Replace(lines[1], `"note":"ok"`, `"note":"EVIL"`, 1)
	os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o640)
	if line, err := Verify(p); err == nil || line == 0 {
		t.Fatal("tampering not detected")
	} else if line != 2 {
		t.Fatalf("tamper detected at wrong line: %d (%v)", line, err)
	}
}

func TestTruncationDetected(t *testing.T) {
	dir := t.TempDir()
	p := logSome(t, dir, 4)
	b, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	os.WriteFile(p, []byte(strings.Join(lines[:3], "\n")+"\n"), 0o640)
	if _, err := Verify(p); err == nil {
		t.Fatal("truncation not detected")
	}
}

func TestSecretScrubbing(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = l.Log(Entry{
		Identity: "x", Tool: "t", Decision: "allow",
		Detail: map[string]interface{}{
			"password":      "hunter2",
			"argv_template": "gbak -user SYSDBA password=secret -b db out",
		},
	})
	l.Close()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if strings.Contains(string(b), "hunter2") || strings.Contains(string(b), "password=secret") {
		t.Fatalf("secret leaked into audit log:\n%s", b)
	}
}
