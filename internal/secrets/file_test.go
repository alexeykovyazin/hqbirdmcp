package secrets

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func withDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := Dir()
	SetDir(dir)
	t.Cleanup(func() { SetDir(old) })
	return dir
}

// B0.9: env → file → keyring.
func TestLookupOrder(t *testing.T) {
	store := withStubStore(t)
	dir := withDir(t)
	(*store)["FBMCP_K"] = "from-keyring"
	os.Unsetenv("FBMCP_K")

	if v, src, err := Lookup("FBMCP_K"); err != nil || v != "from-keyring" || src != SourceKeyring {
		t.Fatalf("no file: %q %s %v", v, src, err)
	}
	if err := WriteFile(dir, "FBMCP_K", "from-file"); err != nil {
		t.Fatal(err)
	}
	if v, src, err := Lookup("FBMCP_K"); err != nil || v != "from-file" || src != SourceFile {
		t.Fatalf("file: %q %s %v", v, src, err)
	}
	t.Setenv("FBMCP_K", "from-env")
	if v, src, err := Lookup("FBMCP_K"); err != nil || v != "from-env" || src != SourceEnv {
		t.Fatalf("env: %q %s %v", v, src, err)
	}
}

func TestWriteFileTrimAndRotate(t *testing.T) {
	dir := t.TempDir()
	if err := WriteFile(dir, "K1", "  v1  "); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(dir, "K1", "v2"); err != nil {
		t.Fatal(err)
	}
	if v, err := ReadFile(dir, "K1"); err != nil || v != "v2" {
		t.Fatalf("after rotate: %q %v", v, err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("temp files left behind: %d entries", len(ents))
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(dir, "K1"))
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
		}
	}
	if err := DropFile(dir, "K1"); err != nil {
		t.Fatal(err)
	}
}

func TestFileRefusals(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"", "../x", "a.b", "a/b", "1abc"} {
		if err := WriteFile(dir, bad, "v"); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	if err := WriteFile(dir, "K", "   "); err == nil {
		t.Error("blank value accepted")
	}
	if err := WriteFile(filepath.Join(dir, "missing"), "K", "v"); err == nil {
		t.Error("missing dir accepted")
	}
	os.WriteFile(filepath.Join(dir, "EMPTY"), []byte("\n"), 0o600)
	if _, err := ReadFile(dir, "EMPTY"); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty file: %v", err)
	}
	if runtime.GOOS != "windows" {
		p := filepath.Join(dir, "OPEN")
		os.WriteFile(p, []byte("v"), 0o644)
		os.Chmod(p, 0o644)
		if _, err := ReadFile(dir, "OPEN"); err == nil {
			t.Error("world-readable secret file accepted")
		}
	}
}

// A broken file must not fall through to a keyring value silently.
func TestLookupBrokenFileReported(t *testing.T) {
	store := withStubStore(t)
	dir := withDir(t)
	os.Unsetenv("FBMCP_B")
	os.WriteFile(filepath.Join(dir, "FBMCP_B"), []byte(" \n"), 0o600)
	if _, _, err := Lookup("FBMCP_B"); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("broken file, no keyring: %v", err)
	}
	(*store)["FBMCP_B"] = "kr"
	if v, src, err := Lookup("FBMCP_B"); err != nil || src != SourceKeyring || v != "kr" {
		t.Fatalf("broken file with keyring: %q %s %v", v, src, err)
	}
}
