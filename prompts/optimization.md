# Optimization-goal playbook

Measured loop. Never auto-apply indexes.

1. Capture a statement with `fb_analyze_query`.
2. Review `fb_index_stats` (advisory ids). Previews are informational.
3. If a change is warranted: `fb_index_rebuild` with `mode=preview`, then human-approved `mode=execute` (or `fb_write` preview → confirm).
4. Re-run `fb_analyze_query`. Stop if the plan did not improve.

Scheduled `fb_index_rebuild` is a durable grant (`fb_schedule_create`); attribute the run to confirmer + channel + creating request id, not to a 15-minute pending token.
