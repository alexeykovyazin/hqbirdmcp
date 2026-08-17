package instlock

import (
	"strings"
	"testing"
)

// Safety fuse #6 (main plan §8): a second concurrent instance must fail fast
// on the state lock — no dual writers.
func TestFuse6SecondInstanceFails(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	defer first.Release()

	second, err := Acquire(dir)
	if err == nil {
		second.Release()
		t.Fatal("FUSE FAILURE: second instance acquired the same state lock")
	}
	if !strings.Contains(err.Error(), "another fbmcp instance") {
		t.Fatalf("error is not the clear fail-fast message: %v", err)
	}
}

func TestReleaseAllowsReacquire(t *testing.T) {
	dir := t.TempDir()
	l, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	l2, err := Acquire(dir)
	if err != nil {
		t.Fatalf("reacquire after release failed: %v", err)
	}
	l2.Release()
}
