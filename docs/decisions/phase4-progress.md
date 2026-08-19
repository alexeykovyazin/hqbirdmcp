# Phase 4 progress — guarded mutations landed (core)

Date: 2026-08-17. Unit tests green across the module; `cmd/fbmcp` builds.
This is the M4 *core*, not the full M4 checklist (dogfooding week and
gap-closure audit remain).

## What landed

| Part | Status | Notes |
|---|---|---|
| P4.1 generic write executor | done | `internal/executor` + `internal/classify` (OpKey→v3); `fb_write` dual-mode; EvaluateMeta wired |
| K6 preview | done | impact + compensation + channels; row estimates via read pool; never says "safe" |
| P4.2 index | done | `fb_index_rebuild` / `fb_index_drop`; constraint-backing refusal; advisory-id bridge from `fb_index_stats` |
| P4.3 security | done | lockout package; effective-access preview (`fb_effective_access` + grant/revoke before/after, cap 200, no role expansion) |
| P4.4 session kill | done | batch ids; refuse `CURRENT_CONNECTION` |
| P4.5 K5 workflows | done | `fb_shutdown_window`; `fb_restore_replace` migrated onto K5 (CloseDB, `.pre-restore` compensate) |
| P4.6 config editor | done | ADR-020; `fb_config_get/diff/set`; atomic `.prev` |
| P4.7 COMMENT ON | done | `fb_comment_set` |
| P3.8 create-db | done as copy | `fb_db_create` copies template FDB; `fb_db_drop` Tier-3 stub |
| P3.6 trace | done | ADR-018 templates only; `fb_trace_start/stop/list`; 8 MiB drain cap |
| P3.1 nbackup | done | `fb_backup_nbackup` levels 0–2; level N requires cataloged N-1 |
| ADRs | done | 018, 019, 020, 021 |

## Still open

- K1 shared/exclusive lock modes
- Localhost approval page (marker-file OOB is the trust channel; since
  Phase 7 the fbmcp-tray popup is the primary operator surface for it)
- Role-hierarchy expansion in effective-access (capped, documented)
- Dogfooding week + Appendix A gap-closure audit
- Linux container matrix

Refreshed 2026-08-20 (phase6_plan_v2 §8 T5): service start/stop/restart
landed in P5 as Tier-2 tools behind `posture.verified`
(`fb_service_start/stop/restart`, cmd/fbmcp/p5tools.go) — removed from the
open list above.
