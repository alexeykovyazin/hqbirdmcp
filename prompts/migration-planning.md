# Migration-planning playbook

1. `fb_info` — ODS / engine version (fail-closed version gating).
2. Fresh `fb_backup_start` then `fb_restore_test` (verification is test-restore, ADR-016).
3. Do **not** schedule `fb_restore_replace`. In-place replace stays human-gated (Tier 2, out-of-band).
4. Nightly evidence chain is K5 `nightly_verify` via `fb_schedule_create` (backup → test-restore).
5. Config drift: `fb_config_get` / `fb_config_diff` / `fb_config_set` (Tier 2).
