# Phase 1 progress review — M1 core complete

Date: 2026-08-16. Verdict: **M1 core reached** (kernel + demo gated tool,
end-to-end green). Deferred items listed below carry into the Phase 2 window
rather than blocking it — Phase 2 consumes the kernel interfaces, which are
now stable and tested.

## What is done (all tests green, committed)

| Part | Status | Notes |
|---|---|---|
| P1.1 registry & config | done | YAML, fail-closed validation, registry-ID-only lookups |
| P1.2 connection layer | done | RO/admin pools, env-only secrets, per-DB degraded health; **fuse #1 green** (engine refuses DML+DDL on RO pool, FB 2.5–5.0) |
| P1.3 identity | done (local form) | local identity with tier ceiling + DB scope; remote identities → P5.1 |
| P1.4 audit | done | hash-chained JSONL, head sidecar (truncation), resume, secret scrubbing — tamper/truncate/scrub tests |
| P1.5 policy engine | done | Evaluate API, tiers, ceilings, DB scope, maintenance windows, declarative preconditions over facts providers (fail-closed) |
| P1.6 human gate | done | pending actions, single-use TTL tokens bound to identity+action, channel policy; **fuse #7 green** (Tier-2 in-band/elicitation rejected, OOB succeeds) |
| P1.7 job manager | done | per-DB serial goroutines (single-flight), persistence, restart reconciliation, cooperative cancel |
| P1.8 state store | done | atomic single-writer store: pending/jobs/catalog/windows + facts interface (stubs until P2.1/P3.1) |

M1 demo verified over stdio: `fb_demo_write` → impact statement + token →
`fb_confirm` (no-token rejected, replay rejected) → job → `fb_job_status`.

## Deviations from phase1_plan.md (accepted, with rationale)

1. **OOB approval surface (localhost page + CLI) not yet built** — the gate
   implements the channel policy and the OOB channel constant, and
   fuse #7 tests it, but the HTTP page/CLI land with the first real Tier-2
   consumer (P3.3) unless pulled earlier. Rationale: no Tier-2 tool exists
   before Phase 3; the contract is tested regardless.
2. **Tool metadata not yet generated from the v3 table** — demo set is
   hand-written and marked as such; the generator + CI diff comes with the
   first real tools (Phase 2 entry task).
3. **D8 instance lock file not yet enforced** — single-process is currently
   by convention; the lock is a Phase-2 entry task (needed before any
   dogfooding).
4. **Elicitation (InputRequests round-trip) not wired into fb_confirm** —
   in-band token works; the elicitation adapter is cosmetic until a real
   client-UX consumer exists (P2 dogfooding decides).

## Corrections for phase2_plan.md (the review output)

1. **Entry tasks added before P2.1:** (a) D8 lock file enforcement +
   fuse #6 test; (b) tool-metadata generator from
   `firebird_dba_tasks_table_v3.md` + CI diff (single source of truth).
2. P2 tools consume `dbpool.ReadOnly()` transactions (not ad-hoc
   `db.Query`) so fuse #1 covers every read tool structurally.
3. Heavy-read guard: jobs.Runner's per-DB serialization doubles as the
   sampler's execution substrate (P2.7) — note to avoid a second queue.
4. fb_confirm error text for a missing token should say "missing token"
   (currently reuses the replay message) — UX fix folded into P2 entry.

## Handoff to Phase 2

Kernel interfaces consumed by Phase 2: `dbpool.Manager` (read pools),
`policy.Engine` (Tier-0 allow decisions + version gating via facts),
`audit.Logger` (every tool call), `jobs.Runner` (P2.7 sampler),
`state.Store` (facts providers registration).
