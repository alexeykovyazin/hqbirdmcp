package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/state"
	"github.com/aleks/fbmcp/internal/statetest"
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

func newEngine(t *testing.T, facts statetest.StubFacts, windows ...state.Window) (*Engine, *state.Store) {
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
	e, _ := newEngine(t, statetest.StubFacts{})
	if d := e.Evaluate(localAll, "spike5", "fb_ping"); d.Outcome != "allow" {
		t.Fatalf("tier0: %+v", d)
	}
}

func TestUnknownToolDenied(t *testing.T) {
	e, _ := newEngine(t, statetest.StubFacts{})
	if d := e.Evaluate(localAll, "spike5", "fb_nope"); d.Outcome != "deny" {
		t.Fatalf("unknown tool: %+v", d)
	}
}

func TestTier1Pending(t *testing.T) {
	e, _ := newEngine(t, statetest.StubFacts{})
	d := e.Evaluate(localAll, "spike5", "fb_demo_write")
	if d.Outcome != "pending" {
		t.Fatalf("tier1 must pend: %+v", d)
	}
}

func TestTier3AlwaysDenied(t *testing.T) {
	e, _ := newEngine(t, statetest.StubFacts{})
	if d := e.Evaluate(Identity{Name: "god", MaxTier: 3}, "spike5", "fb_drop_database"); d.Outcome != "deny" {
		t.Fatalf("tier3 not disabled: %+v", d)
	}
}

func TestIdentityCeiling(t *testing.T) {
	e, _ := newEngine(t, statetest.StubFacts{})
	if d := e.Evaluate(Identity{Name: "ro", MaxTier: 0}, "spike5", "fb_demo_write"); d.Outcome != "deny" {
		t.Fatalf("ceiling not enforced: %+v", d)
	}
}

func TestDBScope(t *testing.T) {
	e, _ := newEngine(t, statetest.StubFacts{})
	id := Identity{Name: "scoped", MaxTier: 0, DBs: []string{"spike3"}}
	if d := e.Evaluate(id, "spike5", "fb_ping"); d.Outcome != "deny" {
		t.Fatalf("db scope not enforced: %+v", d)
	}
	if d := e.Evaluate(id, "spike3", "fb_ping"); d.Outcome != "allow" {
		t.Fatalf("allowed db denied: %+v", d)
	}
}

func TestTier2NeedsWindow(t *testing.T) {
	e, _ := newEngine(t, statetest.StubFacts{})
	if d := e.Evaluate(localAll, "spike5", "fb_demo_drop"); d.Outcome != "deny" {
		t.Fatalf("tier2 without window must deny: %+v", d)
	}
}

func TestTier2PreconditionFailsClosedAndPends(t *testing.T) {
	// fresh window but stale backup (48h)
	now := time.Now()
	e, _ := newEngine(t, statetest.StubFacts{"backup_freshness": 48.0},
		state.Window{Database: "spike5", From: now.Add(-time.Hour), To: now.Add(time.Hour)})
	d := e.Evaluate(localAll, "spike5", "fb_demo_drop")
	if d.Outcome != "pending" || len(d.FailedPreconditions) == 0 {
		t.Fatalf("stale backup must pend with reason: %+v", d)
	}
	// missing provider → deny (fail-closed)
	e2, _ := newEngine(t, statetest.StubFacts{},
		state.Window{Database: "spike5", From: now.Add(-time.Hour), To: now.Add(time.Hour)})
	if d := e2.Evaluate(localAll, "spike5", "fb_demo_drop"); d.Outcome != "deny" {
		t.Fatalf("missing facts provider must deny (fail-closed): %+v", d)
	}
}

func TestEvaluateMetaDynamicTier(t *testing.T) {
	e, _ := newEngine(t, statetest.StubFacts{})
	meta := ToolMeta{Name: "fb_write", Tier: 2, Scope: "database"}
	d := e.EvaluateMeta(localAll, "spike5", meta)
	if d.Outcome != "deny" {
		t.Fatalf("dynamic tier 2 without window must deny: %+v", d)
	}
	meta1 := ToolMeta{Name: "fb_write", Tier: 1, Scope: "database"}
	d = e.EvaluateMeta(localAll, "spike5", meta1)
	if d.Outcome != "pending" {
		t.Fatalf("dynamic tier 1 must pend: %+v", d)
	}
	d = e.EvaluateMeta(Identity{Name: "ro", MaxTier: 0}, "spike5", meta1)
	if d.Outcome != "deny" {
		t.Fatalf("ceiling must apply to EvaluateMeta: %+v", d)
	}
}

func TestMinFBFailClosed(t *testing.T) {
	e, _ := newEngine(t, statetest.StubFacts{})
	meta := ToolMeta{Name: "fb_trace_start", Tier: 1, MinFB: "3.0"}
	d := e.EvaluateMeta(localAll, "spike5", meta)
	if d.Outcome != "deny" || !strings.Contains(d.Reason, "fail-closed") {
		t.Fatalf("missing engine_version must deny: %+v", d)
	}
	e2, _ := newEngine(t, statetest.StubFacts{"engine_version": "2.5"})
	d = e2.EvaluateMeta(localAll, "spike5", meta)
	if d.Outcome != "deny" {
		t.Fatalf("2.5 must miss MinFB 3.0: %+v", d)
	}
	e3, _ := newEngine(t, statetest.StubFacts{"engine_version": "5.0"})
	d = e3.EvaluateMeta(localAll, "spike5", meta)
	if d.Outcome != "pending" {
		t.Fatalf("5.0 should pass MinFB 3.0: %+v", d)
	}
}

func TestVersionAtLeast(t *testing.T) {
	ok, err := versionAtLeast("5.0", "2.5")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	ok, _ = versionAtLeast("2.5", "3.0")
	if ok {
		t.Fatal("2.5 >= 3.0")
	}
}

// --- P1.4 (test_plan): TierForRisk exhaustively, Tools listing, WithNow ---

func TestTierForRiskExhaustive(t *testing.T) {
	risks := []string{"Critical", "High", "Medium", "Low", "None", ""}
	for _, risk := range risks {
		for _, opType := range []string{"Read", "Write", ""} {
			got := TierForRisk(V3Op{Risk: risk, OpType: opType})
			if opType == "Read" {
				if got != 0 {
					t.Fatalf("Read/%s → tier %d, want 0", risk, got)
				}
				continue
			}
			switch risk {
			case "Critical":
				if got != 3 {
					t.Fatalf("Write/%s → tier %d, want 3", risk, got)
				}
			case "High":
				if got != 2 {
					t.Fatalf("Write/%s → tier %d, want 2", risk, got)
				}
			default:
				if got != 1 {
					t.Fatalf("Write/%s → tier %d, want 1", risk, got)
				}
			}
		}
	}
}

// The frozen system surface must be tier-consistent with the v3 risk
// mapping: every Tier-2+ tool is a High/Critical write, every Tier-0 tool
// is a read. (Dynamic-tier tools fb_write/fb_migration_apply/fb_schedule_create
// are Tier-1 placeholders and exempt.)
func TestSystemToolsTierShape(t *testing.T) {
	for _, m := range SystemTools() {
		switch m.Tier {
		case 3:
			if m.Name != "fb_db_drop" {
				t.Fatalf("%s: unexpected Tier-3 tool", m.Name)
			}
		case 2:
			if m.Preconditions == nil && m.Scope != "instance" {
				t.Fatalf("%s: Tier-2 database tool without preconditions", m.Name)
			}
		}
		if m.Tier >= 2 && m.Name == "fb_write" {
			t.Fatal("fb_write must stay a Tier-1 dynamic placeholder")
		}
	}
}

func TestEngineToolsSortedAndComplete(t *testing.T) {
	st, _ := state.Open(t.TempDir())
	e := New(SystemTools(), statetest.StubFacts{}, st)
	tools := e.Tools()
	if len(tools) != len(SystemTools()) {
		t.Fatalf("Tools()=%d, SystemTools()=%d", len(tools), len(SystemTools()))
	}
	for i := 1; i < len(tools); i++ {
		if tools[i-1].Name >= tools[i].Name {
			t.Fatalf("Tools() not sorted at %d: %q >= %q", i, tools[i-1].Name, tools[i].Name)
		}
	}
	seen := map[string]bool{}
	for _, tl := range tools {
		if seen[tl.Name] {
			t.Fatalf("duplicate tool %q", tl.Name)
		}
		seen[tl.Name] = true
	}
	for _, want := range SystemTools() {
		if !seen[want.Name] {
			t.Fatalf("Tools() missing %q", want.Name)
		}
	}
}

func TestWithNowDrivesWindowCheck(t *testing.T) {
	st, _ := state.Open(t.TempDir())
	windowStart := time.Date(2026, 9, 9, 2, 0, 0, 0, time.UTC)
	windowEnd := windowStart.Add(time.Hour)
	if err := st.AddWindow(state.Window{Database: "spike5", From: windowStart, To: windowEnd}); err != nil {
		t.Fatal(err)
	}
	e := New([]ToolMeta{{Name: "t2", Tier: 2, Scope: "database"}}, statetest.StubFacts{}, st)
	id := Identity{Name: "op", MaxTier: 2}
	inside := e.WithNow(func() time.Time { return windowStart.Add(10 * time.Minute) }).
		Evaluate(id, "spike5", "t2")
	if inside.Outcome != "pending" || !strings.Contains(inside.Reason, "human confirmation") {
		t.Fatalf("inside window: %+v", inside)
	}
	later := e.WithNow(func() time.Time { return windowEnd.Add(time.Minute) }).
		Evaluate(id, "spike5", "t2")
	if later.Outcome != "deny" || !strings.Contains(later.Reason, "maintenance window") {
		t.Fatalf("outside window: %+v", later)
	}
}

func TestToFloatKinds(t *testing.T) {
	cases := []struct {
		v    any
		want float64
		ok   bool
	}{
		{7, 7, true},
		{int64(9), 9, true},
		{2.5, 2.5, true},
		{90 * time.Minute, 1.5, true},
		{"7", 0, false},
		{nil, 0, false},
	}
	for _, c := range cases {
		got, ok := toFloat(c.v)
		if ok != c.ok || (ok && got != c.want) {
			t.Fatalf("toFloat(%#v) = %v,%v want %v,%v", c.v, got, ok, c.want, c.ok)
		}
	}
}
