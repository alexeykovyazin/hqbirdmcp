package killpoint

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHitNoOpWithoutEnv(t *testing.T) {
	SetEnabled(nil)
	done := make(chan struct{})
	go func() { Hit("anything"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Hit blocked with no checkpoints armed")
	}
}

func TestHitReadyThenRelease(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FBMCP_KILLPOINT_DIR", dir)
	SetEnabled(map[string]bool{"cp1": true})

	hitDone := make(chan struct{})
	go func() { Hit("cp1"); close(hitDone) }()

	ready := filepath.Join(dir, "cp1.ready")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ready marker never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := os.WriteFile(filepath.Join(dir, "cp1.release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-hitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Hit did not return after release")
	}
	if _, err := os.Stat(filepath.Join(dir, "cp1.release")); !os.IsNotExist(err) {
		t.Fatal("release marker not consumed")
	}
}

func TestHitNotArmedName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FBMCP_KILLPOINT_DIR", dir)
	SetEnabled(map[string]bool{"other": true})
	done := make(chan struct{})
	go func() { Hit("cp1"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Hit blocked for a checkpoint that is not armed")
	}
}
