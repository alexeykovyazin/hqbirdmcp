package policy

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Drift protection (main plan §8): regenerating ops_v3_gen.go from the v3
// table must reproduce the checked-in file byte-for-byte. (WS8 T8.3 — the
// previous guard pointed at a non-existent gen/ dir, ran the generator with
// the wrong CWD, skipped on failure and compared only the row count, so
// same-count drift passed and the wrong-path skip fired forever.)
func TestGeneratedOpsInSync(t *testing.T) {
	if testing.Short() {
		t.Skip("regeneration requires the v3 table")
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller info")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	genDir := filepath.Join(repoRoot, "internal", "gen", "fromv3")
	if _, err := os.Stat(filepath.Join(genDir, "main.go")); err != nil {
		t.Skipf("generator not present (%v) — non-repo layout", err)
	}
	// the v3 table lives in the planning corpus next to the fbmcp repo;
	// FBMCP_MD overrides (the generator's own flag), CI without the corpus
	// skips rather than pretending to check.
	src := os.Getenv("FBMCP_MD")
	if src == "" {
		src = filepath.Join(repoRoot, "..", "firebird_dba_tasks_table_v3.md")
	}
	if _, err := os.Stat(src); err != nil {
		t.Skipf("v3 table not present (%v) — set FBMCP_MD to run this check", err)
	}
	genFile := filepath.Join(repoRoot, "internal", "policy", "ops_v3_gen.go")
	before, err := os.ReadFile(genFile)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "run", "./internal/gen/fromv3")
	// the generator writes internal/policy/ops_v3_gen.go relative to CWD,
	// so it must run from the repo root (the ./internal/... pattern above
	// resolves from there too)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "FBMCP_MD="+src)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("generator failed: %v; stderr: %s", err, strings.TrimSpace(stderr.String()))
	}
	after, err := os.ReadFile(genFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		bl := strings.Split(string(before), "\n")
		al := strings.Split(string(after), "\n")
		n := len(bl)
		if len(al) < n {
			n = len(al)
		}
		first := 0
		for ; first < n && bl[first] == al[first]; first++ {
		}
		t.Fatalf("ops_v3_gen.go drifted from firebird_dba_tasks_table_v3.md (%d -> %d lines; first difference at line %d):\n  - %s\n  + %s\nRegenerate with: go run ./internal/gen/fromv3 (FBMCP_MD=<v3 table>) and commit",
			len(bl), len(al), first+1, lineOr(bl, first), lineOr(al, first))
	}
	if len(V3Ops) != 111 {
		t.Fatalf("generated op count = %d, want 111", len(V3Ops))
	}
}

func lineOr(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return "<eof>"
}
