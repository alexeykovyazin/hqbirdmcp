package instlock

import "testing"

func TestC4DifferentStateDirAllowed(t *testing.T) {
	a, err := Acquire(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, err := Acquire(t.TempDir())
	if err != nil {
		t.Fatalf("second process with a different state dir must succeed: %v", err)
	}
	b.Release()
}
