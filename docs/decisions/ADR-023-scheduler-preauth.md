# ADR-023 — Scheduler semantics and pre-authorization grants

Status: accepted (2026-08-17) · Fed by: P5.3 / phase5_plan_v2.md

## Context
Scheduled mutations cannot wait for a human at 03:00. The Phase 3 exit review
said to bind pre-authorization to the existing gate arg-hash. That is
insufficient: pending actions expire in 15 minutes (`internal/gate`) and tool
args live in `gatedTools.args` in memory, deleted on dispatch.

## Decision

**Process.** The scheduler runs inside the single fbmcp process (ADR-005).
OS cron must not spawn a second server.

**Grant, not pending action.** Creating/editing/deleting a schedule is a
normal gated tool (`fb_schedule_create` / `fb_schedule_delete`) on the
existing `registerTool` / `dispatch` path. After confirmation, a durable
`state.Schedule` record is persisted:

- target (tool name or workflow type) + canonical args JSON + arg hash
- database, max tier, confirmer, confirmation channel, creating request id
- cron expression, **explicit timezone** (never server-local)
- window binding, missed-run policy, overlap policy, enabled flag

`PendingAction` is used only for the *create* confirmation. The grant outlives
the 15-minute TTL.

**Fire path never calls the gate.** The ticker checks: enabled, cron match in
the schedule timezone, overlap (skip if that DB has `queued`/`running` jobs),
maintenance window if required, arg-hash still matches stored args. Fail-closed
skip + K7 notify + audit. Then `jobs.Runner.Submit` → `gt.execs` or K5
`wf.Run`. Interrupted jobs from Runner.reconcile stay interrupted.

**Calendar.** 5-field cron. Embed `time/tzdata`. Default missed-run = skip +
report; `catchup-once` is opt-in. Default overlap = skip if previous alive.

**Forbidden targets.** Unknown names, Tier-3 (`fb_db_drop`), and
`fb_restore_replace` (in-place replace stays human-gated). `nightly_verify`
(backup → test-restore) is the M5 evidence chain.

**Tier of create.** Dynamic: max tier of the target. Tier 2 ⇒ out-of-band
confirmation at create time (fuse #7).

## Consequences
Every scheduled run audits grant lineage (schedule id, confirmer, channel,
creating request id). P6.1 claim C14 attacks forged/replayed grants.
K1 shared/exclusive lock modes are out of scope; per-DB single-flight is
the overlap primitive.
