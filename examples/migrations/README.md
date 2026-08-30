# Sample migration project (C.1)

Two versions demonstrating the format: `NNN_description.sql`, an up section,
and a down section after a `-- @down` separator. `spike3` in
`fbmcp.dev.yaml` points at this directory (`migrations_dir`).

- `fb_migration_status` — applied/pending state
- `fb_migration_plan` — per-statement tier/risk, batch tier
- `fb_migration_apply` — one gated confirmation for the whole batch (ADR-030)
- `fb_migration_rollback_plan` — renders the recorded down sections;
  execution is pasted into `fb_write`
