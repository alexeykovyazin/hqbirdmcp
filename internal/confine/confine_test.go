package confine

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDirRejectsDotDot(t *testing.T) {
	if err := Dir(`/tmp/foo/../etc`); err == nil {
		t.Fatal(".. accepted")
	}
}

func TestDirRejectsUNC(t *testing.T) {
	if err := Dir(`\\evil\share\x`); err == nil {
		t.Fatal("UNC accepted")
	}
	if err := Dir(`//evil/share/x`); err == nil {
		t.Fatal("UNC slash accepted")
	}
}

func TestDirRejectsRelative(t *testing.T) {
	if err := Dir("relative/dir"); err == nil {
		t.Fatal("relative accepted")
	}
}

func TestJoinUnder(t *testing.T) {
	root := t.TempDir()
	p, err := JoinUnder(root, "orphan.fdb")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(p) != root {
		t.Fatalf("joined %s", p)
	}
	if _, err := JoinUnder(root, "../etc"); err == nil {
		t.Fatal(".. name accepted")
	}
	if _, err := JoinUnder(root, `a\b`); err == nil && runtime.GOOS == "windows" {
		t.Fatal("separator in name accepted")
	}
	if _, err := JoinUnder(root, "a/b"); err == nil {
		t.Fatal("slash name accepted")
	}
}

func TestDirRefusesSymlink(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not permitted: %v", err)
	}
	if err := Dir(link); err == nil {
		t.Fatal("symlink dir accepted (C9 refuse policy)")
	}
}

func TestWindowsADS(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only")
	}
	root := t.TempDir()
	if _, err := JoinUnder(root, "file.fdb:stream"); err == nil {
		t.Fatal("ADS name accepted")
	}
	if err := Dir(filepath.Join(root, "trailing.")); err == nil {
		t.Fatal("trailing-dot accepted")
	}
}
