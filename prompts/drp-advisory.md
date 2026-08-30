# DRP / policy advisory (v3 rows 51, 58–60, 87, 106–108)

This is reference content, not a tool. Grounded in shipped capabilities:

- **51 / 106–108** — Disaster recovery: keep a verified backup (`fb_restore_test` sets `Verified=true`). Retention default is keep-everything (`fb_retention_run` dry-run first). Uncataloged files are never deleted.
- **DR validation** — `fb_diff_schema` compares a restored copy against the live source (two registered ids, or db vs stored snapshot); `fb_diff_data` does a bounded PK-based row comparison. A `nightly_verify` schedule whose args carry `diff_schema: true` fails the chain when the restored schema diverges.
- **58–60** — Index/constraint hygiene: inspect with `fb_index_stats`; drop/rebuild is gated (`fb_index_drop` refuses constraint-backing indexes).
- **87** — Maintenance windows: `fb_window_open` (Tier 1). Tier ≥ 2 mutations require an open window.

Confirmation channels are always named (in-band vs out-of-band). Scheduled work is attributed to the Schedule grant lineage.
