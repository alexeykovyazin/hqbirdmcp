package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/killpoint"
)

// P1.1 (test_plan): lifecycle table tests for the pieces the restart
// invariants depend on, plus the in-process torn-persist proof.

func mustOpen(t *testing.T, dir string) *Store {
	t.Helper()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestPendingTakeReplaySemantics(t *testing.T) {
	st := mustOpen(t, t.TempDir())
	pa := PendingAction{ID: "p1", Tool: "fb_demo_write", Database: "spike5", Tier: 1, Created: time.Now(), Expires: time.Now().Add(time.Hour)}
	if err := st.AddPending(pa); err != nil {
		t.Fatal(err)
	}
	if len(st.Pending()) != 1 {
		t.Fatal("pending not added")
	}
	got, ok, err := st.TakePending("p1")
	if err != nil || !ok || got.ID != "p1" {
		t.Fatalf("take: %v %v %+v", ok, err, got)
	}
	if len(st.Pending()) != 0 {
		t.Fatal("take did not remove")
	}
	if _, ok, _ := st.TakePending("p1"); ok {
		t.Fatal("double take must miss (single-use)")
	}
	// survives a reopen (the replay invariant)
	st2 := mustOpen(t, st.dir)
	if err := st2.AddPending(pa); err != nil {
		t.Fatal(err)
	}
	if len(mustOpen(t, st.dir).Pending()) != 1 {
		t.Fatal("pending lost across reopen (replay invariant)")
	}
}

func TestJobTransitions(t *testing.T) {
	st := mustOpen(t, t.TempDir())
	j := Job{ID: "j1", Type: "backup", Database: "spike5", State: "queued", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := st.PutJob(j); err != nil {
		t.Fatal(err)
	}
	j.State = "running"
	if err := st.PutJob(j); err != nil {
		t.Fatal(err)
	}
	got, ok := st.Job("j1")
	if !ok || got.State != "running" {
		t.Fatalf("job: %+v %v", got, ok)
	}
	if !st.DBHasLiveJob("spike5") {
		t.Fatal("running job must make DBHasLiveJob true")
	}
	j.State = "interrupted"
	if err := st.PutJob(j); err != nil {
		t.Fatal(err)
	}
	if st.DBHasLiveJob("spike5") {
		t.Fatal("terminal job must not be live")
	}
	if _, ok := st.Job("nope"); ok {
		t.Fatal("unknown job found")
	}
}

func TestWindowsExpiryAndAllDatabases(t *testing.T) {
	st := mustOpen(t, t.TempDir())
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	if st.InWindow("spike5", now) {
		t.Fatal("no window yet")
	}
	if err := st.AddWindow(Window{Database: "spike5", From: now.Add(-time.Hour), To: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddWindow(Window{Database: "", From: now.Add(-time.Hour), To: now.Add(time.Hour)}); err != nil { // wildcard
		t.Fatal(err)
	}
	if !st.InWindow("spike5", now) {
		t.Fatal("inside window")
	}
	if st.InWindow("spike5", now.Add(2*time.Hour)) {
		t.Fatal("window expired but still open")
	}
}

func TestCatalogLatestVerifiedAndLevels(t *testing.T) {
	st := mustOpen(t, t.TempDir())
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	entries := []CatalogEntry{
		{ID: "c1", Database: "spike5", CreatedAt: base, Verified: true, Kind: "gbak"},
		{ID: "c2", Database: "spike5", CreatedAt: base.Add(24 * time.Hour), Verified: true, Kind: "gbak"},
		{ID: "c3", Database: "spike5", CreatedAt: base.Add(48 * time.Hour), Verified: false}, // unverified must not win
		{ID: "c4", Database: "other", CreatedAt: base.Add(72 * time.Hour), Verified: true},
	}
	for _, e := range entries {
		if err := st.AddCatalogEntry(e); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := st.LatestVerifiedBackup("spike5")
	if !ok || !got.Equal(base.Add(24*time.Hour)) {
		t.Fatalf("latest verified: %v %v", got, ok)
	}
	if _, ok := st.LatestVerifiedBackup("nope"); ok {
		t.Fatal("unknown db has no verified backup")
	}
}

func TestWorkflowStateMachine(t *testing.T) {
	st := mustOpen(t, t.TempDir())
	w := Workflow{ID: "wf1", Type: "auto_reopen", Database: "spike5", State: "running",
		Steps: []string{"shut", "online"}, StepStatus: []string{"done", "pending"}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := st.PutWorkflow(w); err != nil {
		t.Fatal(err)
	}
	if len(st.RunningWorkflows()) != 1 {
		t.Fatal("running workflow not listed")
	}
	w.State = "compensating"
	if err := st.PutWorkflow(w); err != nil {
		t.Fatal(err)
	}
	if len(st.RunningWorkflows()) != 1 {
		t.Fatal("compensating counts as running")
	}
	w.State = "succeeded"
	if err := st.PutWorkflow(w); err != nil {
		t.Fatal(err)
	}
	if len(st.RunningWorkflows()) != 0 {
		t.Fatal("terminal workflow still running")
	}
	// survives reopen
	if got, ok := mustOpen(t, st.dir).Workflow("wf1"); !ok || got.State != "succeeded" {
		t.Fatalf("workflow lost across reopen: %+v %v", got, ok)
	}
}

// TestMidPersistNeverTorn is the in-process twin of the killharness
// TestKillAtStateMidPersist: arm the state.mid-persist checkpoint (SetEnabled
// + FBMCP_KILLPOINT_DIR), let a persist block between the fsynced tmp write
// and the rename, assert the on-disk snapshot is still the OLD one, release,
// and assert the rename completes cleanly.
func TestMidPersistNeverTorn(t *testing.T) {
	dir := t.TempDir()
	st := mustOpen(t, dir)
	first := Job{ID: "j1", Type: "backup", Database: "spike5", State: "succeeded", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := st.PutJob(first); err != nil {
		t.Fatal(err)
	}

	kpDir := t.TempDir()
	t.Setenv("FBMCP_KILLPOINT_DIR", kpDir)
	killpoint.SetEnabled(map[string]bool{"state.mid-persist": true})
	t.Cleanup(func() { killpoint.SetEnabled(nil) })

	second := Job{ID: "j2", Type: "backup", Database: "spike5", State: "running", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	var wg sync.WaitGroup
	done := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		done <- st.PutJob(second) // blocks at the checkpoint holding the store lock
	}()

	tmp := filepath.Join(dir, "state.json.tmp")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(tmp); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("persist never reached the mid-persist checkpoint")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// while blocked: state.json is still the OLD snapshot (one job), the tmp
	// file is the NEW one (two jobs), and state.json is valid JSON
	var old struct {
		Jobs []Job `json:"jobs"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &old); err != nil {
		t.Fatalf("state.json torn while mid-persist: %v", err)
	}
	if len(old.Jobs) != 1 {
		t.Fatalf("state.json changed before the rename: %d jobs", len(old.Jobs))
	}
	var neu struct {
		Jobs []Job `json:"jobs"`
	}
	b, err = os.ReadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &neu); err != nil {
		t.Fatalf("tmp file not valid JSON: %v", err)
	}
	if len(neu.Jobs) != 2 {
		t.Fatalf("tmp file missing the new job: %d", len(neu.Jobs))
	}

	// release: the rename completes and the new snapshot becomes current
	if err := os.WriteFile(filepath.Join(kpDir, "state.mid-persist.release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if err := <-done; err != nil {
		t.Fatalf("persist after release: %v", err)
	}
	st2 := mustOpen(t, dir)
	if len(st2.Jobs()) != 2 {
		t.Fatalf("post-release reopen: %d jobs", len(st2.Jobs()))
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("tmp file left behind after rename")
	}
}
