# ADR-027 — Materialized-view SQL surface (fbparse/classify)

Status: accepted (2026-08-19) · Fed by: phase7_plan.md P7.4

## Context
HQBird/Firebird 5.0 adds a materialized-view SQL extension
(`README.materialized_view.md`): `CREATE/ALTER/RECREATE MATERIALIZED VIEW`,
`REFRESH MATERIALIZED VIEW [CONCURRENTLY|DROP DATA] [CASCADE]`, and the
`ALTER VIEW ... TO MATERIALIZED` / `ALTER MATERIALIZED VIEW ... TO NOT
MATERIALIZED` conversion forms. Before this ADR, every one of these
statements was **hard-denied** by `fb_write`: `internal/classify/v3map.go`
denies whenever a mutating statement's verb is `VerbUnknown` or its OpKey
maps to v3 op 0. `REFRESH` wasn't a recognized top-level keyword at all
(`Verb` stayed `VerbUnknown`); `CREATE/ALTER MATERIALIZED VIEW` set `Verb`
but never `ObjectType` (falling to `createStmt`/`alterStmt`'s `default:`
case), which maps to v3 op 0 either way.

The 111-row v3 operations table (`firebird_dba_tasks_table_v3.md`, generated
into `internal/policy/ops_v3_gen.go`) has exactly **one** row for this (#23,
"Create / drop / refresh materialized view", Medium risk) — it doesn't
distinguish create from refresh, doesn't know about `CASCADE`, and DROP
already routes through the existing `DROP VIEW` row (materialized views have
no separate drop statement — `README.materialized_view.md`: *"There is no
separate command to drop MV, use regular DROP VIEW"*).

## Decision
1. **New `ObjectType`**: `ObjMaterializedView`, kept distinct from `ObjView`
   rather than a variant flag on it — because `DROP VIEW` legitimately drops
   either kind, a shared `ObjectType` would make that ambiguous at the
   `v3map.go` tier-lookup layer.
2. **New `Verb`**: `VerbRefresh`, dispatched from `classifier.run()`'s
   top-level switch. `REFRESH` has no other meaning in Firebird grammar, so
   an unrecognized `REFRESH ...` correctly stays a hard deny (never a false
   "read").
3. **Reversibility**: `VerbRefresh` statements get `ReversibilityNone` — not
   because they're reads (they mutate MV contents), but because there is no
   reverse-DDL for a refresh; re-running `REFRESH` doesn't restore prior
   data. `internal/classify.Compensation()` special-cases `VerbRefresh`
   explicitly instead of reusing the generic "none (read)" text that
   `ReversibilityNone` produces elsewhere, so the honesty rule (ADR-021:
   never imply safety) still holds for a genuinely different situation that
   happens to share the same `Reversibility` value.
4. **Tiering — special-case in `mapStatement`, not a new ops-table row.**
   `CREATE`/`RECREATE`/redefining `ALTER MATERIALIZED VIEW` map to v3 op 23
   (unchanged Medium risk → Tier 1). The two conversion forms (`TO
   [NOT] MATERIALIZED`) map to op 22 instead — same risk class as a plain
   `ALTER VIEW`, since the doc states dependent objects are unaffected by
   the conversion. `REFRESH ... CASCADE` escalates to Tier 2 via the existing
   contextual-escalation switch (same pattern as the DROP escalation
   already there) — multi-object blast radius, mirrors precedent rather than
   inventing a new mechanism. This intentionally does **not** touch
   `ops_v3_gen.go` / `internal/policy/gen_drift_test.go`: that generated
   table is sourced 1:1 from the 111-op catalog, and this statement family
   was never meant to be modeled there beyond its one existing row.
5. **Version floor**: every MV form calls `raiseVersion("5.0", ...)` (FR-12
   monotone floor, same mechanism as `BOOLEAN`→3.0 / `DECFLOAT`→4.0
   elsewhere in the classifier). `policy.EvaluateMeta`'s `MinFB` check was
   already there — but live verification against `spike3`/`spike4`
   (FB 3.0/4.0) initially showed it **not** enforced for `fb_write`: its
   dynamic `policy.ToolMeta` (`cmd/fbmcp/p4tools.go`) was built from
   `prep.MaxTier` only, never setting `MinFB` at all, so `raiseVersion` fed
   nothing but impact-text/confidence — a real fail-open gap, not a
   documentation nit. Fixed by adding `executor.Prepared.MinFB` (the highest
   `Statement.MinVersion` across the script, computed in
   `executor.Prepare`) and wiring it into `meta.MinFB` at the `fb_write`
   call site. Re-verified live: `CREATE MATERIALIZED VIEW` and
   `CREATE INDEX ... CONCURRENTLY` both now `DENIED: engine 4.0 does not
   meet MinFB 5.0` (and the 3.0 equivalent) on execute — preview mode never
   ran this check either way (by design, ADR-021: preview never touches the
   gate) — while plain `CREATE INDEX` without `CONCURRENTLY` on the same
   FB 4.0 database proceeds normally (no false-positive gating from an
   empty `MinFB`).
6. **`WITH DATA` vs `WITH NO DATA`**: recorded as a `Flags.Extras["with_data"]`
   marker (not a new struct field — this is a one-off modifier, unlike
   `IndexConcurrently` in ADR-028's sibling change which is reused across two
   call sites) since it changes operation blast radius (immediate data load
   vs. an empty shell) and is surfaced in `classify.Preview()`.

## Consequences
- No kernel change. No new MCP tool — materialized views ride the existing
  `fb_write` generic-DDL path once classification succeeds, same as any
  other DDL form.
- `internal/backupsvc` is unaffected: `gbak` skips MV data on backup (like
  views) and auto-refreshes MVs on restore unless `-NO_MATVIEWS` /
  `isc_spb_res_no_matviews` is passed — the pinned driver
  (`nakagami/firebirdsql@v0.9.19`) has **no** field for this restore option,
  so `fb_restore_test`/`fb_restore_replace` cannot currently suppress the
  auto-refresh. Documented as a known gap, not implemented (would need
  either a driver fork or falling back to `gbak` CLI shell-out for restore,
  neither justified by current demand).
- **Exclusive-refresh vs. pooled connections (found live, fixed in WS3.1)**:
  a non-`CONCURRENTLY` `REFRESH` initially failed with *"Can not REFRESH
  MATERIALIZED VIEW ... that is in use by concurrent transaction"* even with
  no other client attached — `internal/dbpool`'s pooled connections hold
  snapshot transactions the engine's exclusive-reservation check counts as
  concurrent use. Fixed: `executor.Prepared.NeedsExclusive` marks scripts
  containing a non-concurrent `VerbRefresh`, and the `fb_write` executor
  drains that DB's pools (`dbpool.CloseDB`, the same primitive
  `restore_replace` uses) before executing; verified live on `spike5`.
  A `CONCURRENTLY` refresh needs a unique index on the MV first (confirmed
  live: `Unique index not found`, exactly per the doc's precondition) and
  doesn't hit the exclusivity check, so it skips the drain.
- Materialized-view test coverage lives in
  `internal/fbparse/canonical_test.go:TestCanonicalMaterializedView` and
  `internal/classify/matrix_test.go`'s `canonical` table (drift-gated the
  same way every other row is — `knownOpKey` / `TestCanonicalV3Matrix`).
