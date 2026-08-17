// Package state implements the P1.8 kernel state store: pending actions,
// maintenance windows, backup catalog (stub until P3.1), job records —
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
)

// FactsProvider supplies named facts to the policy engine's precondition
// checker. Real providers register in later phases (P2.1 engine_version,
// P3.1 backup_freshness…); Phase 1 runs stubs. Fail-closed: a missing
// provider is an evaluation error, never a silent pass.
type FactsProvider interface {
	Fact(ctx FactContext, name string, args map[string]string) (any, error)
}

// FactContext carries the database/instance a fact is evaluated for.
type FactContext struct {
	Database string
	Instance string
}

// StubFacts returns fixed values (Phase 1 tests).
type StubFacts map[string]any

func (s StubFacts) Fact(_ FactContext, name string, _ map[string]string) (any, error) {
	if v, ok := s[name]; ok {
		return v, nil
	}
	return nil, fmt.Errorf("no facts provider registered for %q (fail-closed)", name)
}

// PendingAction is a gated request awaiting confirmation.
type PendingAction struct {
	ID            string            `json:"id"`
	Created       time.Time         `json:"created"`
	Expires       time.Time         `json:"expires"`
	Identity      string            `json:"identity"`
	Database      string            `json:"database"`
	Tool          string            `json:"tool"`
	Tier          int               `json:"tier"`
	ImpactText    string            `json:"impact_text"`
	ArgHash       string            `json:"arg_hash"`                // re-validation: changed args ⇒ re-request
	Preconditions map[string]string `json:"preconditions,omitempty"` // name → human-readable requirement
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

// CatalogEntry is the backup-catalog stub (activated by P3.1/K2).
type CatalogEntry struct {
	ID        string    `json:"id"`
	Database  string    `json:"database"`
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"created_at"`
	Verified  bool      `json:"verified"`
}

type snapshot struct {
	Pending []PendingAction `json:"pending"`
	Jobs    []Job           `json:"jobs"`
	Catalog []CatalogEntry  `json:"catalog"`
	Windows []Window        `json:"windows"`
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
			return nil, fmt.Errorf("state: corrupt store: %w", err)
		}
	}
	return s, nil
}

func (s *Store) path() string { return filepath.Join(s.dir, "state.json") }

// persist is atomic (temp+rename) so readers never see a torn file.
func (s *Store) persist() error {
	b, err := json.MarshalIndent(s.snap, "", " ")
	if err != nil {
		return err
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, s.path())
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
