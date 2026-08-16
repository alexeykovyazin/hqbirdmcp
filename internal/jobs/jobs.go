// Package jobs implements the P1.7 job manager: async jobs with persistence
// across restart, per-database single-flight (one serial goroutine per DB),
// cooperative cancellation, and startup reconciliation (interrupted jobs).
// Phase 1 runs fake job functions; Phase 3 registers real executors.
package jobs

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aleks/fbmcp/internal/state"
)

// Func is a job body: receives ctx (cancelled on Cancel), reports progress,
// returns a result message. Crash-safety is the runner's contract: the job
// func must be idempotent or tolerate interrupted re-runs (v1 marks them
// interrupted instead of auto-resuming destructive work).
type Func func(ctx context.Context, progress func(frac float64, msg string)) (string, error)

// Runner executes jobs with per-DB serialization.
type Runner struct {
	st *state.Store

	mu     sync.Mutex
	dbChan map[string]chan task
	cancel map[string]context.CancelFunc
	closed bool
	wg     sync.WaitGroup
}

type task struct {
	job state.Job
	fn  Func
}

func NewRunner(st *state.Store) *Runner {
	r := &Runner{st: st, dbChan: map[string]chan task{}, cancel: map[string]context.CancelFunc{}}
	r.reconcile()
	return r
}

// reconcile marks running jobs from a previous process as interrupted.
func (r *Runner) reconcile() {
	for _, j := range r.st.Jobs() {
		if j.State == "running" || j.State == "queued" {
			j.State = "interrupted"
			j.Message = "server restarted mid-job; re-submit if safe (job not auto-resumed)"
			j.UpdatedAt = time.Now().UTC()
			r.st.PutJob(j)
		}
	}
}

// Submit enqueues a job. Jobs on the same database serialize (single-flight);
// different databases run concurrently.
func (r *Runner) Submit(jobType, db, identity, requestID string, fn Func) (string, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return "", fmt.Errorf("runner is closed")
	}
	ch, ok := r.dbChan[db]
	if !ok {
		ch = make(chan task, 256)
		r.dbChan[db] = ch
		r.wg.Add(1)
		go r.worker(ch)
	}
	r.mu.Unlock()

	id := fmt.Sprintf("j%d", time.Now().UnixNano())
	j := state.Job{
		ID: id, Type: jobType, Database: db, Identity: identity, RequestID: requestID,
		State: "queued", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := r.st.PutJob(j); err != nil {
		return "", err
	}
	select {
	case ch <- task{job: j, fn: fn}:
		return id, nil
	default:
		j.State = "failed"
		j.Message = "job queue full for this database"
		r.st.PutJob(j)
		return "", fmt.Errorf("job queue full for database %q", db)
	}
}

// worker serially executes tasks for one database.
func (r *Runner) worker(ch chan task) {
	defer r.wg.Done()
	for t := range ch {
		ctx, cancel := context.WithCancel(context.Background())
		r.mu.Lock()
		r.cancel[t.job.ID] = cancel
		r.mu.Unlock()

		j := t.job
		j.State = "running"
		j.UpdatedAt = time.Now().UTC()
		r.st.PutJob(j)

		progress := func(frac float64, msg string) {
			j.Progress = frac
			j.Message = msg
			j.UpdatedAt = time.Now().UTC()
			r.st.PutJob(j)
		}

		res, err := t.fn(ctx, progress)
		wasCancelled := ctx.Err() != nil // check before our own cancel() call
		cancel()
		r.mu.Lock()
		delete(r.cancel, t.job.ID)
		r.mu.Unlock()

		j.UpdatedAt = time.Now().UTC()
		if wasCancelled {
			j.State = "cancelled"
			j.Message = "cancelled by operator"
		} else if err != nil {
			j.State = "failed"
			j.Message = err.Error()
		} else {
			j.State = "succeeded"
			j.Message = res
			j.Progress = 1
		}
		r.st.PutJob(j)
	}
}

// Cancel cooperatively cancels a running/queued job.
func (r *Runner) Cancel(id string) error {
	j, ok := r.st.Job(id)
	if !ok {
		return fmt.Errorf("unknown job %q", id)
	}
	switch j.State {
	case "succeeded", "failed", "cancelled", "interrupted":
		return fmt.Errorf("job already %s", j.State)
	}
	r.mu.Lock()
	if c, ok := r.cancel[id]; ok {
		c()
	}
	r.mu.Unlock()
	return nil
}

// Status reports a job.
func (r *Runner) Status(id string) (state.Job, bool) { return r.st.Job(id) }

// Close stops accepting jobs and waits (bounded) for workers.
func (r *Runner) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	chans := make([]chan task, 0, len(r.dbChan))
	for _, ch := range r.dbChan {
		chans = append(chans, ch)
	}
	r.mu.Unlock()
	for _, ch := range chans {
		close(ch)
	}
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
}
