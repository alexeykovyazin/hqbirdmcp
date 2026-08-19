package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestN1StateWithoutSchedulesLoads(t *testing.T) {
	dir := t.TempDir()
	body := `{"pending":[],"windows":[],"catalog":[],"jobs":[]}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Schedules()) != 0 {
		t.Fatal("expected empty schedules")
	}
}

func TestCorruptStateFailClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o640); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir)
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("corrupt store accepted: %v", err)
	}
}

func TestDropPendingForKeepsRegister(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = st.AddPending(PendingAction{ID: "p1", Database: "spike5", Tool: "fb_backup_start"})
	_ = st.AddPending(PendingAction{ID: "p2", Database: "fb5", Tool: "fb_db_register"})
	got := st.DropPendingFor([]string{"spike5", "fb5"}, nil)
	if len(got) != 1 || got[0] != "p1" {
		t.Fatalf("dropped=%v", got)
	}
	if len(st.Pending()) != 1 || st.Pending()[0].ID != "p2" {
		t.Fatalf("register pending dropped: %+v", st.Pending())
	}
	got = st.DropPendingFor(nil, []string{"fb5"})
	if len(got) != 1 || got[0] != "p2" {
		t.Fatalf("instance drop=%v", got)
	}
}

func TestRemapDatabaseID(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = st.PutSchedule(Schedule{ID: "s1", Database: "old"})
	if err := st.RemapDatabaseID("old", "new"); err != nil {
		t.Fatal(err)
	}
	if st.Schedules()[0].Database != "new" {
		t.Fatalf("%+v", st.Schedules())
	}
	if err := st.RemapDatabaseID("new", ""); err != nil {
		t.Fatal(err)
	}
	if len(st.Schedules()) != 0 {
		t.Fatalf("expected delete, got %+v", st.Schedules())
	}
}
