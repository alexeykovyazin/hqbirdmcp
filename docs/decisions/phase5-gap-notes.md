# Phase 5 gap notes (Appendix A deferrals)

Written with P5.4. These are explicit, not silent.

| Item | Disposition |
|---|---|
| POST_EVENT (v3 gap #28) | Deferred. Ops 96–100 are scheduler / index rebuild / stats / cleanup / rotation. Driver events unproven. |
| K1 shared/exclusive job locks | Out of Phase 5. Overlap = skip if DB has queued/running; Runner serializes. |
| Restore staging+atomic swap | Live path remains in-place + `.pre-restore` + `dbpool.CloseDB`. |
| Approval HTTP page | Not M5-blocking. OOB is `fbmcpctl approve` marker-file. No remote page in v1 (ADR-022). |
| Role-hierarchy expansion | Capped at 200; documented in security-audit playbook. |
| Linux Firebird containers | Best-effort. Docker image exists; host matrix deferred if Docker is down. |
| D5 OS keyring | Env fallback remains supported (ADR-009). `fbmcpctl setup` documents the residual. |
| `fb_index_advice` | **Closed (Phase 8D C.2, 2026-08-30).** Now exists: plan analysis → proposed CREATE INDEX DDL (estimate-only), applied via `fb_write`; `recheck_of` diffs after a human applies. |
