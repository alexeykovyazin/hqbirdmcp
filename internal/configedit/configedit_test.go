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

// Phase 12 test W: fb_config_get showed "DefaultDbCachePages = 40K" and
// fb_config_set refused the same value as "not an int".
func TestValidateIntSuffix(t *testing.T) {
	for _, v := range []string{"2048", "40K", "40k", "64M", "1G", " 8K "} {
		if _, err := ValidateSet("DefaultDbCachePages", v); err != nil {
			t.Errorf("%q refused: %v", v, err)
		}
	}
	for _, v := range []string{"", "K", "40KB", "4.5K", "x", "9999999999999G"} {
		if _, err := ValidateSet("DefaultDbCachePages", v); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
	if n, err := fbInt("40K"); err != nil || n != 40960 {
		t.Fatalf("fbInt(40K) = %d, %v", n, err)
	}
}

// Phase 12 test W: a set re-rendered every line, so the HQbird
// firebird.conf (CRLF, indented lines, inline comments) would lose them
// all, and ";" cut TempDirectories. Only the set line may change.
func TestApplyKeepsOtherLines(t *testing.T) {
	src := "# header\r\n  DefaultDbCachePages = 40K      # pages\r\nTempDirectories = c:\\temp;d:\\temp\r\n\tWireCrypt = Enabled # keep\r\n"
	f := Parse("x", src)
	if v, _ := f.Get("TempDirectories"); v != `c:\temp;d:\temp` {
		t.Fatalf("TempDirectories = %q", v)
	}
	got := f.Apply("DefaultDbCachePages", "64K").String()
	want := "# header\r\n  DefaultDbCachePages = 64K      # pages\r\nTempDirectories = c:\\temp;d:\\temp\r\n\tWireCrypt = Enabled # keep\r\n"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if out := f.Apply("TempCacheLimit", "64M").String(); !strings.HasSuffix(out, "WireCrypt = Enabled # keep\r\nTempCacheLimit = 64M\r\n") {
		t.Fatalf("append: %q", out)
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
	// Windows 3+: conf beside the binaries.
	win := t.TempDir()
	if err := os.WriteFile(filepath.Join(win, "firebird.conf"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ConfPath(win); got != filepath.Join(win, "firebird.conf") {
		t.Fatalf("ConfPath(win)=%q", got)
	}
	if got := DatabasesConfPath(win); got != filepath.Join(win, "databases.conf") {
		t.Fatalf("DatabasesConfPath(win)=%q", got)
	}
	// Linux: binaries in <root>/bin, conf in <root>.
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "firebird.conf"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ConfPath(bin); got != filepath.Join(root, "firebird.conf") {
		t.Fatalf("ConfPath(linux)=%q", got)
	}
	if got := DatabasesConfPath(bin); got != filepath.Join(root, "databases.conf") {
		t.Fatalf("DatabasesConfPath(linux)=%q", got)
	}
}
