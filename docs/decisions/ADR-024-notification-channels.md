# ADR-024 — Notification channels v1 (K7)

Status: accepted (2026-08-17) · Fed by: P5.3 T6 / phase5_plan_v2.md

## Context
`internal/jobs` shipped with no notification hooks. Operators need to know
when a scheduled run skipped, a job failed, or a Tier-2 confirmation is
waiting. POST_EVENT (v3 gap #28) is a separate “good to have”; v3 ops 96–100
are schedule/cleanup/rotation, not Firebird events.

## Decision

**Bus.** `internal/notify` is the only kernel addition in Phase 5 besides
the scheduler. Event sources: job/workflow succeeded|failed, gate pending
created|expired, scheduler skip, retention report.

**Channels v1.**
1. Local event log (JSONL under the state dir) — always on.
2. Signed webhook — optional. HMAC-SHA256 of the raw body; secret from env
   (or keyring). Headers: `X-FBMCP-Event-Id` (UUID, idempotency),
   `X-FBMCP-Signature` (`sha256=<hex>`).

**Delivery.** At-least-once. Three retries, exponential backoff. **No**
ordering guarantee. SMTP and chat are out of v1 (operational cost, no
current operator demand). POST_EVENT is deferred (driver events unproven).

**Replay.** Receivers must reject duplicate `X-FBMCP-Event-Id`. The bus
offers `Verify` + a small in-memory replay guard for tests; production
receivers own their own idempotency store.

**Storms.** Coalescing and backoff calibration are P6.2, not this ADR.

## Consequences
Default config has notifications = local log only. Webhook secret never
appears in audit, logs, or job output (existing scrubber). Claim C18 in
Phase 6 is signature verify + replay rejection.
