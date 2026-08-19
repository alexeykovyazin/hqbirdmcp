// Package classify maps fbparse statements to policy tiers and impact text
// for the P4.1 generic write executor (ADR-019). Defense-in-depth only: the
// engine-level controls (read-only pool, human gate) remain the actual safety
// boundary; unclassifiable ⇒ deny, never "assume read".
package classify

import (
	"fmt"
	"strings"

	"github.com/aleks/fbmcp/internal/fbparse"
)

const (
	MaxStatements = 50
	MaxBytes      = 1 << 20 // 1 MiB
)

// Result is the classification of one statement.
type Result struct {
	Statement fbparse.Statement
	Tier      int    // policy tier (3 = deny-by-default critical)
	TierKnown bool   // false ⇒ unclassifiable ⇒ the whole request is denied
	V3Op      int    // v3 operations-table row (0 = none)
	Reason    string // why the tier, for the preview
}

// Classify maps a statement to a tier using the v3-style risk model
// (audit lesson #1: conservative by default). Low parser confidence never
// lowers a tier; it escalates by one instead.
func Classify(s fbparse.Statement) Result {
	return escalate(mapStatement(s))
}

// escalate bumps low-confidence classifications one tier (max 3).
func escalate(r Result) Result {
	if r.Statement.Confidence == fbparse.ConfidenceLow && r.TierKnown && r.Tier < 3 {
		r.Tier++
		r.Reason += " (escalated: low parse confidence)"
	}
	return r
}

// Script classifies a multi-statement script; any unknown ⇒ whole-script deny.
func Script(sqlText string) ([]Result, int, string, bool) {
	if len(sqlText) > MaxBytes {
		return nil, 0, "script exceeds 1 MiB size cap", false
	}
	stmts := fbparse.Parse(sqlText)
	if len(stmts) > MaxStatements {
		return nil, 0, fmt.Sprintf("script has %d statements (cap %d)", len(stmts), MaxStatements), false
	}
	var out []Result
	maxTier := 0
	seenTier := map[int]bool{}
	for _, s := range stmts {
		r := Classify(s)
		if !r.TierKnown {
			return nil, 0, fmt.Sprintf("statement %d (%s…) is unclassifiable — whole request denied", len(out)+1, preview(s.Raw)), false
		}
		if r.Tier > maxTier {
			maxTier = r.Tier
		}
		seenTier[r.Tier] = true
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, 0, "empty script", false
	}
	if len(seenTier) > 1 {
		return nil, 0, "statement list mixes tiers — split the script and resubmit each tier separately", false
	}
	return out, maxTier, "", true
}

// HasDDL reports whether any classified statement mutates metadata.
func HasDDL(results []Result) bool {
	for _, r := range results {
		if isDDLVerb(r.Statement.Verb) {
			return true
		}
	}
	return false
}

func isDDLVerb(v fbparse.Verb) bool {
	switch v {
	case fbparse.VerbCreate, fbparse.VerbAlter, fbparse.VerbDrop, fbparse.VerbRecreate,
		fbparse.VerbCreateOrAlter, fbparse.VerbComment, fbparse.VerbGrant, fbparse.VerbRevoke,
		fbparse.VerbSet, fbparse.VerbDeclare:
		return true
	}
	return false
}

// Preview renders the human impact statement (LLM-facing honesty §5.4).
// It never uses the word "safe" (ADR-021).
func Preview(results []Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d statement(s):\n", len(results))
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s %s %s — tier %d (%s)\n", i+1, r.Statement.Verb, r.Statement.ObjectType, r.Statement.Object.Name, r.Tier, r.Reason)
		if r.Statement.Where != "" {
			fmt.Fprintf(&b, "   WHERE: %s\n", r.Statement.Where)
		}
		if r.Statement.Flags.IndexConcurrently {
			fmt.Fprintf(&b, "   CONCURRENTLY: reduced-lock index build (FB5) — takes longer, blocks writers for less time\n")
		}
		if mode := r.Statement.Flags.Extras["refresh_mode"]; mode != "" {
			fmt.Fprintf(&b, "   refresh mode: %s\n", mode)
		}
		if r.Statement.Flags.Extras["cascade"] == "1" {
			fmt.Fprintf(&b, "   CASCADE: also refreshes every materialized view this one depends on\n")
		}
		if r.Statement.Flags.Extras["with_data"] == "1" {
			fmt.Fprintf(&b, "   WITH DATA: populated immediately on commit\n")
		}
		fmt.Fprintf(&b, "   compensation: %s\n", Compensation(r.Statement))
	}
	return b.String()
}

// Compensation is the reverse-DDL hint or restore-point requirement.
func Compensation(s fbparse.Statement) string {
	if s.Verb == fbparse.VerbRefresh {
		// P7.4: Reversibility is None here for the same reason a read is —
		// "nothing to undo" — but this is NOT a read (it replaces MV data).
		// Say so plainly rather than reusing the read wording (ADR-021: never
		// say "safe"); MV data isn't in gbak backups either (same as views),
		// so a prior REFRESH can't be restored from a normal backup.
		return "none — re-running REFRESH does not restore prior MV contents (materialized view data is not backed up by gbak, same as regular views); only a database-level restore point predating this REFRESH recovers the old data"
	}
	switch s.Reversibility {
	case fbparse.ReversibilityNone:
		return "none (read)"
	case fbparse.ReversibilityReverseDDL:
		name := s.Object.Name
		switch s.Verb {
		case fbparse.VerbDrop:
			return fmt.Sprintf("recreate %s %s from a metadata extract / fb_describe (or restore point if extract is stale)", s.ObjectType, name)
		case fbparse.VerbCreate, fbparse.VerbRecreate, fbparse.VerbCreateOrAlter:
			if s.ObjectType == fbparse.ObjMaterializedView {
				// P7.4: there is no "DROP MATERIALIZED VIEW" statement —
				// README.materialized_view.md: use regular DROP VIEW for
				// either kind. Saying "DROP MATERIALIZED VIEW" here would be
				// a compensation instruction that fails if followed literally.
				return fmt.Sprintf("DROP VIEW %s", name)
			}
			return fmt.Sprintf("DROP %s %s", s.ObjectType, name)
		case fbparse.VerbGrant:
			return "matching REVOKE of the same privilege"
		case fbparse.VerbRevoke:
			return "matching GRANT of the same privilege"
		case fbparse.VerbComment:
			return "COMMENT ON … IS NULL (or restore prior comment text)"
		default:
			return "reverse-DDL from a before-image extract, or restore point"
		}
	default:
		return "not reversible in-place — verified backup required"
	}
}

// Template returns literal-scrubbed statement texts for the audit trail.
func Template(sqlText string) []string {
	var out []string
	for _, s := range fbparse.Parse(sqlText) {
		out = append(out, s.Template())
	}
	return out
}

func preview(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 40 {
		return s[:40]
	}
	return s
}
