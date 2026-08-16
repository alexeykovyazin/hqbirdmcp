package policy

import (
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/state"
)

func demoTools() []ToolMeta {
	return []ToolMeta{
		{Name: "fb_ping", Tier: 0},
		{Name: "fb_db_health", Tier: 0},
		{Name: "fb_demo_write", Tier: 1, RetrySafe: true},
		{Name: "fb_demo_drop", Tier: 2, Preconditions: []Precondition{
			{Name: "backup_freshness", Op: "lt", Value: 24.0, Why: "last verified backup must be younger than 24h"},
		}},
		{Name: "fb_drop_database", Tier: 3},
	}
}

func newEngine(t *testing.T, facts state.StubFacts, windows ...state.Window) (*Engine, *state.Store) {
	t.Helper()
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range windows {
		st.AddWindow(w)
	}
	return New(demoTools(), facts, st), st
}

var localAll = Identity{Name: "local", MaxTier: 2}

func TestTier0Allowed(t *testing.T) {
	e, _ := newEngine(t, state.StubFacts{})
	if d := e.Evaluate(localAll, "spike5", "fb_ping"); d.Outcome != "allow" {
		t.Fatalf("tier0: %+v", d)
	}
}

func TestUnknownToolDenied(t *testing.T) {
	e, _ := newEngine(t, state.StubFacts{})
	if d := e.Evaluate(localAll, "spike5", "fb_nope"); d.Outcome != "deny" {
		t.Fatalf("unknown tool: %+v", d)
	}
}

func TestTier1Pending(t *testing.T) {
	e, _ := newEngine(t, state.StubFacts{})
	d := e.Evaluate(localAll, "spike5", "fb_demo_write")
	if d.Outcome != "pending" {
		t.Fatalf("tier1 must pend: %+v", d)
	}
}

func TestTier3AlwaysDenied(t *testing.T) {
	e, _ := newEngine(t, state.StubFacts{})
	if d := e.Evaluate(Identity{Name: "god", MaxTier: 3}, "spike5", "fb_drop_database"); d.Outcome != "deny" {
		t.Fatalf("tier3 not disabled: %+v", d)
	}
}

func TestIdentityCeiling(t *testing.T) {
	e, _ := newEngine(t, state.StubFacts{})
	if d := e.Evaluate(Identity{Name: "ro", MaxTier: 0}, "spike5", "fb_demo_write"); d.Outcome != "deny" {
		t.Fatalf("ceiling not enforced: %+v", d)
	}
}

func TestDBScope(t *testing.T) {
	e, _ := newEngine(t, state.StubFacts{})
	id := Identity{Name: "scoped", MaxTier: 0, DBs: []string{"spike3"}}
	if d := e.Evaluate(id, "spike5", "fb_ping"); d.Outcome != "deny" {
		t.Fatalf("db scope not enforced: %+v", d)
	}
	if d := e.Evaluate(id, "spike3", "fb_ping"); d.Outcome != "allow" {
		t.Fatalf("allowed db denied: %+v", d)
	}
}

func TestTier2NeedsWindow(t *testing.T) {
	e, _ := newEngine(t, state.StubFacts{})
	if d := e.Evaluate(localAll, "spike5", "fb_demo_drop"); d.Outcome != "deny" {
		t.Fatalf("tier2 without window must deny: %+v", d)
	}
}

func TestTier2PreconditionFailsClosedAndPends(t *testing.T) {
	// fresh window but stale backup (48h)
	now := time.Now()
	e, _ := newEngine(t, state.StubFacts{"backup_freshness": 48.0},
		state.Window{Database: "spike5", From: now.Add(-time.Hour), To: now.Add(time.Hour)})
	d := e.Evaluate(localAll, "spike5", "fb_demo_drop")
	if d.Outcome != "pending" || len(d.FailedPreconditions) == 0 {
		t.Fatalf("stale backup must pend with reason: %+v", d)
	}
	// missing provider → deny (fail-closed)
	e2, _ := newEngine(t, state.StubFacts{},
		state.Window{Database: "spike5", From: now.Add(-time.Hour), To: now.Add(time.Hour)})
	if d := e2.Evaluate(localAll, "spike5", "fb_demo_drop"); d.Outcome != "deny" {
		t.Fatalf("missing facts provider must deny (fail-closed): %+v", d)
	}
}

func TestTier2HappyPathPending(t *testing.T) {
	now := time.Now()
	e, _ := newEngine(t, state.StubFacts{"backup_freshness": 1.0},
		state.Window{Database: "spike5", From: now.Add(-time.Hour), To: now.Add(time.Hour)})
	d := e.Evaluate(localAll, "spike5", "fb_demo_drop")
	if d.Outcome != "pending" {
		t.Fatalf("healthy tier2 must pend for confirmation: %+v", d)
	}
}
