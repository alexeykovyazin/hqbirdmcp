// Package policy implements the P1.5 policy & tier engine: tool tier
// metadata, declarative preconditions over facts providers, maintenance
// windows, impact scopes, and the single decision API
// Evaluate(identity, db, tool) → allow / pending / deny + reason.
//
// In production the ToolMeta table is generated from
// firebird_dba_tasks_table_v3.md (single source of truth, CI-diffed); the
// hand-written entries here are the Phase-1 demo set and are marked as such.
package policy

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aleks/fbmcp/internal/state"
)

// ToolMeta is the per-tool policy metadata (Appendix B).
type ToolMeta struct {
	Name        string   `json:"name"`
	Tier        int      `json:"tier"` // 0 read … 3 critical (disabled by default)
	Scope       string   `json:"scope"` // "database" | "instance"
	MinFB       string  `json:"min_fb,omitempty"` // "3.0" style
	Preconditions []Precondition `json:"preconditions,omitempty"`
	RetrySafe   bool     `json:"retry_safe"`
}

// Precondition is a declarative check against facts providers.
type Precondition struct {
	Name string            `json:"name"` // fact name
	Op   string            `json:"op"`   // lt | le | gt | ge | eq | true | exists
	Value any              `json:"value,omitempty"`
	Args map[string]string `json:"args,omitempty"`
	Why  string            `json:"why"` // human explanation when it fails
}

// Identity is the caller (P1.3 minimal form; remote identities arrive in P5.1).
type Identity struct {
	Name     string
	MaxTier  int      // identity ceiling: requests above this are denied
	DBs      []string // empty = all registered
	Kind     string   // "local" | "api-key" | "operator"
}

// Engine is the policy decision point.
type Engine struct {
	tools  map[string]ToolMeta
	facts  state.FactsProvider
	st     *state.Store
	now    func() time.Time
}

func New(tools []ToolMeta, facts state.FactsProvider, st *state.Store) *Engine {
	e := &Engine{tools: map[string]ToolMeta{}, facts: facts, st: st, now: time.Now}
	for _, t := range tools {
		e.tools[t.Name] = t
	}
	return e
}

func (e *Engine) WithNow(f func() time.Time) *Engine { e.now = f; return e }

// Decision is the outcome of Evaluate.
type Decision struct {
	Outcome string   // "allow" | "pending" | "deny"
	Reason  string
	Meta    ToolMeta
	FailedPreconditions []string // human-readable, for the pending message
}

// Evaluate is the single decision API (main plan P1.5).
func (e *Engine) Evaluate(id Identity, db string, tool string) Decision {
	meta, ok := e.tools[tool]
	if !ok {
		return Decision{Outcome: "deny", Reason: fmt.Sprintf("unknown tool %q (advertise-only-what-exists)", tool)}
	}
	if !id.allowsDB(db) {
		return Decision{Outcome: "deny", Reason: fmt.Sprintf("identity %q has no permission for database %q", id.Name, db)}
	}
	if meta.Tier == 3 {
		return Decision{Outcome: "deny", Reason: "Tier 3 operations are disabled by default (config unlock + dual control required)"}
	}
	if meta.Tier > id.MaxTier {
		return Decision{Outcome: "deny", Reason: fmt.Sprintf("tier %d exceeds identity ceiling %d", meta.Tier, id.MaxTier)}
	}
	// Tier 2+ needs an open maintenance window.
	if meta.Tier >= 2 && !e.st.InWindow(db, e.now()) {
		return Decision{Outcome: "deny", Reason: "no open maintenance window for this database (Tier ≥ 2 requires one)"}
	}
	// Preconditions (Tier ≥ 2 mandatory checks; evaluated whenever declared).
	var failed []string
	for _, p := range meta.Preconditions {
		ok, why, err := e.check(db, p)
		if err != nil {
			return Decision{Outcome: "deny", Reason: fmt.Sprintf("precondition %q could not be evaluated (fail-closed): %v", p.Name, err)}
		}
		if !ok {
			failed = append(failed, why)
		}
	}
	if len(failed) > 0 {
		return Decision{Outcome: "pending", Reason: "preconditions failed: " + strings.Join(failed, "; "), Meta: meta, FailedPreconditions: failed}
	}
	if meta.Tier >= 1 {
		return Decision{Outcome: "pending", Reason: "human confirmation required", Meta: meta}
	}
	return Decision{Outcome: "allow", Reason: "Tier 0 read", Meta: meta}
}

func (id Identity) allowsDB(db string) bool {
	if len(id.DBs) == 0 {
		return true
	}
	for _, d := range id.DBs {
		if d == db {
			return true
		}
	}
	return false
}

func (e *Engine) check(db string, p Precondition) (bool, string, error) {
	v, err := e.facts.Fact(state.FactContext{Database: db}, p.Name, p.Args)
	if err != nil {
		return false, "", err
	}
	why := p.Why
	if why == "" {
		why = fmt.Sprintf("%s %v %v", p.Name, p.Op, p.Value)
	}
	switch p.Op {
	case "true":
		b, ok := v.(bool)
		return ok && b, why, nil
	case "exists":
		return v != nil, why, nil
	case "lt", "le", "gt", "ge":
		got, ok1 := toFloat(v)
		want, ok2 := toFloat(p.Value)
		if !ok1 || !ok2 {
			return false, why, fmt.Errorf("non-numeric fact for %s", p.Name)
		}
		switch p.Op {
		case "lt":
			return got < want, why, nil
		case "le":
			return got <= want, why, nil
		case "gt":
			return got > want, why, nil
		default:
			return got >= want, why, nil
		}
	case "eq":
		return fmt.Sprint(v) == fmt.Sprint(p.Value), why, nil
	default:
		return false, "", fmt.Errorf("unknown precondition op %q", p.Op)
	}
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case time.Duration:
		return n.Hours(), true
	}
	return 0, false
}

// Tools lists known tool metadata sorted by name (for the startup self-test).
func (e *Engine) Tools() []ToolMeta {
	out := make([]ToolMeta, 0, len(e.tools))
	for _, t := range e.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// V3Op is one row of the generated v3 operations table.
type V3Op struct {
	Num      int
	Category string
	Action   string
	Risk     string
	OpType   string
	ExclDB   bool
	ExclObj  bool
	Restart  bool
}

// TierForRisk maps the v3 Risk column to the tool tier (main plan §5.2).
// Read rows are Tier 0; Write rows map by risk; Critical = Tier 3 (disabled).
func TierForRisk(o V3Op) int {
	if o.OpType == "Read" {
		return 0
	}
	switch o.Risk {
	case "Critical":
		return 3
	case "High":
		return 2
	default: // Medium / Low writes
		return 1
	}
}

//go:generate go run github.com/aleks/fbmcp/internal/gen/fromv3
