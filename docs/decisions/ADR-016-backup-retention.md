# ADR-016 — Backup format, verification, and retention

Status: accepted (2026-08-17) · Fed by: P3.1 intent; written in P5.3 T0

## Context
Phase 3 shipped gbak backup, nbackup levels 0–2, and `fb_restore_test` as
verification. The Phase 3 plan named this ADR but it was never recorded.
Housekeeping (P5.3) must not delete artifacts until the policy is explicit.

## Decision

**Format.** Default backup is full gbak. nbackup levels 0–2 are opt-in; level
N requires a cataloged level N−1. Catalog entries carry `Kind` and `Level`.

**Verification.** gbak has no standalone verify (`gbak -v file.fbk` requires
both input and output). Verification **is** restore-into-scratch via
`fb_restore_test`. A catalog row is `Verified=true` only after that succeeds.

**Retention default: keep-everything.** `keep_days: 0` means never delete.
Deletion happens only through the explicit gated tool `fb_retention_run`
(Tier 1). Dry-run is the default; execute requires `dry_run=false`.

**What may be deleted.** Only files that are (a) registered in the catalog,
(b) `Verified=true`, and (c) older than `keep_days`. Uncataloged files,
unverified backups, and foreign files in the backup directory are never
touched. A seeded canary file must survive every test.

**Staging+swap.** Phase 3 intended restore-to-staging then atomic swap. The
live `fb_restore_replace` path is in-place + `.pre-restore` + `dbpool.CloseDB`.
That is **not** changed by this ADR (Phase 5 carry-in).

## Consequences
P5.3 retention implements this policy. P6.1 claim C13 is the canary test.
nbackup chain gaps remain unrestorable until a fresh level 0.
