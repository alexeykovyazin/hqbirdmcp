# Migration-planning playbook

1. `fb_info` — ODS / engine version (fail-closed version gating).
2. Fresh `fb_backup_start` then `fb_restore_test` (verification is test-restore, ADR-016).
3. Do **not** schedule `fb_restore_replace`. In-place replace stays human-gated (Tier 2, out-of-band).
4. Nightly evidence chain is K5 `nightly_verify` via `fb_schedule_create` (backup → test-restore).
5. Config drift: `fb_config_get` / `fb_config_diff` / `fb_config_set` (Tier 2).

## Applying a migration project (C.1, ADR-030)

6. `fb_migration_status` — dir vs `FBMCP_MIGRATIONS` history (applied/pending, tamper state).
7. `fb_migration_plan` — per-statement tiers, batch tier, checksums, down-section presence. Previews are informational.
8. `fb_migration_apply` — ONE confirmation for the whole batch (manifest-bound; any file change re-requests; Tier-2 statements escalate the batch out-of-band). `baseline:true` records the current schema as version 0 without DDL.
9. Rollback: `fb_migration_rollback_plan` renders the recorded down sections — execution is pasted into `fb_write` (classified and confirmed there). There is deliberately no rollback-execution tool.
