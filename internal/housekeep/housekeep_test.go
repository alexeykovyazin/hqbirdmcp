package housekeep

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRotateCapsSize(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "server.log")
	if err := os.WriteFile(p, make([]byte, 64), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Rotate(p, 32, 2); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi != nil && fi.Size() != 0 {
		t.Fatalf("log not truncated, size=%d", fi.Size())
	}
	if _, err := os.Stat(p + ".1"); err != nil {
		t.Fatal("missing rotated .1")
	}
}

func TestOrphanCleanupAge(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "orphan.fdb")
	os.WriteFile(old, []byte("x"), 0o644)
	past := time.Now().Add(-48 * time.Hour)
	os.Chtimes(old, past, past)
	fresh := filepath.Join(dir, "fresh.fdb")
	os.WriteFile(fresh, []byte("y"), 0o644)
	n, err := CleanOrphans(dir, 24*time.Hour, []string{".fdb", ".pre-restore"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("cleaned %d, want 1", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old orphan survived")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh file removed")
	}
}

func TestCleanOrphansRefusesSymlinkDir(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "worklink")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not permitted: %v", err)
	}
	if _, err := CleanOrphans(link, time.Hour, []string{".fdb"}); err == nil {
		t.Fatal("symlink work dir accepted (C9)")
	}
}
