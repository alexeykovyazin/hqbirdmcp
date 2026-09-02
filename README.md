# fbmcp — Firebird DBA MCP Server

Work-in-progress implementation per the plans in the parent directory
(`mcp_implementation_plan.md`, `phase0_plan.md` … `phase6_plan_v2.md`,
`phase7_plan.md`).

**Current status: Phase 7 (HQBird extensions, M7) closed 2026-08-19. Phase 6 (hardening) remains open — C7 kill-harness, P6.2 chaos/soak and the P6.3 runbook are the remaining M6 work, tracked as WS1 in [`improvement_plan.md`](../improvement_plan.md).**

Execution plan: [`phase6_plan_v2.md`](../phase6_plan_v2.md). Claims: [`docs/findings/claims-register.md`](docs/findings/claims-register.md).

**Claude Desktop / Claude Code:** [`docs/claude-desktop.md`](docs/claude-desktop.md) — stdio MCP, spike config, confirmations.

Everything under `spikes/` is throwaway/reference code — it intentionally cuts
corners (hardcoded ports, `masterkey` passwords) that production code must not.

## Environment (this dev host)

- Go 1.24.5, Windows amd64
- Local Firebird instances (HQbird installs, all running as services):
  - FB 2.5 — port 3052 (`C:\HQbird\Firebird25`)
  - FB 3.0 — port 3053 (`C:\HQbird\Firebird30`)
  - FB 4.0 — port 3054 (`C:\HQbird\Firebird40`)
  - FB 5.0 — port 3055 (`C:\HQbird\Firebird50`)
- Linux side: Docker Desktop present but not running; FB-version coverage on
  Linux is deferred until containers are brought up (plan E2 tracks this).

Spikes run only against dedicated spike databases created under
`C:\HQbirdData\output\fbmcp-spike\` — never against real databases.

---

# Tool reference

Catalog of tools the server advertises. Source of truth is `toolMeta` in
[`cmd/fbmcp/main.go`](cmd/fbmcp/main.go). This file is honesty-linted (C15):
every `fb_*` name here must exist, and every `toolMeta` name must appear here.

On this host, instance ids are **`fb3`**, **`fb4`**, **`fb5`**; database ids
are **`spike3`**, **`spike4`**, **`spike5`**. Tools take a registry id (`db`
or `instance`), never a file path or connection string.

## How confirmation works

| Tier | Meaning | How to confirm |
|---|---|---|
| 0 | Read / gate entry | No confirm |
| 1 | Mutation | In-band: `fb_confirm` with `request_id` + token from the pending statement |
| 2 | High impact | **Out-of-band only:** the [`fbmcp-tray`](#operator-cli-and-tray-not-mcp) Approve/Deny popup, or `fbmcpctl approve <state_dir> <request_id>` |
| 3 | Critical | Compiled in, **disabled** (`fb_db_drop`) |

Gated tools (tier ≥ 1) also accept `mode`: `preview` (impact text only) or
`execute` (default → pending action). Nested parameters go in `args`.

Async work returns a job id; poll with `fb_job_status`.

## Live config (`fbmcp.yaml`)

The kernel applies YAML in-process ([ADR-012](docs/decisions/ADR-012-config-reload.md)).
Claude and extra stdio clients keep the same D8 kernel; they do not restart.
`fb_db_list` / `fb_db_health` see new ids after apply.

| Trigger | When |
|---|---|
| File watcher | Hand-edits of `fbmcp.yaml` (directory watch, ~400ms debounce). Identical content is a no-op. |
| `fb_config_reload` | Explicit apply. Use after rotating TLS cert/key **files** when the YAML paths are unchanged (bytes are not watched). |
| `fb_db_register` | After out-of-band confirm writes YAML (add-only; no Claude/process restart). |

Reload drains only affected database/instance ids (running jobs, traces,
matching pending actions, then `CloseDB`). Add-only and `retention.keep_days`
skip drain. Timeout 30s or a refuse keeps the old snapshot.

Refused: `state.dir` change; ADR-022 (loopback bind, missing cert/key, zero
identities); duplicate database paths.

`fb_db_create` copies a file only. To manage the new file, add a
`databases:` entry and let the watcher or `fb_config_reload` pick it up.

---

## Kernel / session

| Tool | Tier | Args | What it does |
|---|---|---|---|
| `fb_ping` | 0 | — | Liveness (`pong`) |
| `fb_db_list` | 0 | — | Registered databases + online/offline |
| `fb_db_health` | 0 | `db` | Probe one registry id |
| `fb_confirm` | 0 | `request_id`, `token` | Confirm a **Tier 1** pending action (Tier ≥ 2 is rejected in-band) |
| `fb_cancel` | 0 | `request_id` | Cancel a pending action or a job |
| `fb_job_status` | 0 | `job_id` | Job state / progress |
| `fb_config_reload` | 0 | — | Re-read `fbmcp.yaml` and apply to the live kernel (no Claude/process restart) |
| `fb_demo_write` | 1 | `db` | Demo gate path; **no real mutation** |

## Read / inspect (Tier 0)

| Tool | Args | What it does |
|---|---|---|
| `fb_info` | `db` | Engine version, ODS, dialect, page size, RO / Forced Writes |
| `fb_connected_dbs` | `instance` | Active databases on that instance via Services API; maps paths back to managed `db` ids when possible |
| `fb_sessions` | `db` | Attachments (user, address, state) + running statements |
| `fb_transactions` | `db` | OIT / OAT / OST / Next and gap sizes |
| `fb_analyze_query` | `db`, `query`, optional `explain` | Access plan (read-only; 60s / output cap) |
| `fb_index_stats` | `db` | Index stats + unused/duplicate advisories |
| `fb_gstat` | `db`, optional `mode` (header\|records, default header), `tables` (records: restrict analysis), `system` | Raw `gstat` output (ADR-003 subprocess route): header page dump (`-h`, no auth, works without a running server) or record/index statistics (`-r`, authenticated), optionally limited to specific tables |
| `fb_schema_list` | `db` | User tables / views / procedures / triggers |
| `fb_describe` | `db`, `table` | Columns, types, nullability |
| `fb_activity_sample` | `db`, `seconds` (1–30) | MON$ IO / record-stat deltas |
| `fb_index_advice` | `db`, `query`, optional `recheck_of` | Plan analysis → proposed CREATE INDEX DDL (estimate-only; applied via `fb_write`; `recheck_of` diffs after applying) |
| `fb_query` | `db`, `sql`, optional `max_rows` | Default for reads: one SELECT / WITH-SELECT / EXECUTE PROCEDURE on an engine-enforced read-only transaction; rows + plan + per-table stats; mutating-procedure refusals route to `fb_write`'s gate; every call logged to `query-log.jsonl` |
| `fb_diff_schema` | `db`, optional `vs_db`, `use_snapshot`, `save_snapshot` | Schema diff between two registered databases or db vs stored snapshot (grouped CREATE/ALTER/DROP output) |
| `fb_diff_data` | `db`, `vs_db`, `table`, optional `row_cap` (default 100k), `sample_rows` | Bounded PK-based data diff; refuses above the cap |
| `fb_trends` | `db`, optional `hours` (default 24), `threshold_gb` | Capacity trends from sampler history: size projection, attachment-spike flag, IO deltas |
| `fb_migration_status` | `db` | Migration state: `migrations_dir` files vs `FBMCP_MIGRATIONS` history |
| `fb_migration_plan` | `db`, optional `baseline` | Dry-run per-statement classification for the migration batch |
| `fb_migration_apply` | `db`, optional `mode`, `baseline` | ADR-030 gated batch: one confirmation, manifest argHash-bound, re-validated at execution |
| `fb_migration_rollback_plan` | `db`, optional `to_version` | Renders down sections recorded at apply time; execution via `fb_write` |
| `fb_effective_access` | `db`, `user` | Object privileges for a user/role (cap 200; no role expansion) |
| `fb_trace_list` | `db` | Engine traces + traces this server started |
| `fb_service_status` | `instance` | OS Firebird service status (read-only) |
| `fb_config_get` | `instance`, optional `param` | Read `firebird.conf` registry keys |
| `fb_config_diff` | `instance` | Diff vs registry defaults; flags restart-required |
| `fb_schedule_list` | `db` | Durable schedule grants |
| `fb_lwmonitoring` | `instance`, optional `query` (1-4, default 1), `db` (required for levels 2-4) | HQBird lightweight monitoring (`isc_action_svc_lwmonitoring` via `fbsvcmgr`, ADR-028) — DB/attachment/transaction counts without MON$ table overhead; all Firebird versions |

Min Firebird 2.5 for the MON$-backed reads (`fb_info` … `fb_activity_sample`).

## Discovery / registration

| Tool | Tier | Args | What it does |
|---|---|---|---|
| `fb_db_register` | 2 | `instance`, `path`, optional `db` + per-db overrides (`backup_dir`, `work_dir`, `ro_user`, `ro_secret_env`, `admin_user`, `admin_secret_env`) | Persist a discovered database into `fbmcp.yaml`; applied live after **OOB** confirm |

Typical path: `fb_connected_dbs` → `fb_db_register` (`mode=preview` then
`execute`) → approve via `fbmcp-tray` or `fbmcpctl approve` → `fb_db_list`.

## Backup, restore, housekeeping

| Tool | Tier | Args | What it does |
|---|---|---|---|
| `fb_backup_start` | 1 | `db`, optional `args.parallel_workers` (1-64) | Full gbak into `backup_dir`; catalog **unverified**. `parallel_workers` is HQBird/FB5 multi-thread backup (native driver support, all versions) |
| `fb_backup_nbackup` | 1 | `db`, `args.level` 0\|1\|2 | Incremental nbackup; level N needs catalog level N−1. No parallel-workers support (page-copy based) |
| `fb_restore_test` | 1 | `db`, optional `args.parallel_workers` (1-64) | Restore newest backup into **work dir**; never touches the source DB; marks catalog verified. `parallel_workers` speeds up index creation during restore |
| `fb_restore_replace` | 2 | `db` | In-place replace from newest backup (`.pre-restore` + `CloseDB`). Needs open window + verified backup &lt; 24h. **OOB only.** Cannot be scheduled |
| `fb_retention_run` | 1 | `db`, `args.keep_days`, `args.dry_run` (default **true**) | Delete only catalog-verified artifacts past keep days. `keep_days=0` = keep everything. Uncataloged files are never touched |
| `fb_validate` | 1 | `db` | Online validation (findings only, no repair) |
| `fb_sweep` | 1 | `db` | Manual sweep |

## Database state / exclusive window

| Tool | Tier | Args | What it does |
|---|---|---|---|
| `fb_window_open` | 1 | `db`, `args.hours` (default 2, max 24) | Open a maintenance window (required for Tier 2) |
| `fb_shutdown_window` | 2 | `db`, `args.mode` force\|attach\|tran, `args.kick` | Exclusive window: pools down → `gfix -shut` → always `gfix -online`. Needs verified backup &lt; 24h. **OOB** |
| `fb_set_forcewrite` | 1 | `db`, `args.on` bool | Toggle Forced Writes |
| `fb_set_readonly` | 1 | `db`, `args.readonly` bool | Toggle read-only |
| `fb_set_page_buffers` | 1 | `db`, `args.buffers` | `gfix` page buffers |

## SQL / DDL / DCL

| Tool | Tier | Args | What it does |
|---|---|---|---|
| `fb_write` | 1+ (classified) | `db`, `sql`, `mode` | Generic script on the **admin** pool. Mixed tiers denied. Irreversible / Tier 2 needs verified backup &lt; 24h. Includes HQBird/FB5-only extensions: `CREATE/ALTER INDEX ... CONCURRENTLY`, `ALTER INDEX ... VALIDATE UNIQUE`, `CREATE/ALTER/RECREATE MATERIALIZED VIEW`, `REFRESH MATERIALIZED VIEW [CONCURRENTLY\|DROP DATA] [CASCADE]`, `ALTER VIEW ... TO [NOT] MATERIALIZED` (ADR-027; denied on engines below 5.0 by the `MinFB` gate). Note: a bare `DROP INDEX` classifies Tier 2 here — prefer `fb_index_drop` (Tier 1) |
| `fb_index_rebuild` | 1 | `args.index`, `args.action` rebuild\|statistics, optional `args.advisory_id` | INACTIVE+ACTIVE or SET STATISTICS |
| `fb_index_drop` | 1 | `args.index` or `args.advisory_id` | DROP INDEX; **refused if the index backs a constraint** |
| `fb_comment_set` | 1 | `args.on` TABLE\|COLUMN, `args.name`, `args.column`, `args.text` | COMMENT ON |
| `fb_db_create` | 1 | `db` (template id), `args.filename` (bare name) | Copy template into work dir. Add a `databases:` YAML entry, then watcher / `fb_config_reload` |
| `fb_db_drop` | 3 | — | **Disabled stub.** Dual-control not implemented |

## Users, grants, sessions, trace

| Tool | Tier | Args | What it does |
|---|---|---|---|
| `fb_user_create` | 1 | `args.user`, `args.password` | CREATE USER; password is not audited |
| `fb_user_drop` | 1 | `args.user` | DROP USER; refuses SYSDBA / operating identity / registry RO user |
| `fb_role_create` | 1 | `args.role` | CREATE ROLE |
| `fb_grant` | 1 | `args.privilege`, `args.on`, `args.name`, `args.to` | GRANT (preview shows privilege diff) |
| `fb_revoke` | 1 | `args.privilege`, `args.on`, `args.name`, `args.from` | REVOKE; lockout guards on SYSDBA / admin / RO |
| `fb_session_kill` | 1 | `args.attachment_id` or `args.attachment_ids` | DELETE FROM MON$ATTACHMENTS; **refuses the server's own connection** |
| `fb_trace_start` | 1 | `args.template` `audit-lite` \| `performance` \| `errors` | Named templates only (no free-form config) |
| `fb_trace_stop` | 1 | `args.session_id` | Stop a trace this server started |

## Instance config and OS service

| Tool | Tier | Args | What it does |
|---|---|---|---|
| `fb_config_set` | 2 | `db` or `args.instance`, `args.param`, `args.value` | Atomic `firebird.conf` write (registry-validated). Never auto-restarts. **OOB** |
| `fb_service_start` | 2 | `db` or `args.instance` | Start OS Firebird service. Refuses without `posture.verified`. **OOB** |
| `fb_service_stop` | 2 | same | Stop OS service. **OOB** |
| `fb_service_restart` | 2 | same | Restart OS service. **OOB** |

`fb_config_set` / service control change **Firebird**, not `fbmcp.yaml`. YAML
listen/TLS/identities/databases use the live-config path above.

## Scheduler (durable grants)

Fire path does **not** call the human gate. Creating a schedule is gated
once. Unknown database ids are skipped (no `Submit`).

| Tool | Tier | Args | What it does |
|---|---|---|---|
| `fb_schedule_create` | max of target | `db`, `target`, `cron` (5-field), `timezone` (IANA, required), optional `window_required`, `missed_run` skip\|catchup-once, `args` | Persist grant. Forbidden: Tier 3, `fb_restore_replace`. Allowed targets include `nightly_verify`, `fb_backup_start`, `fb_restore_test`, `fb_index_rebuild`, `fb_validate`, `fb_sweep`, `fb_retention_run` |
| `fb_schedule_delete` | 1 | `db`, `args.id` | Delete a grant |
| `fb_schedule_list` | 0 | `db` | List grants |

`nightly_verify` is a workflow: `fb_backup_start` → `fb_restore_test` (source DB untouched).

---

## Operator CLI and tray (not MCP)

These are host-side surfaces, not tools Claude can call — they exist because
Tier ≥ 2 actions require out-of-band confirmation the MCP client cannot reach.

**`fbmcp-tray`** (Windows) sits in the system tray, polls the state dir for
Tier ≥ 2 pending actions, and pops a native Approve/Deny dialog for each one.
Approving/denying writes the same marker files the CLI below writes.
Built as a GUI-subsystem binary (`-H=windowsgui` — no console window at
logon); recovered panics and dialog failures append to `tray.log` in the
state dir, and `--console` reattaches stdout/stderr for terminal diagnostics.

```
fbmcp-tray.exe [--console] [fbmcp.yaml]
```

**`fbmcpctl`** (cross-platform; absorbs the former `fbmcp-approve` alias):

```
fbmcpctl approve <state_dir> [request_id]
fbmcpctl status [config]
fbmcpctl setup [--write-posture] [config]
fbmcpctl doctor [config]
```

Spike state dir: `C:\HQbirdData\output\fbmcp-spike\state`.

Playbooks (how to combine tools): [`prompts/`](prompts/).
