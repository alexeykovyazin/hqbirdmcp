package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/state"
)

func newRunner(t *testing.T) (*Runner, *state.Store) {
	t.Helper()
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := NewRunner(st)
	t.Cleanup(r.Close)
	return r, st
}

func TestJobSucceeds(t *testing.T) {
	r, _ := newRunner(t)
	id, err := r.Submit("backup", "spike5", "local", "", func(ctx context.Context, p func(float64, string)) (string, error) {
		p(0.5, "halfway")
		return "done: 42 rows", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { j, _ := r.Status(id); return j.State == "succeeded" })
	j, _ := r.Status(id)
	if j.Message != "done: 42 rows" || j.Progress != 1 {
		t.Fatalf("bad result: %+v", j)
	}
}

func TestCancelMidFlight(t *testing.T) {
	r, _ := newRunner(t)
	id, _ := r.Submit("sweep", "spike5", "local", "", func(ctx context.Context, p func(float64, string)) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(10 * time.Second):
			return "finished", nil
		}
	})
	waitFor(t, func() bool { j, _ := r.Status(id); return j.State == "running" })
	if err := r.Cancel(id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { j, _ := r.Status(id); return j.State == "cancelled" })
}

func TestPerDBSingleFlight(t *testing.T) {
	r, _ := newRunner(t)
	release := make(chan struct{})
	first, _ := r.Submit("backup", "spike5", "local", "", func(ctx context.Context, p func(float64, string)) (string, error) {
		<-release
		return "first done", nil
	})
	waitFor(t, func() bool { j, _ := r.Status(first); return j.State == "running" })

	second, _ := r.Submit("restore", "spike5", "local", "", func(ctx context.Context, p func(float64, string)) (string, error) {
		return "second done", nil
	})
	// different DB must not block
	other, _ := r.Submit("backup", "spike3", "local", "", func(ctx context.Context, p func(float64, string)) (string, error) {
		return "other done", nil
	})
	waitFor(t, func() bool { j, _ := r.Status(other); return j.State == "succeeded" })
	if j, _ := r.Status(second); j.State != "queued" {
		t.Fatalf("overlapping same-DB job did not queue: %+v", j)
	}
	close(release)
	waitFor(t, func() bool { j, _ := r.Status(first); return j.State == "succeeded" })
	firstDone, _ := r.Status(first)
	waitFor(t, func() bool { j, _ := r.Status(second); return j.State == "succeeded" })
	secondDone, _ := r.Status(second)
	// serialization proof: second cannot have started before first finished
	if !secondDone.UpdatedAt.After(firstDone.UpdatedAt) {
		t.Fatalf("second job overlapped first: first done %v, second done %v", firstDone.UpdatedAt, secondDone.UpdatedAt)
	}
}

func TestRestartMarksInterrupted(t *testing.T) {
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// simulate a crashed process having left a running job
	st.PutJob(state.Job{ID: "j1", Type: "backup", Database: "spike5", State: "running", CreatedAt: time.Now(), UpdatedAt: time.Now()})
	r := NewRunner(st)
	defer r.Close()
	j, ok := r.Status("j1")
	if !ok || j.State != "interrupted" {
		t.Fatalf("running job not marked interrupted after restart: %+v", j)
	}
}

func TestJobSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	st, _ := state.Open(dir)
	r := NewRunner(st)
	id, _ := r.Submit("backup", "spike5", "local", "", okFunc)
	waitFor(t, func() bool { j, _ := r.Status(id); return j.State == "succeeded" })
	r.Close() // "crash"

	st2, _ := state.Open(dir)
	j, ok := st2.Job(id)
	if !ok || j.State != "succeeded" {
		t.Fatalf("job history lost across restart: %+v", j)
	}
}

func okFunc(ctx context.Context, p func(float64, string)) (string, error) { return "ok", nil }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}

func TestSubmitRefusesDraining(t *testing.T) {
	r, _ := newRunner(t)
	r.SetDraining([]string{"spike5"}, true)
	_, err := r.Submit("backup", "spike5", "local", "", okFunc)
	if err == nil {
		t.Fatal("submit during drain")
	}
}

func TestWaitIdleExceptSelf(t *testing.T) {
	r, _ := newRunner(t)
	release := make(chan struct{})
	id, err := r.Submit("fb_db_register", "fb5", "local", "", func(ctx context.Context, p func(float64, string)) (string, error) {
		<-release
		return "ok", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { j, _ := r.Status(id); return j.State == "running" })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.WaitIdle(ctx, "fb5", id); err != nil {
		t.Fatalf("self job must be excluded: %v", err)
	}
	close(release)
	waitFor(t, func() bool { j, _ := r.Status(id); return j.State == "succeeded" })
}
