package workflows

import (
	"context"
	"testing"

	"github.com/aleks/fbmcp/internal/state"
)

func TestRunSucceeds(t *testing.T) {
	st, _ := state.Open(t.TempDir())
	e := New(st)
	var order []string
	e.Register("t", []StepDef{
		{Name: "a", Do: func(ctx context.Context, wf *state.Workflow, prog func(float64, string)) error {
			order = append(order, "a")
			return nil
		}},
		{Name: "b", Do: func(ctx context.Context, wf *state.Workflow, prog func(float64, string)) error {
			order = append(order, "b")
			return nil
		}},
	})
	if _, err := e.Run(context.Background(), "w1", "t", "db", true, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := order[0] + order[1]; got != "ab" {
		t.Fatalf("order %v", order)
	}
	w, ok := st.Workflow("w1")
	if !ok || w.State != "succeeded" {
		t.Fatalf("state %+v", w)
	}
}

func TestCompensateOnFailure(t *testing.T) {
	st, _ := state.Open(t.TempDir())
	e := New(st)
	var comps []string
	e.Register("t", []StepDef{
		{Name: "a", Do: func(ctx context.Context, wf *state.Workflow, prog func(float64, string)) error { return nil },
			Compensate: func(ctx context.Context, wf *state.Workflow) error { comps = append(comps, "a"); return nil }},
		{Name: "b", Do: func(ctx context.Context, wf *state.Workflow, prog func(float64, string)) error {
			return context.Canceled
		}},
	})
	if _, err := e.Run(context.Background(), "w2", "t", "db", true, nil, nil); err == nil {
		t.Fatal("expected error")
	}
	if len(comps) != 1 || comps[0] != "a" {
		t.Fatalf("compensations %v", comps)
	}
}
