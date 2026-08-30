# Optimization-goal playbook

Measured loop. Never auto-apply indexes.

1. Capture a statement with `fb_analyze_query`; for a missing-index hypothesis run `fb_index_advice` (proposes CREATE INDEX DDL for natural scans — estimate-only, never applied).
2. Review `fb_index_stats` (advisory ids). Previews are informational.
3. If a change is warranted: `fb_index_rebuild` with `mode=preview`, then human-approved `mode=execute` — or apply proposed DDL via `fb_write` preview → confirm.
4. Re-check: `fb_index_advice` with `recheck_of` (reports whether the scan was resolved), or re-run `fb_analyze_query`. Stop if the plan did not improve.

Scheduled `fb_index_rebuild` is a durable grant (`fb_schedule_create`); attribute the run to confirmer + channel + creating request id, not to a 15-minute pending token.
