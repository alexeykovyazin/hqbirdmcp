# ADR-021 — Dry-run / preview semantics (dual-mode contract)

Status: accepted (2026-08-17) · Fed by: P4.1 T2 / K6

## Context
Every mutation tool must be able to show impact before a human confirms.
Phase 3 already has a gate→pending→confirm→job pipeline
(`gatedTools.registerTool` / `dispatch`). Phase 4 must not invent a second
one (phase3-exit-review finding #1).

## Decision
Every mutation tool accepts `mode: preview | execute` (default `execute`).

**Preview** (informational only):
- Returns: impact statement (locking / exclusivity / blast radius from v3
  metadata), affected objects with size estimates where feasible,
  compensation advisory (reverse-DDL text or "restore point required"),
  verification plan, and the confirmation channels for the computed tier.
- Does **not** create a pending action, does **not** acquire locks, does
  **not** touch the admin pool. Row estimates use the **read** pool.
- Safety never derives from preview. The word "safe" is forbidden in
  preview text.

**Execute**:
- Classifies (ADR-019) → `policy.EvaluateMeta` with the dynamic tier →
  existing human gate (`Request` / `fb_confirm` or OOB) → `dispatch` →
  `internal/executor` on the admin pool.
- Re-validates preconditions at confirmation time by re-running Evaluate
  on the stored args (arg-hash already binds the request).

K6 is the single preview builder. Structured tools (index, security,
sessions, COMMENT ON, create-db) compile parameters into SQL (or a
structured impact) and call the same service.

## Consequences
`gatedTools.registerTool` grows a preview short-circuit. `fb_write` uses
the same `mode` field. Playbooks (P5.4) must phrase previews as impact, not
as a safety guarantee.
