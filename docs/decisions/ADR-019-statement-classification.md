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

## Addendum (2026-09-28, WS9 T9.1 — body-bearing tier asymmetry)

EXECUTE BLOCK and EXECUTE PROCEDURE mapped to op 25 ("procedure-ish",
Medium → Tier 1), so a mutation the engine would gate at Tier 2 written
top-level rode in through a PSQL body or a stored call: no maintenance
window, no verified-backup precondition, no out-of-band approval. Two
explicit escalations now close it (v3map `mapStatement`):

- **EXECUTE BLOCK** → Tier 2 when the body is not provably read-only
  (`bodyOpaque`): row DML (`BodyInfo.HasDML`), a nested EXECUTE
  PROCEDURE/BLOCK, dynamic `EXECUTE STATEMENT` (matched on the raw text —
  deliberately crude; a false positive only raises the tier), or no
  captured body at all. A SELECT-into block stays Tier 1.
- **EXECUTE PROCEDURE** → Tier 2 unconditionally on the fb_write path:
  the stored body lives in the database and is invisible to the
  classifier — read-only cannot be proven. (`fb_query` keeps its Tier-0
  path behind the engine-enforced read-only transaction; a procedure the
  engine refuses there lands in the fb_write fallback, which now hits the
  same Tier-2 wall as a top-level mutation.)

These tiers are marked `TierFinal` so the generic low-confidence bump
does not stack on a deliberate decision. Adversarial coverage:
`p61_adversarial_test.go` (nested blocks, comment-hidden DML, dynamic
SQL, opaque procedure calls, the read-only-block floor).
