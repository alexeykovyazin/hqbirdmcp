# Phase 3 exit review — M3 core reached

Date: 2026-08-17. All tests green (10 packages), full E2E verified live on
FB 5.0 (HQbird). Committed as `748e899`.

## What is done and proven (live)

| Part | Status | Evidence |
|---|---|---|
| P3.1 backup suite | done | `fb_backup_start` (Tier 1): policy → impact+token → confirm → job; Services-API backup; catalog registered unverified |
| P3.2 test-restore | done | `fb_restore_test`: restore-to-workdir + validate + catalog `Verified=true`; smoke shows full chain |
| P3.3 guarded restore | done | `fb_restore_replace` (Tier 2): window + verified-backup + freshness preconditions; **in-band confirm rejected with explicit channel-policy message (fuse #7 live)**; OOB via `fbmcp-approve` CLI → marker file → server watcher → pools closed → `.pre-restore` copy → replace → validate; failure path restores previous file |
| P3.4 validation | done | `fb_validate` via Services API |
| P3.5 gfix settings family | partial | sweep, ForceWrite, RO/RW mode live; page buffers (-buffers) not yet exposed |
| P3.6 trace sessions | **deferred** | not built (see corrections) |
| P3.7 service lifecycle | partial | read-only `fb_service_status` (config-driven service names); start/stop/restart NOT built (needs §4.8 posture) |
| P3.8 database lifecycle | **deferred** | create-from-template/drop/multi-file not built |

K1 (lock modes): per-DB serialization exists in the job runner (single mode);
shared/exclusive distinction deferred to P4.5 (workflow engine) — acceptable
because no current operation overlaps.

## Factual findings that change the next-phase plans

1. **The gate→job pattern is now generic** (`gatedTools.registerTool` +
   `dispatch`). Phase 4's P4.1 "generic write executor" should reuse this
   pattern rather than inventing a second one; ADR-019's classifier plugs in
   as the `Evaluate` step's input, not a new architecture.
2. **The driver's verbose channels never close** — Services-API progress must
   be drained in a background goroutine (bug found and worked around in
   `backupsvc`; also affects P3.6 trace when built).
3. **Windows file-lock reality**: replacing a database file requires closing
   our own pools first (`dbpool.CloseDB`); `os.Remove` on an attached DB
   fails silently otherwise. Any Phase-4 shutdown/replace workflow must call
   it (feeds P4.5).
4. **OOB approval is marker-file based** — CLI writes, server watcher (2 s
   poll) consumes, confirming as the pending identity with `channel: out-of-band`
   audited separately. The plan's localhost approval *page* remains
   desirable for UX but the trust mechanism is complete and tested.
5. **Maintenance windows are state-only** — no tool opens/closes them yet;
   they're edited in state.json (documented). P4.6/P5.x should add a gated
   `fb_window_open` tool.
6. **Facts composition works** (`state.CompositeFacts`): engine facts +
   catalog facts feed preconditions today; P4.x providers (attachments count
   etc.) register the same way.

## Corrections pushed into phase4_plan.md

- P4.1 T3 (execution engine): build on `gatedTools`/`dispatch`; don't
  duplicate the gate plumbing.
- P4.5 (shutdown orchestration): MUST call `dbpool.CloseDB` before file-level
  operations (finding #3).
- P6.1 safety fuses: add explicit live test for OOB marker replay/dual-marker
  and for the `.pre-restore` failure path (both already exercised in
  `cmd/t2test`; formalize as CI tests).

## Corrections pushed into phase5_plan.md

- P5.3 scheduler: reuse `jobs.Runner` + `gatedTools.execs`; pre-authorization
  binds to the arg-hash mechanism that already exists in the gate.
- P5.6 bootstrap: now also owns `fb_window_open` (or P4.6) and the read-only
  user setup for spike/production DBs (currently SYSDBA everywhere — dev only).

## Carried debts (explicit)

- P3.6 trace, P3.8 create-db: implemented in early Phase 4 window (both have
  driver APIs available; trace needs the channel-drain pattern).
- nbackup levels, K1 shared/exclusive, page-buffers tool, service
  start/stop: next-phase backlog with owners assigned above.
