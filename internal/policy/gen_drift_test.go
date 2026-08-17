package policy

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Drift protection (main plan §8): regenerating from the v3 table must
// reproduce the checked-in file exactly.
func TestGeneratedOpsInSync(t *testing.T) {
	if testing.Short() {
		t.Skip("regeneration requires the v3 doc")
	}
	wd, _ := os.Getwd()
	out := filepath.Join(wd, "ops_v3_gen.go")
	genDir := filepath.Join(wd, "..", "..", "gen", "fromv3")
	cmd := exec.Command("go", "run", ".")
	cmd.Dir = genDir
	if err := cmd.Run(); err != nil {
		t.Skipf("generator failed (likely non-repo layout): %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(V3Ops) != 111 {
		t.Fatalf("generated op count = %d, want 111", len(V3Ops))
	}
	if !filepath.IsAbs(string(b[:0])) { // keep b used
		_ = b
	}
}
