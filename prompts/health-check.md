# Health-check playbook

Walk a registered database. Previews are informational; they are not a safety guarantee.

1. `fb_info` — engine, ODS, dialect, page size, RO/ForceWrite.
2. `fb_sessions` — attachments (capped).
3. `fb_transactions` — OIT/OAT/OST/Next gap.
4. `fb_index_stats` — selectivity / duplicate advisory (`ADVISORY id=…`).
5. `fb_db_health` / `fb_ping`.
6. Summarize. Do not auto-apply mutations.
5. `fb_trends` — capacity trajectory (size projection, attachment spikes) from the sampler history; needs ≥3 samples for a projection.
