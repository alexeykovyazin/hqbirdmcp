// Package schedule is the in-process ticker (ADR-023). It never calls the
// human gate: fire uses a callback that submits jobs / workflows.
package schedule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "time/tzdata"

	"github.com/aleks/fbmcp/internal/state"
)

// FireFunc runs a due schedule. Implementations must not call gate.Request.
type FireFunc func(ctx context.Context, s state.Schedule) (jobID string, err error)

// GateProbe is optional; tests set it to fail if the fire path touches the gate.
type GateProbe func()

// Ticker evaluates schedules.
type Ticker struct {
	st     *state.Store
	fire   FireFunc
	probe  GateProbe
	onSkip func(s state.Schedule, reason string)
	now    func() time.Time
	dbOK   func(id string) bool
	mu     sync.Mutex
	fired  int
	skips  int
}

func New(st *state.Store, fire FireFunc) *Ticker {
	return &Ticker{st: st, fire: fire, now: time.Now}
}

func (t *Ticker) WithNow(f func() time.Time) *Ticker { t.now = f; return t }
func (t *Ticker) WithGateProbe(p GateProbe) *Ticker  { t.probe = p; return t }
func (t *Ticker) OnSkip(f func(state.Schedule, string)) *Ticker {
	t.onSkip = f
	return t
}
func (t *Ticker) WithDBExists(f func(id string) bool) *Ticker {
	t.dbOK = f
	return t
}

func (t *Ticker) Fired() int { t.mu.Lock(); defer t.mu.Unlock(); return t.fired }
func (t *Ticker) Skips() int { t.mu.Lock(); defer t.mu.Unlock(); return t.skips }

// Start ticks immediately then every 15s until ctx is done.
func (t *Ticker) Start(ctx context.Context) {
	t.Tick(ctx)
	go func() {
		tk := time.NewTicker(15 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				t.Tick(ctx)
			}
		}
	}()
}

// Tick evaluates every enabled schedule once. Safe to call from a loop.
func (t *Ticker) Tick(ctx context.Context) {
	now := t.now().UTC()
	for _, s := range t.st.Schedules() {
		if !s.Enabled {
			continue
		}
		t.consider(ctx, s, now)
	}
}

func (t *Ticker) consider(ctx context.Context, s state.Schedule, now time.Time) {
	reason, due := t.due(s, now)
	if !due {
		if reason != "" {
			t.skip(s, reason, now)
		}
		return
	}
	if t.probe != nil {
		t.probe()
	}
	if t.fire == nil {
		t.skip(s, "no fire callback", now)
		return
	}
	if t.dbOK != nil && !t.dbOK(s.Database) {
		t.skip(s, "unknown database", now)
		return
	}
	jobID, err := t.fire(ctx, s)
	result := "fired"
	msg := ""
	if err != nil {
		result = "skipped"
		msg = err.Error()
		s.LastSkipReason = msg
		_ = t.st.PutSchedule(s)
		t.mu.Lock()
		t.skips++
		t.mu.Unlock()
	} else {
		s.LastFiredAt = now
		s.LastSkipReason = ""
		_ = t.st.PutSchedule(s)
		t.mu.Lock()
		t.fired++
		t.mu.Unlock()
	}
	_ = t.st.AddScheduleRun(state.ScheduleRun{
		ID: fmt.Sprintf("sr%d", now.UnixNano()), ScheduleID: s.ID, Database: s.Database,
		Target: s.Target, Result: result, Reason: msg, JobID: jobID, At: now,
		Confirmer: s.Confirmer, Channel: s.Channel, RequestID: s.CreatingRequest,
	})
}

func (t *Ticker) due(s state.Schedule, now time.Time) (skipReason string, fire bool) {
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return "invalid timezone: " + s.Timezone, false
	}
	local := now.In(loc)
	if !s.LastFiredAt.IsZero() && now.Before(s.LastFiredAt) {
		return "clock behind last fire (rewound)", false
	}
	match, err := Matches(s.Cron, local)
	if err != nil {
		return "invalid cron: " + err.Error(), false
	}

	lastMin := s.LastFiredAt.In(loc).Truncate(time.Minute)
	thisMin := local.Truncate(time.Minute)
	want := match && !lastMin.Equal(thisMin)
	if !want && s.MissedRun == "catchup-once" && !s.LastFiredAt.IsZero() {
		prev, ok := PreviousMatch(s.Cron, loc, local)
		if ok && s.LastFiredAt.Before(prev) && !match {
			want = true
		}
	}
	if !want {
		return "", false
	}
	if HashArgs(s.ArgsJSON) != s.ArgHash {
		return "arg-hash mismatch (grant drift)", false
	}
	if s.Overlap != "allow" && t.st.DBHasLiveJob(s.Database) {
		return "overlap: previous job alive", false
	}
	if s.WindowRequired && !t.st.InWindow(s.Database, now) {
		return "maintenance window closed", false
	}
	return "", true
}

func (t *Ticker) skip(s state.Schedule, reason string, now time.Time) {
	s.LastSkipReason = reason
	_ = t.st.PutSchedule(s)
	t.mu.Lock()
	t.skips++
	t.mu.Unlock()
	_ = t.st.AddScheduleRun(state.ScheduleRun{
		ID: fmt.Sprintf("ss%d", now.UnixNano()), ScheduleID: s.ID, Database: s.Database,
		Target: s.Target, Result: "skipped", Reason: reason, At: now,
		Confirmer: s.Confirmer, Channel: s.Channel, RequestID: s.CreatingRequest,
	})
	if t.onSkip != nil {
		t.onSkip(s, reason)
	}
}

// HashArgs is the grant binding (canonical JSON).
func HashArgs(argsJSON string) string {
	h := sha256.Sum256([]byte(argsJSON))
	return hex.EncodeToString(h[:8])
}

// CanonicalJSON stable-encodes args for the grant hash.
func CanonicalJSON(args map[string]any) string {
	if args == nil {
		args = map[string]any{}
	}
	b, _ := json.Marshal(args)
	return string(b)
}

// ValidateCreate rejects forbidden / unknown targets (ADR-023).
func ValidateCreate(target, cron, tz string, maxTierOf func(string) (kind string, tier int, err error)) (kind string, tier int, err error) {
	if strings.TrimSpace(tz) == "" {
		return "", 0, fmt.Errorf("timezone is required (explicit IANA name; never server-local)")
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return "", 0, fmt.Errorf("timezone: %w", err)
	}
	if _, err := parseCron(cron); err != nil {
		return "", 0, err
	}
	kind, tier, err = maxTierOf(target)
	if err != nil {
		return "", 0, err
	}
	if tier >= 3 {
		return "", 0, fmt.Errorf("Tier-3 target %q cannot be scheduled", target)
	}
	if target == "fb_restore_replace" {
		return "", 0, fmt.Errorf("%s cannot be scheduled (in-place replace stays human-gated)", target)
	}
	return kind, tier, nil
}
