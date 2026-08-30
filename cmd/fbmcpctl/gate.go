// fbmcpctl gate — the D6.3 §9 walk (phase6_plan_v2.md): lists the state of
// the eight M9 gate items with whatever evidence is mechanically checkable
// from the source tree. It decides NOTHING — every item line is evidence or
// "manual"; the human go/no-go is item 8.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aleks/fbmcp/internal/attach"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/policy"
)

func cmdGate(args []string) int {
	root, err := findRepoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gate: not inside the fbmcp source tree (go.mod not found); use -root <path>")
		if len(args) > 0 && args[0] == "-root" && len(args) > 1 {
			root = args[1]
		} else {
			return 2
		}
	}
	if len(args) >= 2 && args[0] == "-root" {
		root = args[1]
	}

	fmt.Println("M9 gate report (phase6_plan_v2.md §9) — state listing only, decides nothing")
	fmt.Printf("source tree: %s\n\n", root)

	// §9-1: claims C1–C23 each verified-green or accepted-residual
	green, residual, open, openList := claimsState(filepath.Join(root, "docs", "findings", "claims-register.md"))
	verdict(open == 0, fmt.Sprintf("claims: %d verified-green, %d accepted-residual, %d open", green, residual, open),
		strings.Join(openList, "; "))

	// §9-2: zero open critical/high; mediums owned with dates
	// (the register carries no severity column — the open rows above ARE
	// the review queue; govulncheck runs in fbmcp-security.yml)
	secYml := readIfExists(filepath.Join(root, ".github", "workflows", "fbmcp-security.yml"))
	hasVuln := strings.Contains(secYml, "govulncheck")
	verdict(hasVuln && open == 0,
		fmt.Sprintf("zero-open: govulncheck in CI = %v; open register rows = %d (severity triage is manual)", hasVuln, open),
		"critical/high classification of any open row is a human call")

	// §9-3: fuzz corpus committed; govulncheck + SBOM + three-arch checksums
	corpus := countSeedCorpus(root)
	threeArch := strings.Contains(secYml, "GOOS=windows") && strings.Contains(secYml, "GOARCH=arm64") &&
		strings.Contains(secYml, "-trimpath")
	sbom := strings.Contains(secYml, "sbom")
	checksums := strings.Contains(secYml, "SHA256SUMS")
	verdict(len(corpus) > 0 && threeArch && sbom && checksums,
		fmt.Sprintf("fuzz corpus: %d seed files across %d targets; three-arch trimpath=%v sbom=%v checksums=%v",
			total(corpus), len(corpus), threeArch, sbom, checksums),
		detail(corpus))

	// §9-4: chaos seeded suite in nightly; soak green or explicit residual
	chaosScript := fileExists(filepath.Join(root, "packaging", "chaos-nightly.ps1"))
	soakScript := fileExists(filepath.Join(root, "packaging", "soak-week.ps1"))
	chaosLog := fileExists(filepath.Join(root, "docs", "findings", "chaos-log.md"))
	soakReport := fileExists(filepath.Join(root, "docs", "findings", "soak-report.md"))
	verdict(chaosScript && soakScript && soakReport,
		fmt.Sprintf("chaos nightly script=%v, soak script=%v, chaos-log=%v, soak-report=%v (8M/M2+M3 execution pending)",
			chaosScript, soakScript, chaosLog, soakReport),
		"soak needs 7 unattended days; ≥5 clean chaos nights — manual evidence")

	// §9-5: runbook walkthrough
	runbook := fileExists(filepath.Join(root, "docs", "runbook.md"))
	verdict(false,
		fmt.Sprintf("runbook present=%v — walkthrough is MANUAL (fresh host, runbook only)", runbook),
		"8M/M1")

	// §9-6: ADR-025/026 recorded; threat model signed; SECURITY.md published
	adr025 := fileExists(filepath.Join(root, "docs", "decisions", "ADR-025-release-and-disclosure.md"))
	adr026 := fileExists(filepath.Join(root, "docs", "decisions", "ADR-026-security-ci.md"))
	security := fileExists(filepath.Join(root, "SECURITY.md"))
	threat := fileExists(filepath.Join(root, "docs", "findings", "threat-model.md"))
	verdict(adr025 && adr026 && security && threat,
		fmt.Sprintf("ADR-025=%v ADR-026=%v SECURITY.md=%v threat-model=%v (signature is manual)", adr025, adr026, security, threat),
		"sign-off is a human act")

	// §9-7: Appendix A live statement — no silent drift
	if err := toolSurfaceStatement(root); err != nil {
		verdict(false, "tool surface drift: "+err.Error(), "fix README.md / docs/tool-reference.md")
	} else {
		verdict(true, "tool surface: README.md and docs/tool-reference.md state every toolMeta tool; no phantoms (see live counts above)", "")
	}

	// §9-8: go/no-go
	verdict(false, "go/no-go against main plan §1 — MANUAL decision (this report is its input)", "M9")

	fmt.Println("\nNo verdict is expressed; exit code is always 0 (usage errors: 2).")
	return 0
}

func verdict(cond bool, passDetail, failDetail string) {
	mark := "MANUAL"
	switch {
	case cond:
		mark = "EVIDENCED"
	case failDetail == "":
		mark = "PENDING"
	}
	fmt.Printf("[%s] %s\n", mark, passDetail)
	if failDetail != "" {
		fmt.Printf("         %s\n", failDetail)
	}
}

// claimsState parses docs/findings/claims-register.md: table rows whose
// last cell is verified-green / accepted-residual / anything else (open).
func claimsState(path string) (green, residual, open int, openList []string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, 0, []string{"claims-register.md unreadable: " + err.Error()}
	}
	rowRe := regexp.MustCompile(`(?m)^\|\s*(C[0-9a-z]+)\s*\|.*\|\s*([^|*]*)\s*\|\s*$`)
	for _, m := range rowRe.FindAllStringSubmatch(string(b), -1) {
		id, status := m[1], strings.TrimSpace(m[2])
		switch {
		case strings.HasPrefix(status, "verified-green"):
			green++
		case strings.HasPrefix(status, "accepted-residual"):
			residual++
		default:
			open++
			openList = append(openList, id+" (status empty/unparsed)")
		}
	}
	return green, residual, open, openList
}

// countSeedCorpus counts committed seed corpus files per fuzz target.
func countSeedCorpus(root string) map[string]int {
	out := map[string]int{}
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == "testdata" {
			fuzzDir := filepath.Dir(path)
			_ = fuzzDir
			// count files under testdata/fuzz/<Target>/
			base := filepath.Base(path)
			if base != "testdata" {
				return nil
			}
			fuzz := filepath.Join(path, "fuzz")
			ents, err := os.ReadDir(fuzz)
			if err != nil {
				return nil
			}
			for _, e := range ents {
				if !e.IsDir() || !strings.HasPrefix(e.Name(), "Fuzz") {
					continue
				}
				files, _ := os.ReadDir(filepath.Join(fuzz, e.Name()))
				out[e.Name()] += len(files)
			}
		}
		return nil
	})
	return out
}

func total(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func detail(corpus map[string]int) string {
	var parts []string
	for k, v := range corpus {
		parts = append(parts, fmt.Sprintf("%s=%d", k, v))
	}
	if len(parts) == 0 {
		return "no seed corpus found"
	}
	return "targets: " + strings.Join(parts, ", ")
}

// toolSurfaceStatement runs the D6.2 drift logic (same source of truth as
// TestToolSurfaceDrift) and prints the live per-tier counts.
func toolSurfaceStatement(root string) error {
	read := func(rel string) (string, error) {
		b, err := os.ReadFile(filepath.Join(root, rel))
		return string(b), err
	}
	readmeB, err := read("README.md")
	if err != nil {
		return err
	}
	refB, err := read(filepath.Join("docs", "tool-reference.md"))
	if err != nil {
		return err
	}
	readme, ref := string(readmeB), string(refB)
	tools := policy.SystemTools()
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
	perTier := map[int]int{}
	for _, m := range tools {
		perTier[m.Tier]++
		if !strings.Contains(readme, "`"+m.Name+"`") {
			return fmt.Errorf("%s missing from README.md", m.Name)
		}
		if !refTools[m.Name] {
			return fmt.Errorf("%s missing from docs/tool-reference.md", m.Name)
		}
	}
	for name := range refTools {
		found := false
		for _, m := range tools {
			if m.Name == name {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("tool-reference row %s not in the live surface", name)
		}
	}
	fmt.Printf("         live Appendix A statement: %d tools (tier0=%d tier1=%d tier2=%d tier3=%d) — compare with the frozen 93/10+3/8 at M9, do not gate on it\n",
		len(tools), perTier[0], perTier[1], perTier[2], perTier[3])
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func readIfExists(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// findRepoRoot walks up from cwd looking for go.mod with the fbmcp module.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil && strings.Contains(string(b), "module github.com/aleks/fbmcp") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found")
		}
		dir = parent
	}
}

// cmdPing — real readiness check: can a client reach the kernel over the
// attach socket right now? (fbmcpctl status is an offline state.json read;
// it says nothing about a running kernel.) Exit 0 = reachable.
func cmdPing(args []string) int {
	cfgPath := "fbmcp.yaml"
	if len(args) > 0 {
		cfgPath = args[0]
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	timeout := 15 * time.Second
	if len(args) > 1 {
		if d, err := time.ParseDuration(args[1]); err == nil {
			timeout = d
		}
	}
	conn, err := attach.Dial(cfg.State.Dir, timeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ping: no kernel on the attach socket:", err)
		return 1
	}
	conn.Close()
	fmt.Println("kernel reachable")
	return 0
}
