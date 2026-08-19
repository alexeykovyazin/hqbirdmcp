package retention

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/state"
)

func TestKeepEverythingDefault(t *testing.T) {
	st, _ := state.Open(t.TempDir())
	st.AddCatalogEntry(state.CatalogEntry{
		ID: "b1", Database: "spike5", Path: "/tmp/old.fbk", Verified: true,
		CreatedAt: time.Now().Add(-90 * 24 * time.Hour),
	})
	if p := Plan(st, 0, time.Now()); len(p) != 0 {
		t.Fatalf("keep-everything must yield empty plan, got %+v", p)
	}
}

func TestUncatalogedCanarySurvives(t *testing.T) {
	dir := t.TempDir()
	st, _ := state.Open(dir)
	canary := filepath.Join(dir, "FOREIGN.fbk")
	if err := os.WriteFile(canary, []byte("not ours"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "old.fbk")
	if err := os.WriteFile(old, []byte("gbak"), 0o644); err != nil {
		t.Fatal(err)
	}
	st.AddCatalogEntry(state.CatalogEntry{
		ID: "b1", Database: "spike5", Path: old, Verified: true,
		CreatedAt: time.Now().Add(-40 * 24 * time.Hour),
	})
	unverified := filepath.Join(dir, "fresh.fbk")
	os.WriteFile(unverified, []byte("new"), 0o644)
	st.AddCatalogEntry(state.CatalogEntry{
		ID: "b2", Database: "spike5", Path: unverified, Verified: false,
		CreatedAt: time.Now().Add(-40 * 24 * time.Hour),
	})

	now := time.Now()
	plan := Plan(st, 30, now)
	if len(plan) != 1 || plan[0].CatalogID != "b1" {
		t.Fatalf("plan = %+v", plan)
	}
	dry, err := Execute(st, plan, true)
	if err != nil || dry == "" {
		t.Fatalf("dry-run: %v %s", err, dry)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("dry-run deleted a file")
	}
	if _, err := Execute(st, plan, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("verified aged artifact should be gone")
	}
	if _, err := os.Stat(canary); err != nil {
		t.Fatal("uncataloged canary was touched")
	}
	if _, err := os.Stat(unverified); err != nil {
		t.Fatal("unverified artifact was touched")
	}
}

func TestDryRunExecuteParity(t *testing.T) {
	dir := t.TempDir()
	st, _ := state.Open(dir)
	p := Plan(st, 7, time.Now())
	msg, err := Execute(st, p, true)
	if err != nil {
		t.Fatal(err)
	}
	if msg == "" {
		t.Fatal("empty dry-run report")
	}
}
