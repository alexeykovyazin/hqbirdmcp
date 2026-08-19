package classify

import (
	"fmt"
	"strings"

	"github.com/aleks/fbmcp/internal/fbparse"
	"github.com/aleks/fbmcp/internal/policy"
)

// v3OpFor maps an OpKey onto a v3 operations-table row number.
// 0 means "no row" (reads, or unmapped — caller decides).
func v3OpFor(k fbparse.OpKey) int {
	v, ot, variant := k.Verb, k.ObjectType, k.Variant
	switch v {
	case fbparse.VerbSelect:
		return 0
	case fbparse.VerbInsert, fbparse.VerbUpdate, fbparse.VerbDelete, fbparse.VerbMerge:
		return 102 // closest write row: data-change / purge-class DML
	case fbparse.VerbExecuteBlock, fbparse.VerbExecuteProc:
		return 25
	case fbparse.VerbComment:
		return 109
	case fbparse.VerbSet:
		switch variant {
		case "STATISTICS":
			return 21
		case "GENERATOR":
			return 24
		case "TRANSACTION":
			return 43
		default:
			return 0
		}
	case fbparse.VerbDeclare:
		return 30
	case fbparse.VerbGrant, fbparse.VerbRevoke:
		if ot == fbparse.ObjRole && (variant == "GRANT_ADMIN_OPTION" || variant == "REVOKE_ADMIN_OPTION" || variant == "") {
			if ot == fbparse.ObjRole {
				return 35
			}
		}
		if ot == fbparse.ObjDatabase {
			return 36
		}
		return 37
	}

	// DDL family.
	switch ot {
	case fbparse.ObjDatabase:
		return 8
	case fbparse.ObjUser:
		return 31
	case fbparse.ObjRole:
		return 34
	case fbparse.ObjMapping:
		return 33
	case fbparse.ObjIndex:
		if v == fbparse.VerbAlter && (variant == "INDEX_ACTIVE" || variant == "INDEX_INACTIVE") {
			return 20
		}
		return 19
	case fbparse.ObjView:
		return 22
	case fbparse.ObjSequence:
		return 24
	case fbparse.ObjProcedure, fbparse.ObjFunction:
		return 25
	case fbparse.ObjPackage:
		return 26
	case fbparse.ObjTrigger:
		if v == fbparse.VerbAlter && variant == "" {
			return 28
		}
		return 27
	case fbparse.ObjDomain:
		return 29
	case fbparse.ObjFilter, fbparse.ObjShadow:
		if ot == fbparse.ObjFilter {
			return 30
		}
		return 10
	case fbparse.ObjTable, fbparse.ObjGlobalTempTable, fbparse.ObjExternalTable:
		switch variant {
		case "COLUMN_ADD", "COLUMN_DROP":
			return 12
		case "COLUMN_TYPE":
			return 13
		case "COLUMN_RENAME", "COLUMN_DEFAULT":
			return 14
		case "COLUMN_NOT_NULL":
			return 15
		case "CONSTRAINT_PK", "CONSTRAINT_UNIQUE", "CONSTRAINT_DROP":
			return 16
		case "CONSTRAINT_FK":
			return 17
		case "CONSTRAINT_CHECK":
			return 18
		default:
			return 11
		}
	}
	return 0
}

func v3Op(n int) (policy.V3Op, bool) {
	if n <= 0 || n > len(policy.V3Ops) {
		return policy.V3Op{}, false
	}
	o := policy.V3Ops[n-1]
	if o.Num != n {
		for _, x := range policy.V3Ops {
			if x.Num == n {
				return x, true
			}
		}
		return policy.V3Op{}, false
	}
	return o, true
}

// mapStatement returns the v3-derived tier plus documented escalations.
func mapStatement(s fbparse.Statement) Result {
	if s.Verb == "" || s.Verb == fbparse.VerbUnknown {
		return Result{Statement: s, TierKnown: false, Reason: "unclassifiable — denying"}
	}

	opN := v3OpFor(s.OpKey())

	// SET TRANSACTION is a write (op 43) even when the lexer marks it non-mutating.
	mutating := s.Mutating || (s.Verb == fbparse.VerbSet && s.OpKey().Variant == "TRANSACTION")
	if !mutating {
		return Result{Statement: s, Tier: 0, TierKnown: true, V3Op: 0, Reason: "read-only statement"}
	}
	tier := 1
	reason := fmt.Sprintf("%s %s %s", s.Verb, s.ObjectType, s.Object.Name)
	if op, ok := v3Op(opN); ok {
		tier = policy.TierForRisk(op)
		reason = fmt.Sprintf("v3 op %d (%s) → tier %d", op.Num, strings.TrimSpace(op.Action), tier)
	} else if opN == 0 {
		// mutating but unmapped: deny rather than assume a tier
		return Result{Statement: s, TierKnown: false, Reason: fmt.Sprintf("unmapped mutating OpKey %v — denying", s.OpKey())}
	}

	name := string(s.Object.Name)

	// Contextual escalations (never reduce). Documented in ADR-019.
	switch {
	case s.Verb == fbparse.VerbDrop && strings.EqualFold(string(s.ObjectType), "database"):
		tier, reason = 3, "DROP DATABASE is Tier 3 (disabled)"
	case s.Verb == fbparse.VerbCreate && strings.EqualFold(string(s.ObjectType), "database"):
		tier, reason = 1, "CREATE DATABASE (template path) — Tier 1"
	case s.Verb == fbparse.VerbDrop:
		if tier < 2 {
			tier = 2
		}
		reason = fmt.Sprintf("DROP %s %s — hard to reverse", s.ObjectType, name)
	case (s.Verb == fbparse.VerbDelete || s.Verb == fbparse.VerbUpdate) && strings.TrimSpace(s.Where) == "":
		if tier < 2 {
			tier = 2
		}
		reason = fmt.Sprintf("%s without WHERE — affects all rows", s.Verb)
	case s.OpKey().Variant == "COLUMN_TYPE" && tier < 2:
		tier = 2
		reason += " (column type change — restore point required)"
	}

	if strings.EqualFold(string(s.ObjectType), "database") && s.Verb != fbparse.VerbCreate && s.Verb != fbparse.VerbDrop && s.Verb != fbparse.VerbComment {
		if tier < 2 {
			tier = 2
		}
		reason = "database-scope DDL"
	}

	// SET TRANSACTION is often classified non-mutating by the lexer; op 43 is a write.
	if s.Verb == fbparse.VerbSet && s.OpKey().Variant == "TRANSACTION" && tier < 1 {
		tier, reason = 1, "SET TRANSACTION (lock timeout / isolation) — Tier 1"
	}

	return Result{Statement: s, Tier: tier, TierKnown: true, V3Op: opN, Reason: reason}
}
