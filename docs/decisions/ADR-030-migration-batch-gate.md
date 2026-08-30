# ADR-030: Migration batch-gate semantics

Status: accepted (2026-08-30, Phase 8D C.1)

## Context

`internal/gate` tokens one action per pending: a single classified script
(ADR-021) or tool invocation. Migrations (improvement_plan C.1) are ordered
`.sql` files applied as a set; per-file confirmation would be unusable and
per-statement confirmation would be worse. The version-table bootstrap
(`CREATE TABLE FBMCP_MIGRATIONS`) is itself a write and must not become an
end-run around the write path.

## Decision

1. **One pending action covers the whole batch.** `fb_migration_apply`
   requests a single confirmation whose impact statement lists every pending
   file with version, name and checksum. The batch tier is the **max tier
   over all classified statements** across all files (bootstrap included) —
   a Tier-2 statement anywhere escalates the whole batch to Tier 2
   (out-of-band only, verified-backup preconditions attached).
2. **argHash binds the batch manifest.** The hash is taken over canonical
   JSON `{baseline, files: [{version, name, checksum}]}`. Any file edit,
   addition or removal after the request changes the hash → `fb_confirm`
   rejects with re-request semantics (existing gate behavior, no new code
   path).
3. **Post-confirm re-validation.** The execution entry re-reads the
   directory, re-hashes every file, and refuses on any drift from the
   confirmed manifest, on a checksum mismatch against `FBMCP_MIGRATIONS`
   history (tamper), on a missing prerequisite version, or on a version
   already applied. Then it re-classifies every statement through
   `executor.Prepare` — classification happens again at execution time, not
   only at request time.
4. **Bootstrap through the executor.** When the version table does not
   exist, its `CREATE TABLE` is prepended to the batch and classified like
   any other statement (Tier 1) — it is never executed outside the classified
   path. The `--baseline` mode (record current schema as version 0 without
   DDL) is an INSERT into the same table and goes through the same gate.
5. **Execution shape (corrected during implementation).** Statements run
   per-statement autocommit (the same shape as fb_write's DDL path) and the
   history row is written after the migration's last statement succeeds.
   The originally planned per-migration single transaction is **not
   achievable in Firebird**: a statement cannot see objects created earlier
   in the same transaction (a migration that creates and then populates a
   table fails with "table unknown" — verified live on FB3). The history
   row is therefore the completion marker: a crash mid-migration leaves the
   version pending and a re-apply fails loudly on the first
   already-applied statement, with the statement text in the error. The
   batch stops at the first failure; completed migrations stay applied.
6. **Down sections are recorded, not guessed.** The file's `-- @down`
   section text is stored in the history row at apply time;
   `fb_migration_rollback_plan` renders down-scripts from history, so later
   file edits cannot silently change what a rollback would do. Rollback
   **execution** stays `fb_write`'s job (explicitly pasted, classified and
   confirmed there) — there is no rollback-execution tool.

## Deviation from the C.1 plan text

The improvement plan names the table `_FBMCP_MIGRATIONS` (leading
underscore). Firebird 3.0 refuses unquoted identifiers that start with an
underscore (verified live: `CREATE TABLE _T1` → SQL error -104), and
always-quoting the name would make every history query case-sensitive and
brittle. The table is therefore **`FBMCP_MIGRATIONS`**.

## Consequences

- A confirmed batch is reproducible: the manifest hash proves the files did
  not change between human approval and execution.
- Rollback is deliberately one step more manual than apply — reversing a
  migration is rarer and riskier than applying one, and reusing fb_write
  avoids a second privileged execution path.
- `fb_migration_status/plan/rollback_plan` stay Tier 0 (reads + rendering
  only).
