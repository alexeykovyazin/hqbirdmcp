// Package state implements the kernel state store: pending actions,
// maintenance windows, backup catalog, job records —
// single persistent component under the state dir, written atomically
// (temp+rename, ADR-009/D6), guarded by the single-instance lock (D8).
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aleks/fbmcp/internal/killpoint"
)

// FactsProvider supplies named facts to the policy engine's precondition
// checker. Real providers register at startup (P2.1 engine_version,
// P3.1 backup_freshness…). Fail-closed: a missing
// provider is an evaluation error, never a silent pass.
type FactsProvider interface {
	Fact(ctx FactContext, name string, args map[string]string) (any, error)
}

// FactContext carries the database/instance a fact is evaluated for.
type FactContext struct {
	Database string
	Instance string
}

// PendingAction is a gated request awaiting confirmation.
type PendingAction struct {
	ID               string            `json:"id"`
	Created          time.Time         `json:"created"`
	Expires          time.Time         `json:"expires"`
	Identity         string            `json:"identity"`
	Database         string            `json:"database"`
	Tool             string            `json:"tool"`
	Tier             int               `json:"tier"`
	ImpactText       string            `json:"impact_text"`
	ArgHash          string            `json:"arg_hash"`                // re-validation: changed args ⇒ re-request
	Preconditions    map[string]string `json:"preconditions,omitempty"` // name → human-readable requirement
	ConfirmedChannel string            `json:"-"`                       // set by gate.Confirm; not persisted
}

// Job record persisted across restarts.
type Job struct {
	ID        string            `json:"id"`
	Type      string            `json:"type"`
	Database  string            `json:"database"`
	Identity  string            `json:"identity"`
	State     string            `json:"state"` // queued|running|succeeded|failed|cancelled|interrupted
	Progress  float64           `json:"progress"`
	Message   string            `json:"message"`
	RequestID string            `json:"request_id,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	Detail    map[string]string `json:"detail,omitempty"`
}

// CatalogEntry is one backup artifact (K2).
type CatalogEntry struct {
	ID        string    `json:"id"`
	Database  string    `json:"database"`
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"created_at"`
	Verified  bool      `json:"verified"`
	Kind      string    `json:"kind,omitempty"`  // gbak | nbackup
	Level     int       `json:"level,omitempty"` // nbackup level
}

// Advisory is a P2.5 suggestion that a P4.2 tool can execute by id.
type Advisory struct {
	ID        string    `json:"id"`
	Database  string    `json:"database"`
	Tool      string    `json:"tool"`
	Object    string    `json:"object"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

// TraceRec is a persisted P3.6 trace session (engine id + output path).
type TraceRec struct {
	ID        string    `json:"id"`
	Database  string    `json:"database"`
	Template  string    `json:"template"`
	EngineID  int32     `json:"engine_id"`
	Path      string    `json:"path"`
	StartedAt time.Time `json:"started_at"`
}

// Workflow is a persisted K5 step-graph (P4.5). Compensations run in reverse
// on failure; startup reconciliation drives AutoReopen workflows back online.
type Workflow struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	Database   string            `json:"database"`
	State      string            `json:"state"` // running|succeeded|failed|compensated
	Step       int               `json:"step"`
	Steps      []string          `json:"steps"`
	StepStatus []string          `json:"step_status"`
	AutoReopen bool              `json:"auto_reopen"`
	Message    string            `json:"message"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
	Detail     map[string]string `json:"detail,omitempty"`
}

// Schedule is a durable pre-authorization grant (ADR-023). It is not a
// PendingAction: those expire in 15 minutes.
type Schedule struct {
	ID              string    `json:"id"`
	Database        string    `json:"database"`
	Target          string    `json:"target"` // tool name or workflow type
	Kind            string    `json:"kind"`   // tool | workflow
	ArgsJSON        string    `json:"args_json"`
	ArgHash         string    `json:"arg_hash"`
	MaxTier         int       `json:"max_tier"`
	Confirmer       string    `json:"confirmer"`
	Channel         string    `json:"channel"`
	CreatingRequest string    `json:"creating_request"`
	Cron            string    `json:"cron"`
	Timezone        string    `json:"timezone"`
	WindowRequired  bool      `json:"window_required"`
	MissedRun       string    `json:"missed_run"` // skip | catchup-once
	Overlap         string    `json:"overlap"`    // skip
	Enabled         bool      `json:"enabled"`
	LastFiredAt     time.Time `json:"last_fired_at,omitempty"`
	LastSkipReason  string    `json:"last_skip_reason,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// ScheduleRun is one fire/skip evidence row (M5 unattended-week log).
type ScheduleRun struct {
	ID         string    `json:"id"`
	ScheduleID string    `json:"schedule_id"`
	Database   string    `json:"database"`
	Target     string    `json:"target"`
	Result     string    `json:"result"` // fired | skipped
	Reason     string    `json:"reason,omitempty"`
	JobID      string    `json:"job_id,omitempty"`
	At         time.Time `json:"at"`
	Confirmer  string    `json:"confirmer,omitempty"`
	Channel    string    `json:"channel,omitempty"`
	RequestID  string    `json:"creating_request,omitempty"`
}

type snapshot struct {
	Pending    []PendingAction `json:"pending"`
	Jobs       []Job           `json:"jobs"`
	Catalog    []CatalogEntry  `json:"catalog"`
	Windows    []Window        `json:"windows"`
	Workflows  []Workflow      `json:"workflows,omitempty"`
	Advisories []Advisory      `json:"advisories,omitempty"`
	Traces     []TraceRec      `json:"traces,omitempty"`
	Schedules  []Schedule      `json:"schedules,omitempty"`
	SchedRuns  []ScheduleRun   `json:"schedule_runs,omitempty"`
}

// Window is a maintenance window (db "" = all databases).
type Window struct {
	Database string    `json:"database"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
}

// Store is the single-writer kernel state.
type Store struct {
	mu   sync.Mutex
	dir  string
	snap snapshot
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir}
	b, err := os.ReadFile(s.path())
	if err == nil {
		if err := json.Unmarshal(b, &s.snap); err != nil {
			// P6.2 T4: corruption is fail-closed. A quarantine copy preserves
			// the evidence (tamper or crash artifact); state.json itself is
			// left in place so every subsequent start also refuses until an
			// operator removes or repairs it — never a silent empty restart.
			q := fmt.Sprintf("%s.corrupt-%d", s.path(), time.Now().Unix())
			_ = os.WriteFile(q, b, 0o640)
			return nil, fmt.Errorf("state: corrupt store (evidence copy: %s): %w", filepath.Base(q), err)
		}
	}
	return s, nil
}

func (s *Store) path() string { return filepath.Join(s.dir, "state.json") }

// persist is crash-durable and atomic: the temp file is fsynced before the
// rename and the directory afterwards (E.3.1), so a kill at any point leaves
// either the previous or the new state.json — never a torn file.
func (s *Store) persist() error {
	b, err := json.MarshalIndent(s.snap, "", " ")
	if err != nil {
		return err
	}
	tmp := s.path() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	killpoint.Hit("state.mid-persist") // chaos harness: kill between durable tmp and rename
	if err := os.Rename(tmp, s.path()); err != nil {
		return err
	}
	d, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer d.Close()
	// Best-effort: directory fsync is unsupported on some platforms (Windows
	// returns access denied on directory handles); the pre-rename file fsync
	// above already makes the payload durable.
	_ = d.Sync()
	return nil
}

// AddPending stores a pending action.
func (s *Store) AddPending(p PendingAction) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Pending = append(s.snap.Pending, p)
	return s.persist()
}

// TakePending atomically removes and returns a pending action (single-use).
func (s *Store) TakePending(id string) (PendingAction, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.snap.Pending {
		if p.ID == id {
			s.snap.Pending = append(s.snap.Pending[:i], s.snap.Pending[i+1:]...)
			return p, true, s.persist()
		}
	}
	return PendingAction{}, false, nil
}

func (s *Store) Pending() []PendingAction {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PendingAction, len(s.snap.Pending))
	copy(out, s.snap.Pending)
	return out
}

// InWindow reports whether db is inside a maintenance window now.
func (s *Store) InWindow(db string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.snap.Windows {
		if (w.Database == db || w.Database == "") && !now.Before(w.From) && now.Before(w.To) {
			return true
		}
	}
	return false
}

func (s *Store) AddWindow(w Window) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Windows = append(s.snap.Windows, w)
	return s.persist()
}

func (s *Store) PutJob(j Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.snap.Jobs {
		if e.ID == j.ID {
			s.snap.Jobs[i] = j
			return s.persist()
		}
	}
	s.snap.Jobs = append(s.snap.Jobs, j)
	return s.persist()
}

func (s *Store) Job(id string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.snap.Jobs {
		if j.ID == id {
			return j, true
		}
	}
	return Job{}, false
}

func (s *Store) Jobs() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Job, len(s.snap.Jobs))
	copy(out, s.snap.Jobs)
	return out
}

func (s *Store) AddCatalogEntry(e CatalogEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Catalog = append(s.snap.Catalog, e)
	return s.persist()
}

// LatestVerifiedBackup returns creation time of newest verified backup (P3.1 wires real entries).
func (s *Store) LatestVerifiedBackup(db string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best time.Time
	found := false
	for _, e := range s.snap.Catalog {
		if e.Database == db && e.Verified && (!found || e.CreatedAt.After(best)) {
			best, found = e.CreatedAt, true
		}
	}
	return best, found
}

func (s *Store) PutWorkflow(w Workflow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.snap.Workflows {
		if e.ID == w.ID {
			s.snap.Workflows[i] = w
			return s.persist()
		}
	}
	s.snap.Workflows = append(s.snap.Workflows, w)
	return s.persist()
}

func (s *Store) Workflow(id string) (Workflow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.snap.Workflows {
		if w.ID == id {
			return w, true
		}
	}
	return Workflow{}, false
}

func (s *Store) Workflows() []Workflow {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Workflow, len(s.snap.Workflows))
	copy(out, s.snap.Workflows)
	return out
}

func (s *Store) RunningWorkflows() []Workflow {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Workflow
	for _, w := range s.snap.Workflows {
		if w.State == "running" || w.State == "compensating" {
			out = append(out, w)
		}
	}
	return out
}

func (s *Store) PutAdvisory(a Advisory) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.Advisories = append(s.snap.Advisories, a)
	if len(s.snap.Advisories) > 200 {
		s.snap.Advisories = s.snap.Advisories[len(s.snap.Advisories)-200:]
	}
	return s.persist()
}

func (s *Store) Advisory(id string) (Advisory, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.snap.Advisories) - 1; i >= 0; i-- {
		if s.snap.Advisories[i].ID == id {
			return s.snap.Advisories[i], true
		}
	}
	return Advisory{}, false
}

func (s *Store) Catalog() []CatalogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CatalogEntry, len(s.snap.Catalog))
	copy(out, s.snap.Catalog)
	return out
}

func (s *Store) LatestNBackup(db string, level int) (CatalogEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best CatalogEntry
	found := false
	for _, e := range s.snap.Catalog {
		if e.Database == db && e.Kind == "nbackup" && e.Level == level {
			if !found || e.CreatedAt.After(best.CreatedAt) {
				best, found = e, true
			}
		}
	}
	return best, found
}

func (s *Store) PutTrace(t TraceRec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.snap.Traces {
		if e.ID == t.ID {
			s.snap.Traces[i] = t
			return s.persist()
		}
	}
	s.snap.Traces = append(s.snap.Traces, t)
	return s.persist()
}

func (s *Store) RemoveTrace(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.snap.Traces {
		if e.ID == id {
			s.snap.Traces = append(s.snap.Traces[:i], s.snap.Traces[i+1:]...)
			return s.persist()
		}
	}
	return nil
}

func (s *Store) Traces() []TraceRec {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TraceRec, len(s.snap.Traces))
	copy(out, s.snap.Traces)
	return out
}

func (s *Store) PutSchedule(sc Schedule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.snap.Schedules {
		if e.ID == sc.ID {
			s.snap.Schedules[i] = sc
			return s.persist()
		}
	}
	s.snap.Schedules = append(s.snap.Schedules, sc)
	return s.persist()
}

func (s *Store) RemoveSchedule(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.snap.Schedules {
		if e.ID == id {
			s.snap.Schedules = append(s.snap.Schedules[:i], s.snap.Schedules[i+1:]...)
			return s.persist()
		}
	}
	return fmt.Errorf("unknown schedule %q", id)
}

func (s *Store) Schedule(id string) (Schedule, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.snap.Schedules {
		if e.ID == id {
			return e, true
		}
	}
	return Schedule{}, false
}

func (s *Store) Schedules() []Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Schedule, len(s.snap.Schedules))
	copy(out, s.snap.Schedules)
	return out
}

func (s *Store) AddScheduleRun(r ScheduleRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.SchedRuns = append(s.snap.SchedRuns, r)
	if len(s.snap.SchedRuns) > 500 {
		s.snap.SchedRuns = s.snap.SchedRuns[len(s.snap.SchedRuns)-500:]
	}
	return s.persist()
}

func (s *Store) ScheduleRuns() []ScheduleRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ScheduleRun, len(s.snap.SchedRuns))
	copy(out, s.snap.SchedRuns)
	return out
}

// DBHasLiveJob reports queued/running jobs for overlap policy (ADR-023).
func (s *Store) DBHasLiveJob(db string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.snap.Jobs {
		if j.Database == db && (j.State == "queued" || j.State == "running") {
			return true
		}
	}
	return false
}

func (s *Store) RemoveCatalogEntry(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.snap.Catalog {
		if e.ID == id {
			s.snap.Catalog = append(s.snap.Catalog[:i], s.snap.Catalog[i+1:]...)
			return s.persist()
		}
	}
	return fmt.Errorf("unknown catalog entry %q", id)
}

// DropPendingFor removes pending actions for the given database ids.
// Register (instance-scoped) pending is dropped only when the instance id is
// listed in dropInstanceIDs.
func (s *Store) DropPendingFor(dbIDs, dropInstanceIDs []string) []string {
	dbWant := map[string]bool{}
	for _, id := range dbIDs {
		dbWant[id] = true
	}
	instWant := map[string]bool{}
	for _, id := range dropInstanceIDs {
		instWant[id] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var dropped []string
	kept := s.snap.Pending[:0]
	for _, p := range s.snap.Pending {
		if p.Tool == "fb_db_register" {
			if instWant[p.Database] {
				dropped = append(dropped, p.ID)
				continue
			}
			kept = append(kept, p)
			continue
		}
		if dbWant[p.Database] {
			dropped = append(dropped, p.ID)
			continue
		}
		kept = append(kept, p)
	}
	s.snap.Pending = kept
	_ = s.persist()
	return dropped
}

// RemapDatabaseID rewrites schedule/window/trace database fields. empty newID deletes.
func (s *Store) RemapDatabaseID(oldID, newID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if newID == "" {
		var sched []Schedule
		for _, sc := range s.snap.Schedules {
			if sc.Database != oldID {
				sched = append(sched, sc)
			}
		}
		s.snap.Schedules = sched
		var win []Window
		for _, w := range s.snap.Windows {
			if w.Database != oldID {
				win = append(win, w)
			}
		}
		s.snap.Windows = win
		var tr []TraceRec
		for _, t := range s.snap.Traces {
			if t.Database != oldID {
				tr = append(tr, t)
			}
		}
		s.snap.Traces = tr
		return s.persist()
	}
	for i := range s.snap.Schedules {
		if s.snap.Schedules[i].Database == oldID {
			s.snap.Schedules[i].Database = newID
		}
	}
	for i := range s.snap.Windows {
		if s.snap.Windows[i].Database == oldID {
			s.snap.Windows[i].Database = newID
		}
	}
	for i := range s.snap.Traces {
		if s.snap.Traces[i].Database == oldID {
			s.snap.Traces[i].Database = newID
		}
	}
	return s.persist()
}

// CompositeFacts merges several providers (first match wins, fail-closed).
type CompositeFacts []FactsProvider

func (c CompositeFacts) Fact(fc FactContext, name string, args map[string]string) (any, error) {
	var firstErr error
	for _, p := range c {
		v, err := p.Fact(fc, name, args)
		if err == nil {
			return v, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("no facts providers registered")
	}
	return nil, firstErr
}
