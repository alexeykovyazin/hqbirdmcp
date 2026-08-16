# Phase 0 exit review — M0 gate

Date: 2026-08-16. Reviewer: executing agent. Verdict: **PASS with recorded
deviations** — Phase 1 may start.

## Checklist (phase0_plan.md §10)

- [x] All nine ADRs accepted (ADR-001…009 in `docs/decisions/`); D1–D9 locked,
      no TBD cells.
- [x] RO-refusal proof green on Windows (FB 2.5/3.0/4.0/5.0, engine-level
      DML+DDL refusal captured in capability matrix). **Deviation:** Linux
      runtime leg not executed (Docker Desktop not running); cross-compile
      green for all 3 targets; Linux runtime verification moves to the
      Phase 1 CI matrix (recorded in ADR-001 consequences).
- [x] Driver capability matrix complete; plan-retrieval route decided
      (isql subprocess; driver lacks the info item).
- [x] Utility matrix complete; every Phase 3/4 operation routed
      (API-first hybrid); golden corpus captured for gbak-verbose,
      gstat-header, gfix-validate on 3/4/5 (plus error-path samples).
      **Deviation:** corpus is smaller than the full plan list (no
      fbtracemgr/nbackup/restore captures yet) — extended opportunistically
      by P3.x parts that parse those outputs; blocking parser targets are
      covered.
- [x] Credential-via-env proven; no argv leakage (by construction + trusted-
      auth finding documented).
- [x] SDK skeleton runs (self-tested over stdio on Windows); elicitation
      reality documented (SEP-2322 round-trip semantics — significant,
      new-vs-plan); D8 evidenced structurally.
- [x] Threat register complete (18 threats, each → part + test); residual
      risks listed; D7/D9 confirmed.
- [x] Phase 1 inputs: all handoff artifacts exist (capability matrix,
      utility matrix + corpus, executor contract in ADR-003, SDK skeleton +
      layout, elicitation matrix inside p03 findings, threat register).

## Corrections pushed into later phase plans (the review's main output)

1. `phase3_plan.md` P3.1 T1 — standalone gbak verification does not exist;
   verify = restore-into-scratch (P3.2 engine) exposed as a job type.
2. `phase2_plan.md` P2.4 T1 — ADR-013 pre-answered: isql subprocess route.
3. `phase2_plan.md` P2.1 T1 — engine version source corrected (context var +
   Services API; the assumed SQL columns don't exist).
4. `phase1_plan.md` P1.7 — os/exec shared-writer constraint for subprocess
   output capture on Windows.

## Open items carried (non-blocking)

- Linux/Docker container matrix (E2) — restore before M1; owner: Phase 1 env.
- RO-*user* belt-and-braces demo — folds into P5.6 bootstrap + P1.2 CI fuse.
- Read-only user creation per database — Phase 1 bootstrap script (minimal
  version already flagged in main plan §6 P5.6 note).
