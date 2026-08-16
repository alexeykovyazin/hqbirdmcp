# ADR-003 — Admin-execution strategy (D2)

Status: accepted (2026-08-16) · Fed by: P0.2

## Context
Backup/restore/validate/sweep/trace/stats/users can run via the Firebird
Services API (in-process through the driver) or by wrapping utilities
(gbak/gfix/nbackup/gstat/fbtracemgr/isql) as subprocesses.

## Decision
**Hybrid, API-first.** One `AdminExecutor` interface (Phase 1, P1.7) with two
backends:

1. **Services API backend** (driver managers) — preferred for: backup
   (gbak-equivalent), nbackup, restore, validate, sweep, properties, trace,
   user management, statistics. No argv, no env creds, in-process progress
   channel. Backup via API spike-verified.
2. **Subprocess backend** — for isql-only needs (plan retrieval P2.4,
   CREATE DATABASE P3.8, metadata extraction P2.6) and as universal fallback.
   Contract: absolute path, argv array, env-only credentials
   (`ISC_PASSWORD`), wall-clock timeout, output-size cap — harness proven.

## Key findings baked in
- **Standalone backup verification does not exist** (`gbak -v file.fbk` →
  "requires both input and output filenames"). Verification = test-restore
  (P3.2). P3.1's "verify" tool reframed accordingly.
- Windows trusted auth: local utilities succeed regardless of ISC_PASSWORD
  when the OS user is privileged → the service account is the local boundary
  (feeds threat T-11, §4.1).
- os/exec Windows: separate non-file Stdout/Stderr writers lose output —
  share one writer value (P1.7 implementation constraint).

## Consequences
Per-operation route table lives in `docs/findings/utility-matrix.md`; new
operations default to Services API when covered, subprocess otherwise.
