# ADR-019 — Statement classification & acceptance policy

Status: accepted (2026-08-17) · Fed by: P4.1 T1

## Context
`fb_write` and every structured mutation front-end need a verb+object
classifier mapped to v3 operation rows (tier / impact). No mature Firebird
SQL parser existed in Go at Phase 0; [firebird_parser_requirements.md](../findings/firebird_parser_requirements.md)
listed the options. Safety must not depend on the classifier (claim C1).

## Decision
1. **Adopt the vendored `internal/fbparse` classifier** (pure Go, zero cgo,
   stdlib-only, MIT — candidate (d) in phase4_plan.md P4.1 T1). It is a
   classifier, not a validator. Unknown / unclosed / mangled input ⇒
   `Verb=UNKNOWN` with issues, never a guessed read.
2. **Server-side mapping** (`internal/classify`) turns `Statement.OpKey()`
   into a v3 row → `policy.TierForRisk`, then applies documented escalations
   (DROP of objects, WHERE-less DML, low confidence, RestorePoint). Unmapped
   mutating OpKeys deny (never "assume read").
3. **Acceptance rules** (defense-in-depth; gate + engine remain the
   controls):
   - unclassifiable ⇒ whole request denied
   - mixed tiers in one script ⇒ deny (split required)
   - Tier 3 content ⇒ deny (disabled by default)
   - irreversible (`RestorePoint`) content is at least Tier 2
   - stacked statements are split by `fbparse` (PSQL-body aware), never by
     naive `;` splitting
4. **DDL vs DML transactions (live finding).** Firebird invalidates a
   transaction after DDL: a later DML in the same tx can fail with
   "table unknown". Therefore:
   - DML-only scripts run in **one transaction** (atomic rollback)
   - scripts containing DDL run **per-statement** with an honest
     partial-application report (never silently claimed atomic)

## Consequences
K6 (`internal/executor` preview) and `fb_write` consume this mapping.
Classification quality is measured by the generated matrix + injection
battery; safety is proven by fuse #1 (RO pool) and fuse #2 (gate).
