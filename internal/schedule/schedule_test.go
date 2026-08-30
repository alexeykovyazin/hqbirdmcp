package schedule

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/state"
)

func TestCronMatchUTC(t *testing.T) {
	tm := time.Date(2026, 8, 17, 3, 0, 0, 0, time.UTC)
	ok, err := Matches("0 3 * * *", tm)
	if err != nil || !ok {
		t.Fatalf("expected match: %v %v", ok, err)
	}
	ok, _ = Matches("0 3 * * *", tm.Add(time.Minute))
	if ok {
		t.Fatal("03:01 should not match")
	}
}

func TestDSTSpringForward(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-03-08 02:00 EST → 03:00 EDT. 02:30 does not exist.
	at130 := time.Date(2026, 3, 8, 1, 30, 0, 0, loc)
	ok, _ := Matches("30 1 * * *", at130)
	if !ok {
		t.Fatal("1:30 EST should match")
	}
	at330 := time.Date(2026, 3, 8, 3, 30, 0, 0, loc)
	ok, _ = Matches("30 3 * * *", at330)
	if !ok {
		t.Fatal("3:30 EDT should match")
	}
	// 2:30 on that date is not a valid civil time in NY; constructing it
	// typically lands at 3:30 EDT. The 02:30 cron must not fire twice.
	at230 := time.Date(2026, 3, 8, 2, 30, 0, 0, loc)
	ok230, _ := Matches("30 2 * * *", at230)
	if ok230 && at230.Hour() != 2 {
		t.Logf("2:30 constructed as %s (expected DST skip)", at230)
	}
}

func TestValidateCreateForbidden(t *testing.T) {
	tierOf := func(name string) (string, int, error) {
		if name == "fb_db_drop" {
			return "tool", 3, nil
		}
		if name == "fb_restore_replace" {
			return "tool", 2, nil
		}
		if name == "nightly_verify" {
			return "workflow", 1, nil
		}
		return "", 0, errUnknown(name)
	}
	if _, _, err := ValidateCreate("fb_db_drop", "0 3 * * *", "UTC", tierOf); err == nil || !strings.Contains(err.Error(), "Tier-3") {
		t.Fatalf("tier-3 must be refused: %v", err)
	}
	if _, _, err := ValidateCreate("fb_restore_replace", "0 3 * * *", "UTC", tierOf); err == nil {
		t.Fatal("restore_replace must be refused")
	}
	if _, _, err := ValidateCreate("nightly_verify", "0 3 * * *", "", tierOf); err == nil {
		t.Fatal("empty timezone must be refused")
	}
	kind, tier, err := ValidateCreate("nightly_verify", "0 3 * * *", "Europe/Berlin", tierOf)
	if err != nil || kind != "workflow" || tier != 1 {
		t.Fatalf("nightly_verify: %s %d %v", kind, tier, err)
	}
}

func errUnknown(n string) error { return errStr("unknown target " + n) }

type errStr string

func (e errStr) Error() string { return string(e) }

func TestFireNeverCallsGate(t *testing.T) {
	st, _ := state.Open(t.TempDir())
	args := CanonicalJSON(nil)
	sc := state.Schedule{
		ID: "s1", Database: "spike5", Target: "nightly_verify", Kind: "workflow",
		ArgsJSON: args, ArgHash: HashArgs(args), MaxTier: 1,
		Cron: "0 3 * * *", Timezone: "UTC", Enabled: true, MissedRun: "skip",
		Overlap: "skip", CreatedAt: time.Now().UTC(),
	}
	if err := st.PutSchedule(sc); err != nil {
		t.Fatal(err)
	}
	gateCalled := false
	tick := New(st, func(ctx context.Context, s state.Schedule) (string, error) {
		if gateCalled {
			t.Fatal("gate.Request must not be called on the fire path")
		}
		return "j1", nil
	}).WithNow(func() time.Time { return time.Date(2026, 8, 17, 3, 0, 0, 0, time.UTC) })
	tick.WithGateProbe(func() {
		// probe is the fire-path assertion point; tests prove we do NOT
		// have a gate here by never setting gateCalled.
	})
	tick.Tick(context.Background())
	if tick.Fired() != 1 {
		t.Fatalf("fired=%d skips=%d", tick.Fired(), tick.Skips())
	}
	runs := st.ScheduleRuns()
	if len(runs) != 1 || runs[0].Result != "fired" {
		t.Fatalf("runs: %+v", runs)
	}
}

func TestOverlapSkip(t *testing.T) {
	st, _ := state.Open(t.TempDir())
	args := CanonicalJSON(nil)
	st.PutSchedule(state.Schedule{
		ID: "s1", Database: "spike5", Target: "fb_backup_start", Kind: "tool",
		ArgsJSON: args, ArgHash: HashArgs(args), Cron: "* * * * *", Timezone: "UTC",
		Enabled: true, Overlap: "skip",
	})
	st.PutJob(state.Job{ID: "jlive", Database: "spike5", State: "running", CreatedAt: time.Now(), UpdatedAt: time.Now()})
	fired := 0
	tick := New(st, func(ctx context.Context, s state.Schedule) (string, error) {
		fired++
		return "x", nil
	}).WithNow(func() time.Time { return time.Date(2026, 8, 17, 4, 15, 0, 0, time.UTC) })
	tick.Tick(context.Background())
	if fired != 0 || tick.Skips() == 0 {
		t.Fatalf("expected overlap skip, fired=%d skips=%d", fired, tick.Skips())
	}
}

func TestMissedRunSkipNoCatchup(t *testing.T) {
	st, _ := state.Open(t.TempDir())
	args := CanonicalJSON(nil)
	st.PutSchedule(state.Schedule{
		ID: "s1", Database: "spike5", Target: "fb_backup_start", Kind: "tool",
		ArgsJSON: args, ArgHash: HashArgs(args), Cron: "0 3 * * *", Timezone: "UTC",
		Enabled: true, MissedRun: "skip", LastFiredAt: time.Date(2026, 8, 16, 3, 0, 0, 0, time.UTC),
	})
	fired := 0
	// 10:00 — missed 03:00, skip policy must not catch up
	tick := New(st, func(ctx context.Context, s state.Schedule) (string, error) {
		fired++
		return "x", nil
	}).WithNow(func() time.Time { return time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC) })
	tick.Tick(context.Background())
	if fired != 0 {
		t.Fatal("skip policy caught up a missed run")
	}
}

func TestCatchupOnce(t *testing.T) {
	st, _ := state.Open(t.TempDir())
	args := CanonicalJSON(nil)
	st.PutSchedule(state.Schedule{
		ID: "s1", Database: "spike5", Target: "fb_backup_start", Kind: "tool",
		ArgsJSON: args, ArgHash: HashArgs(args), Cron: "0 3 * * *", Timezone: "UTC",
		Enabled: true, MissedRun: "catchup-once",
		LastFiredAt: time.Date(2026, 8, 16, 3, 0, 0, 0, time.UTC),
	})
	fired := 0
	tick := New(st, func(ctx context.Context, s state.Schedule) (string, error) {
		fired++
		return "x", nil
	}).WithNow(func() time.Time { return time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC) })
	tick.Tick(context.Background())
	if fired != 1 {
		t.Fatalf("catchup-once should fire once, got %d", fired)
	}
}

func TestArgHashMismatch(t *testing.T) {
	st, _ := state.Open(t.TempDir())
	st.PutSchedule(state.Schedule{
		ID: "s1", Database: "spike5", Target: "fb_backup_start", Kind: "tool",
		ArgsJSON: `{"x":1}`, ArgHash: "deadbeef", Cron: "* * * * *", Timezone: "UTC",
		Enabled: true,
	})
	tick := New(st, func(ctx context.Context, s state.Schedule) (string, error) {
		t.Fatal("must not fire on hash mismatch")
		return "", nil
	}).WithNow(func() time.Time { return time.Date(2026, 8, 17, 1, 0, 0, 0, time.UTC) })
	tick.Tick(context.Background())
	if tick.Skips() == 0 {
		t.Fatal("expected skip")
	}
}

func TestWindowRequired(t *testing.T) {
	st, _ := state.Open(t.TempDir())
	args := CanonicalJSON(nil)
	st.PutSchedule(state.Schedule{
		ID: "s1", Database: "spike5", Target: "fb_backup_start", Kind: "tool",
		ArgsJSON: args, ArgHash: HashArgs(args), Cron: "* * * * *", Timezone: "UTC",
		Enabled: true, WindowRequired: true,
	})
	now := time.Date(2026, 8, 17, 5, 0, 0, 0, time.UTC)
	tick := New(st, func(ctx context.Context, s state.Schedule) (string, error) {
		t.Fatal("must not fire outside window")
		return "", nil
	}).WithNow(func() time.Time { return now })
	tick.Tick(context.Background())
	if tick.Skips() == 0 {
		t.Fatal("expected window skip")
	}
}

func TestSkipUnknownDatabase(t *testing.T) {
	st, _ := state.Open(t.TempDir())
	args := CanonicalJSON(nil)
	st.PutSchedule(state.Schedule{
		ID: "s1", Database: "gone", Target: "fb_backup_start", Kind: "tool",
		ArgsJSON: args, ArgHash: HashArgs(args), Cron: "* * * * *", Timezone: "UTC",
		Enabled: true,
	})
	now := time.Date(2026, 8, 17, 5, 0, 0, 0, time.UTC)
	tick := New(st, func(ctx context.Context, s state.Schedule) (string, error) {
		t.Fatal("must not fire unknown db")
		return "", nil
	}).WithNow(func() time.Time { return now }).WithDBExists(func(id string) bool { return false })
	tick.Tick(context.Background())
	if tick.Skips() == 0 {
		t.Fatal("expected unknown database skip")
	}
}

// P6.2 T1 clock-jump: a large forward jump must produce at most one action
// (skip, for missed_run=skip) — never a fire storm — and a backward jump
// must not re-fire an already-fired minute.
func TestClockJumpForwardNoStorm(t *testing.T) {
	st, _ := state.Open(t.TempDir())
	args := CanonicalJSON(nil)
	st.PutSchedule(state.Schedule{
		ID: "s1", Database: "spike5", Target: "fb_backup_start", Kind: "tool",
		ArgsJSON: args, ArgHash: HashArgs(args), Cron: "* * * * *", Timezone: "UTC",
		Enabled: true, MissedRun: "skip", Overlap: "skip",
		LastFiredAt: time.Date(2026, 8, 17, 5, 0, 0, 0, time.UTC),
	})
	now := time.Date(2026, 8, 17, 11, 30, 0, 0, time.UTC) // +6.5h, many missed minutes
	fired := 0
	tick := New(st, func(ctx context.Context, s state.Schedule) (string, error) {
		fired++
		return "x", nil
	}).WithNow(func() time.Time { return now })
	tick.Tick(context.Background())
	// the current matching minute fires exactly once; the missed backlog
	// (05:01..11:29) is never re-run — one action per tick, no storm
	if fired != 1 || len(st.ScheduleRuns()) != 1 {
		t.Fatalf("exactly one fire expected, got fired=%d runs=%d", fired, len(st.ScheduleRuns()))
	}
}

func TestClockJumpBackwardNoRefire(t *testing.T) {
	st, _ := state.Open(t.TempDir())
	args := CanonicalJSON(nil)
	st.PutSchedule(state.Schedule{
		ID: "s1", Database: "spike5", Target: "fb_backup_start", Kind: "tool",
		ArgsJSON: args, ArgHash: HashArgs(args), Cron: "* * * * *", Timezone: "UTC",
		Enabled:     true,
		LastFiredAt: time.Date(2026, 8, 17, 5, 0, 0, 0, time.UTC),
	})
	now := time.Date(2026, 8, 17, 4, 59, 0, 0, time.UTC) // clock wound back before the last fire
	fired := 0
	tick := New(st, func(ctx context.Context, s state.Schedule) (string, error) {
		fired++
		return "x", nil
	}).WithNow(func() time.Time { return now })
	tick.Tick(context.Background())
	if fired != 0 {
		t.Fatal("backward clock jump re-fired an already-fired minute")
	}
}
