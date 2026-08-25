package main

import (
	"github.com/aleks/fbmcp/internal/state"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// phase8_plan D1.2b: repair rebuilds enriched-era schedule grants from the
// audit chain, refuses a healthy state, and moves a corrupt state aside
// instead of destroying it.
func writeRepairFixture(t *testing.T, dir string) string {
	t.Helper()
	audit := `{"time":"2026-08-25T10:00:00Z","identity":"alice","database":"spike5","tool":"fb_schedule_create","tier":1,"decision":"approved","channel":"in-band-token","detail":{"schedule_id":"sch1","target":"nightly_verify","creating_request":"r1","cron":"0 3 * * *","timezone":"UTC","kind":"workflow","missed_run":"skip","window_required":false,"args":"null","arg_hash":"abc"}}
{"time":"2026-08-25T10:05:00Z","identity":"alice","database":"spike5","tool":"fb_schedule_create","tier":1,"decision":"approved","channel":"in-band-token","detail":{"schedule_id":"sch0","target":"nightly_verify","creating_request":"r0"}}
{"time":"2026-08-25T10:10:00Z","identity":"alice","database":"spike5","tool":"fb_ping","tier":0,"decision":"allow"}
`
	if err := os.WriteFile(filepath.Join(dir, "audit.jsonl"), []byte(audit), 0o640); err != nil {
		t.Fatal(err)
	}
	cfg := "state:\n    dir: " + filepath.ToSlash(dir) + "\ninstances:\n    - id: fb5\n      addr: localhost:3055\n      bin_dir: C:/HQbird/Firebird50\n"
	p := filepath.Join(dir, "cfg.yaml")
	if err := os.WriteFile(p, []byte(cfg), 0o640); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRepairRebuildsEnrichedGrants(t *testing.T) {
	dir := t.TempDir()
	cfg := writeRepairFixture(t, dir)
	if code := cmdRepair([]string{cfg}); code != 0 {
		t.Fatalf("repair exit %d", code)
	}
	st := openState(t, dir)
	if len(st.Schedules()) != 1 {
		t.Fatalf("expected 1 rebuilt grant, got %+v", st.Schedules())
	}
	sc := st.Schedules()[0]
	if sc.ID != "sch1" || sc.Target != "nightly_verify" || sc.Cron != "0 3 * * *" ||
		sc.ArgHash != "abc" || sc.Confirmer != "alice" || !sc.Enabled {
		t.Fatalf("grant rebuilt wrong: %+v", sc)
	}
}

func TestRepairRefusesHealthyState(t *testing.T) {
	dir := t.TempDir()
	cfg := writeRepairFixture(t, dir)
	st := openState(t, dir)
	if err := st.PutSchedule(scheduleFor("existing")); err != nil {
		t.Fatal(err)
	}
	if code := cmdRepair([]string{cfg}); code == 0 {
		t.Fatal("repair must refuse a loadable non-empty state")
	}
}

func TestRepairMovesCorruptStateAside(t *testing.T) {
	dir := t.TempDir()
	cfg := writeRepairFixture(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"pending":[ {"id":`), 0o640); err != nil {
		t.Fatal(err)
	}
	if code := cmdRepair([]string{cfg}); code != 0 {
		t.Fatalf("repair exit %d", code)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "state.json.pre-repair-*"))
	if len(matches) != 1 {
		t.Fatalf("corrupt state not moved aside: %v", matches)
	}
	if len(openState(t, dir).Schedules()) != 1 {
		t.Fatal("grant not rebuilt after moving corrupt state aside")
	}
	if strings.Contains(readFile(t, filepath.Join(dir, "state.json")), "sch1") == false {
		// sanity: new state contains the rebuilt grant
		t.Fatal("new state.json lacks the rebuilt grant")
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func openState(t *testing.T, dir string) *state.Store {
	t.Helper()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func scheduleFor(id string) state.Schedule {
	return state.Schedule{ID: id, Database: "spike5", Target: "fb_backup_start", Kind: "tool",
		Cron: "* * * * *", Timezone: "UTC", Enabled: true}
}
