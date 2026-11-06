package ctl

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/gate"
	"github.com/aleks/fbmcp/internal/state"
)

// noKernel keeps the tests off the attach socket (a 2 s dial each).
func noKernel(t *testing.T, up bool) {
	t.Helper()
	old := kernelUp
	kernelUp = func(string) bool { return up }
	t.Cleanup(func() { kernelUp = old })
}

func withStdin(t *testing.T, s string) {
	t.Helper()
	old := promptIn
	promptIn = strings.NewReader(s)
	t.Cleanup(func() { promptIn = old })
}

func pendingDir(t *testing.T, id string) string {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := st.AddPending(state.PendingAction{ID: id, Created: now, Expires: now.Add(time.Hour), Identity: "alice",
		Database: "emp", Tool: "fb_sweep", Tier: 2, ImpactText: "Sweep emp"}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Lab 2026-11-04: approving an expired request id must fail (exit 1) and
// write no marker; listing an empty queue is still fine (exit 0).
func TestApproveUnknownIDFails(t *testing.T) {
	noKernel(t, true)
	dir := t.TempDir()
	if rc := cmdApprove([]string{dir}); rc != 0 {
		t.Fatalf("list on an empty queue: rc=%d, want 0", rc)
	}
	if rc := cmdApprove([]string{dir, "8f132fa83adf6743a0855275", "--yes"}); rc != 1 {
		t.Fatalf("approve unknown id: rc=%d, want 1", rc)
	}
	if _, err := os.Stat(filepath.Join(dir, "approvals", "8f132fa83adf6743a0855275")); !os.IsNotExist(err) {
		t.Fatalf("marker written for an unknown id (err=%v)", err)
	}
}

// UX review W1.5: a wrong path is an error, never a new empty queue.
func TestApproveNeverCreatesTheStateDir(t *testing.T) {
	noKernel(t, true)
	missing := filepath.Join(t.TempDir(), "no-such-state")
	cfg := filepath.Join(t.TempDir(), "fbmcp.yaml")
	if err := os.WriteFile(cfg, []byte("state:\n  dir: "+filepath.ToSlash(missing)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc := cmdApprove([]string{"--config", cfg}); rc != 1 {
		t.Fatalf("approve on a missing state dir: rc=%d, want 1", rc)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("approve created %s (err=%v)", missing, err)
	}
}

func TestApproveAsksFirst(t *testing.T) {
	noKernel(t, true)
	dir := pendingDir(t, "r1")
	marker := filepath.Join(dir, "approvals", "r1")

	withStdin(t, "n\n")
	if rc := cmdApprove([]string{dir, "r1"}); rc != 1 {
		t.Fatalf("answer n: rc=%d, want 1", rc)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("marker written after answer n")
	}

	withStdin(t, "y\n")
	if rc := cmdApprove([]string{dir, "r1"}); rc != 0 {
		t.Fatalf("answer y: rc=%d, want 0", rc)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), gate.MarkerApproved+"\n") {
		t.Fatalf("marker body %q: the first line must stay the tray's verdict line", b)
	}
}

func TestApproveDenyWithReason(t *testing.T) {
	noKernel(t, false) // deny works with the kernel down
	dir := pendingDir(t, "r2")
	if rc := cmdApprove([]string{dir, "--deny", "r2", "--reason", "not during\nbusiness hours", "--yes"}); rc != 0 {
		t.Fatalf("deny: rc=%d", rc)
	}
	b, err := os.ReadFile(filepath.Join(dir, "denials", "r2"))
	if err != nil {
		t.Fatal(err)
	}
	if _, reason := gate.ParseMarker(b); reason != "not during business hours" {
		t.Fatalf("reason = %q (marker %q)", reason, b)
	}
	if _, err := os.Stat(filepath.Join(dir, "approvals", "r2")); !os.IsNotExist(err) {
		t.Fatal("deny wrote an approval")
	}
	if rc := cmdApprove([]string{dir, "r2", "--deny", "r2"}); rc != 2 {
		t.Fatalf("id and --deny together: rc=%d, want 2", rc)
	}
}

func TestResolveConfigOrder(t *testing.T) {
	cwd := t.TempDir()
	exeDir := t.TempDir()
	hq := filepath.Join(t.TempDir(), "fbmcp.yaml")
	t.Chdir(cwd)
	oldExe, oldDef := exePath, defaultConfig
	exePath = func() (string, error) { return filepath.Join(exeDir, "fbmcp.exe"), nil }
	defaultConfig = func() string { return hq }
	t.Cleanup(func() { exePath, defaultConfig = oldExe, oldDef })
	t.Setenv("FBMCP_CONFIG", "")

	if _, err := resolveConfig(""); err == nil || !strings.Contains(err.Error(), "Fix:") {
		t.Fatalf("nothing found: err=%v, want a Fix line", err)
	}
	touch := func(p string) {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	touch(hq)
	if got, _ := resolveConfig(""); got != hq {
		t.Fatalf("HQbird default: got %q", got)
	}
	touch(filepath.Join(exeDir, "fbmcp.yaml"))
	if got, _ := resolveConfig(""); got != filepath.Join(exeDir, "fbmcp.yaml") {
		t.Fatalf("next to the program: got %q", got)
	}
	touch(filepath.Join(cwd, "fbmcp.yaml"))
	if got, _ := resolveConfig(""); got != "fbmcp.yaml" {
		t.Fatalf("current folder: got %q", got)
	}
	t.Setenv("FBMCP_CONFIG", "from-env.yaml")
	if got, _ := resolveConfig(""); got != "from-env.yaml" {
		t.Fatalf("FBMCP_CONFIG: got %q", got)
	}
	if got, _ := resolveConfig("given.yaml"); got != "given.yaml" {
		t.Fatalf("explicit: got %q", got)
	}
}

func TestTakeConfigFlag(t *testing.T) {
	for _, in := range [][]string{{"--config", "a.yaml", "x"}, {"x", "--config=a.yaml"}, {"-config", "a.yaml", "x"}} {
		cfg, rest := takeConfigFlag(in)
		if cfg != "a.yaml" || len(rest) != 1 || rest[0] != "x" {
			t.Errorf("%v: cfg=%q rest=%v", in, cfg, rest)
		}
	}
}

func TestHelp(t *testing.T) {
	out := captureStdout(t, func() {
		if rc := Main([]string{"help"}); rc != 0 {
			t.Fatalf("help rc=%d", rc)
		}
	})
	for _, c := range commands {
		if !strings.Contains(out, "  "+c.name+" ") {
			t.Errorf("help table misses %s", c.name)
		}
	}
	out = captureStdout(t, func() {
		if rc := Main([]string{"doctor", "-h"}); rc != 0 {
			t.Fatalf("doctor -h rc=%d", rc)
		}
	})
	if !strings.Contains(out, "usage: fbmcp ctl doctor [--config FILE]") {
		t.Fatalf("doctor -h: %q", out)
	}
	if rc := Main([]string{"no-such-command"}); rc != 2 {
		t.Fatalf("unknown command rc=%d, want 2", rc)
	}
}

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	defer func() { os.Stdout = old }()
	f()
	w.Close()
	os.Stdout = old
	return <-done
}
