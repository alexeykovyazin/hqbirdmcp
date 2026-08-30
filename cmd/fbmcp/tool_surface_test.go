package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestToolSurfaceDrift (D6.2 / gate §9 item 7): the live toolMeta surface
// must be stated exactly once in README.md and docs/tool-reference.md —
// every toolMeta name appears in both, and every fb_* table row in the
// tool reference exists in toolMeta (the reverse, phantom direction, is
// covered by TestPlaybookHonestyLint). The per-tier counts logged at the
// end are the LIVE Appendix A statement the M9 gate compares against the
// frozen phase6 numbers (93/10+3/8) — intentionally not gated on them.
func TestToolSurfaceDrift(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller info")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		return string(b)
	}
	readme := read("README.md")
	ref := read("docs/tool-reference.md")

	// first table cell of every "| `fb_xxx` | ..." row
	refTools := map[string]bool{}
	for _, line := range strings.Split(ref, "\n") {
		if !strings.HasPrefix(line, "| `fb_") {
			continue
		}
		cell := strings.TrimPrefix(line, "| `")
		if i := strings.Index(cell, "`"); i > 0 {
			refTools[cell[:i]] = true
		}
	}

	inToolMeta := map[string]bool{}
	perTier := map[int]int{}
	for _, m := range toolMeta {
		inToolMeta[m.Name] = true
		perTier[m.Tier]++

		if !strings.Contains(readme, "`"+m.Name+"`") {
			t.Errorf("%s missing from README.md (every toolMeta name must appear there)", m.Name)
		}
		if !refTools[m.Name] {
			t.Errorf("%s missing from docs/tool-reference.md table", m.Name)
		}
	}
	for name := range refTools {
		if !inToolMeta[name] {
			t.Errorf("tool-reference row %s does not exist in toolMeta", name)
		}
	}

	t.Logf("live tool surface: %d tools (tier0=%d tier1=%d tier2=%d tier3=%d)",
		len(toolMeta), perTier[0], perTier[1], perTier[2], perTier[3])
}
