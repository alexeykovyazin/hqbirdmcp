package configedit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseRoundTripComments(t *testing.T) {
	src := "# header\n\nDefaultDbCachePages = 2048\n# keep me\nWireCrypt = Enabled\n"
	f := Parse("x", src)
	out := f.String()
	if !strings.Contains(out, "# header") || !strings.Contains(out, "# keep me") {
		t.Fatalf("lost comments: %q", out)
	}
	if v, ok := f.Get("WireCrypt"); !ok || v != "Enabled" {
		t.Fatalf("get %q %v", v, ok)
	}
}

func TestValidateUnknown(t *testing.T) {
	if _, err := ValidateSet("NotARealParam", "1"); err == nil {
		t.Fatal("unknown accepted")
	}
}

func TestValidateWireCrypt(t *testing.T) {
	p, err := ValidateSet("WireCrypt", "Required")
	if err != nil || !p.Security || !p.Restart {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := ValidateSet("WireCrypt", "maybe"); err == nil {
		t.Fatal("bad enum")
	}
}

func TestAtomicWriteKeepsPrev(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "firebird.conf")
	if err := os.WriteFile(path, []byte("A = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(path, "A = 2\n"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "A = 2\n" {
		t.Fatalf("new %q", b)
	}
	prev, _ := os.ReadFile(path + ".prev")
	if string(prev) != "A = 1\n" {
		t.Fatalf("prev %q", prev)
	}
}

func TestApplyInPlace(t *testing.T) {
	f := Parse("x", "WireCrypt = Enabled\n")
	g := f.Apply("WireCrypt", "Required")
	if v, _ := g.Get("WireCrypt"); v != "Required" {
		t.Fatal(v)
	}
	if v, _ := f.Get("WireCrypt"); v != "Enabled" {
		t.Fatal("mutated original")
	}
}

// P1.5 (test_plan): fuller round-trips, journal, path helpers.

const sampleConf = `# firebird.conf (sample)
# comment line
DefaultDbCachePages = 2048

; semicolon comment
TempCacheLimit	200000000
WireCrypt = Required # inline comment
UnknownGarbageLine
AuthServer = Legacy_Auth, Srp
`

func TestParsePreservesAndResolves(t *testing.T) {
	f := Parse("firebird.conf", sampleConf)
	if v, ok := f.Get("DefaultDbCachePages"); !ok || v != "2048" {
		t.Fatalf("Get cache: %q %v", v, ok)
	}
	if v, ok := f.Get("tempcachelimit"); !ok || v != "200000000" { // tab separator, case-insensitive
		t.Fatalf("Get temp: %q %v", v, ok)
	}
	if v, ok := f.Get("WireCrypt"); !ok || v != "Required" { // inline comment stripped
		t.Fatalf("Get wirecrypt: %q %v", v, ok)
	}
	if v, ok := f.Get("AuthServer"); !ok || v != "Legacy_Auth, Srp" {
		t.Fatalf("Get authserver: %q %v", v, ok)
	}
	// last assignment wins
	dup := Parse("x", "A = 1\nA = 2\n")
	if v, _ := dup.Get("A"); v != "2" {
		t.Fatalf("dup Get=%q", v)
	}
	// round-trip: re-parsing the rendered output is stable
	again := Parse("firebird.conf", f.String())
	for _, k := range []string{"DefaultDbCachePages", "TempCacheLimit", "WireCrypt", "AuthServer"} {
		a, _ := f.Get(k)
		b, _ := again.Get(k)
		if a != b {
			t.Fatalf("round-trip drift for %s: %q != %q", k, a, b)
		}
	}
	// Apply replaces in place, keeps the rest
	out := f.Apply("DefaultDbCachePages", "4096").Apply("BugcheckAbort", "true")
	if v, _ := out.Get("DefaultDbCachePages"); v != "4096" {
		t.Fatalf("apply existing: %q", v)
	}
	if v, _ := out.Get("BugcheckAbort"); v != "true" {
		t.Fatalf("apply new: %q", v)
	}
	if v, _ := out.Get("WireCrypt"); v != "Required" {
		t.Fatalf("apply clobbered a sibling: %q", v)
	}
	if v, _ := f.Get("DefaultDbCachePages"); v != "2048" {
		t.Fatal("Apply mutated the receiver")
	}
}

func TestAppendJournalLine(t *testing.T) {
	dir := t.TempDir()
	if err := AppendJournal(dir, "fb3", "C:/fb/firebird.conf", "WireCrypt", "Enabled", "Required"); err != nil {
		t.Fatal(err)
	}
	if err := AppendJournal(dir, "fb3", "C:/fb/firebird.conf", "WireCrypt", "Required", "Disabled"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "config-journal", "fb3.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 journal lines, got %d", len(lines))
	}
	var rec map[string]string
	if err := json.Unmarshal([]byte(lines[1]), &rec); err != nil {
		t.Fatalf("journal line not JSON: %v", err)
	}
	for k, want := range map[string]string{
		"instance": "fb3", "file": "C:/fb/firebird.conf", "param": "WireCrypt",
		"old": "Required", "new": "Disabled",
	} {
		if rec[k] != want {
			t.Fatalf("journal %s=%q want %q", k, rec[k], want)
		}
	}
	if _, err := time.Parse(time.RFC3339, rec["ts"]); err != nil {
		t.Fatalf("journal ts not RFC3339: %v", err)
	}
}

func TestConfPaths(t *testing.T) {
	if got := ConfPath("C:/fb/bin"); got != filepath.Join("C:/fb/bin", "firebird.conf") {
		t.Fatalf("ConfPath=%q", got)
	}
	if got := DatabasesConfPath("C:/fb/bin"); got != filepath.Join("C:/fb/bin", "databases.conf") {
		t.Fatalf("DatabasesConfPath=%q", got)
	}
}
