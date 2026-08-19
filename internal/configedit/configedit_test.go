package configedit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
