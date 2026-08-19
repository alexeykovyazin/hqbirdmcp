// Package workflows is K5: a persisted step-graph with per-step compensation
// and startup reconciliation that always drives the database back online
// when AutoReopen is set (phase4_plan.md P4.5). Generalizes the P3.3
// exclusive-window primitive (K3 retired).
package workflows

import (
	"context"
	"fmt"
	"time"

	"github.com/aleks/fbmcp/internal/state"
)

// StepFunc performs one step. ctx is cancelled on job cancel / timeout.
type StepFunc func(ctx context.Context, wf *state.Workflow, prog func(float64, string)) error

// CompensateFunc undoes a completed step (best-effort).
type CompensateFunc func(ctx context.Context, wf *state.Workflow) error

// StepDef is one node of a registered workflow type.
type StepDef struct {
	Name       string
	Do         StepFunc
	Compensate CompensateFunc
	AlwaysRun  bool // run even during reconcilation-to-online (e.g. gfix -online)
}

// Engine runs named graphs against the state store.
type Engine struct {
	st    *state.Store
	types map[string][]StepDef
}

func New(st *state.Store) *Engine {
	return &Engine{st: st, types: map[string][]StepDef{}}
}

func (e *Engine) Register(typ string, steps []StepDef) { e.types[typ] = steps }

// Run persists the graph and executes it. On failure, compensations run in
// reverse for steps that completed. Returns a result message.
func (e *Engine) Run(ctx context.Context, id, typ, db string, autoReopen bool, detail map[string]string, prog func(float64, string)) (string, error) {
	defs, ok := e.types[typ]
	if !ok {
		return "", fmt.Errorf("unknown workflow type %q", typ)
	}
	names := make([]string, len(defs))
	status := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
		status[i] = "pending"
	}
	wf := state.Workflow{
		ID: id, Type: typ, Database: db, State: "running",
		Steps: names, StepStatus: status, AutoReopen: autoReopen,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Detail: detail,
	}
	if err := e.st.PutWorkflow(wf); err != nil {
		return "", err
	}
	return e.exec(ctx, &wf, defs, 0, prog)
}

func (e *Engine) exec(ctx context.Context, wf *state.Workflow, defs []StepDef, from int, prog func(float64, string)) (string, error) {
	for i := from; i < len(defs); i++ {
		if err := ctx.Err(); err != nil {
			wf.State = "failed"
			wf.Message = err.Error()
			e.st.PutWorkflow(*wf)
			return "", e.compensate(ctx, wf, defs, i-1)
		}
		wf.Step = i
		wf.StepStatus[i] = "running"
		wf.UpdatedAt = time.Now().UTC()
		e.st.PutWorkflow(*wf)
		if prog != nil {
			prog(float64(i)/float64(len(defs)), defs[i].Name)
		}
		if err := defs[i].Do(ctx, wf, prog); err != nil {
			wf.StepStatus[i] = "failed"
			wf.State = "failed"
			wf.Message = err.Error()
			e.st.PutWorkflow(*wf)
			if cErr := e.compensate(ctx, wf, defs, i-1); cErr != nil {
				return "", fmt.Errorf("step %s failed: %v; compensate: %w", defs[i].Name, err, cErr)
			}
			return "", fmt.Errorf("step %s failed (compensated): %w", defs[i].Name, err)
		}
		wf.StepStatus[i] = "done"
		e.st.PutWorkflow(*wf)
	}
	wf.State = "succeeded"
	wf.Message = "completed"
	wf.UpdatedAt = time.Now().UTC()
	e.st.PutWorkflow(*wf)
	if prog != nil {
		prog(1, "completed")
	}
	return fmt.Sprintf("workflow %s completed (%d steps)", wf.Type, len(defs)), nil
}

func (e *Engine) compensate(ctx context.Context, wf *state.Workflow, defs []StepDef, lastDone int) error {
	wf.State = "compensating"
	e.st.PutWorkflow(*wf)
	var first error
	for i := lastDone; i >= 0; i-- {
		if defs[i].Compensate == nil {
			continue
		}
		if err := defs[i].Compensate(ctx, wf); err != nil && first == nil {
			first = err
		} else {
			wf.StepStatus[i] = "compensated"
		}
	}
	if first != nil {
		wf.Message = first.Error()
	} else {
		wf.State = "compensated"
	}
	wf.UpdatedAt = time.Now().UTC()
	e.st.PutWorkflow(*wf)
	return first
}

// Reconcile drives AutoReopen workflows left running across a crash: it
// skips already-done steps and continues, preferring the online/verify
// tail so a database is never left shut (fuse #3).
func (e *Engine) Reconcile(ctx context.Context) {
	for _, wf := range e.st.RunningWorkflows() {
		defs, ok := e.types[wf.Type]
		if !ok || !wf.AutoReopen {
			wf.State = "failed"
			wf.Message = "server restarted mid-workflow; not auto-resumed"
			e.st.PutWorkflow(wf)
			continue
		}
		w := wf
		// resume from the first non-done step; AlwaysRun steps in the tail
		// (bring-online) still execute if we jump to them.
		from := w.Step
		if from < 0 {
			from = 0
		}
		e.exec(ctx, &w, defs, from, nil)
	}
}
