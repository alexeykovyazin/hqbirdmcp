# fbmcp Operator Runbook (P6.3 T1)

Audience: the person operating fbmcp in production — who did **not** build it.
Everything here is mechanical; no Go or Firebird internals knowledge required.
Paths use the dev-host layout (`E:\Projects_2026\AIDBA\fbmcp`, state dir
`C:\HQbirdData\output\fbmcp-spike\state`); substitute your own.

Related docs: [`docs/tool-reference.md`](tool-reference.md) (generated tool
catalog — source of truth), [`docs/claude-desktop.md`](claude-desktop.md)
(MCP client wiring), [`../README.md`](../README.md), ADRs under
[`docs/decisions/`](decisions/).

---

## 1. Safety model in one paragraph

Every mutating call passes policy → human confirmation → audited execution.
Risk tiers: **0** reads (no confirmation), **1** routine mutations (confirmed
in-band by the person driving the MCP client via `fb_confirm`), **≥ 2**
dangerous (shutdown, replace-restore, config writes, service control) —
**out-of-band confirmation only**: the LLM client can never approve these,
by design. Tier 3 (`fb_db_drop`) is disabled entirely. A confirmation is
only as trustworthy as the channel it arrives on (principle §2.7).

## 2. Install

1. **Binaries**: `fbmcp.exe` (server), `fbmcpctl.exe` (operator CLI),
   `fbmcp-tray.exe` (Approve/Deny popup) — from `dist\` (build via
   [`packaging/rebuild-install.ps1`](../packaging/rebuild-install.ps1), which
   also merges the MCP client config).
2. **Config**: one YAML (`fbmcp.yaml`; example:
   [`packaging/fbmcp.yaml.example`](../packaging/fbmcp.yaml.example)).
   Databases are referenced by registry id only — never paths in client
   conversations. Secrets come from environment variables (never the YAML); an unset variable falls back to the OS keyring entry `fbmcp/<ENV_NAME>` (`fbmcpctl secret set <ENV_NAME>`, value via stdin; env always wins).
3. **State directory**: created once; holds `state.json`, `audit.jsonl`,
   `approvals\`, `instance.lock`. One state dir = one kernel (extra piped
   clients attach to it automatically).
4. **Posture check**: `fbmcpctl doctor <config>` must end `doctor: green`
   before going live. Host-privilege posture scripts live in
   [`packaging/posture/`](../packaging/posture/); `fbmcpctl setup --write-posture <config>`
   records the `posture.verified` marker — `fb_service_start/stop/restart`
   refuse to run without it.
5. **Windows service** (optional): [`packaging/windows/install-service.ps1`](../packaging/windows/install-service.ps1)
   (SCM Stop drains the kernel gracefully). Tray for Tier ≥ 2 popups:
   [`packaging/windows/install-tray.ps1`](../packaging/windows/install-tray.ps1).
6. **MCP clients**: stdio (default; leave `listen` empty) — see
   [`docs/claude-desktop.md`](claude-desktop.md). Remote HTTP requires TLS +
   API key identities (ADR-022) and refuses non-localhost binds without them.

## 3. Daily operations

| Task | Tool / command |
|---|---|
| Liveness | `fb_ping`; instance health `fb_service_status` |
| List databases + online/offline | `fb_db_list`, then `fb_db_health` per db |
| Transaction health (OIT/OAT gaps) | `fb_transactions` |
| Backup (gbak, async job) | `fb_backup_start` → note the job id → `fb_job_status` |
| Verify a backup | `fb_restore_test` (restore into the work dir — this is what marks the catalog entry *verified*; gbak has no standalone verify) |
| Restore drill (real recovery practice) | verified backup → maintenance window (`fb_window_open`) → `fb_restore_replace` (Tier 2, OOB approval) |
| Scheduled nightly backup+verify | `fb_schedule_create` with `target=nightly_verify` (durable grant; fires without re-confirmation — ADR-023) |
| Retention sweep | `fb_retention_run` — **default is keep-everything** (`keep_days: 0` = never delete, ADR-016); dry-run unless `dry_run=false` |
| Config inspect/diff | `fb_config_get`, `fb_config_diff`; apply edits with `fb_config_reload` (hot reload, no restart) |
| Schema/DDL work | `fb_write` — every statement is classified, tiered, previewable (`mode=preview`) before execution |

Jobs are async: submit → poll `fb_job_status`. On the same database they
serialize (one at a time); overlapping tools refuse while a job is live.

## 4. Confirmations — the part you must not get wrong

- **Tier 1**: the client shows a request id + token; the human says yes →
  the call to `fb_confirm` completes it. Fine for routine work.
- **Tier ≥ 2**: in-band confirmation is *rejected by the kernel* ("channel
  policy"). The only paths are out-of-band:
  - CLI marker: `fbmcpctl approve <state_dir> [request_id]` — writes a marker
    into `approvals\` that the kernel consumes within ~2 s; or
  - the tray popup (`fbmcp-tray`), Approve/Deny.
  - **There is no approval web page and there must never be one** — an
    approvable surface the LLM can reach defeats the design.
- Wrong-channel or wrong-identity attempts do **not** consume the pending
  action; it stays available until its TTL (15 min) or `fb_cancel`.
- After approval of e.g. `fb_restore_replace`: expect downtime — the kernel
  closes pools, snapshots the file to `<db>.pre-restore`, restores, revalidates,
  and brings the database back online (AutoReopen), even if the kernel itself
  is killed mid-flight (chaos-verified, claims C7a/C7b).

## 5. Update / rebuild

One command (see [`packaging/rebuild-install.ps1`](../packaging/rebuild-install.ps1)):

```powershell
cd E:\Projects_2026\AIDBA\fbmcp
.\packaging\rebuild-install.ps1 -WhatIf     # dry run
.\packaging\rebuild-install.ps1 -Force       # stop old processes, rebuild, update dist\, merge client config, doctor
```

MCP clients load servers **at startup only** — restart the client after an
update. Old binaries are kept as `*.exe~` in `dist\`.

## 6. Crash / restart recovery — what self-heals and what doesn't

Verified by the chaos harness (`internal/killharness`); do not re-derive:

| Situation after a crash/kill | Behavior on restart | Operator action |
|---|---|---|
| Job was running/queued | Marked `interrupted` ("re-submit if safe"); never auto-resumed | Re-submit if needed |
| Pending gate action, unconfirmed | Replayed from `state.json` (TTL still applies) | Confirm or let it expire |
| Workflow mid-`restore_replace` | AutoReopen resumes: restores, validates, database back online; `<db>.pre-restore` kept | Verify `fb_db_health` = online; clean up `.pre-restore` after confidence |
| Workflow mid-`shutdown_window` | AutoReopen finishes the `gfix -online` tail | Verify online |
| Stale `instance.lock` (dead PID) | Reclaimed automatically via the OS file lock | none |
| `state.json` corrupt | **Kernel refuses to start.** Evidence copy kept as `state.json.corrupt-<ts>`; every start keeps failing until you intervene | Inspect the `.corrupt-*` copy; remove/repair `state.json` deliberately — there is no silent empty restart |
| `audit.jsonl` corrupt/truncated | **Kernel refuses to start** (head-sidecar mismatch) | See §7 incident: audit-verify first; treat as tamper signal, not bad luck |
| Clock rewound (NTP) | Scheduler skips ("clock behind last fire"); no double-fire | none |

Rule of thumb: the kernel fails **closed** on anything it cannot prove
intact. A restart loop is a finding, not a bug — look at stderr.

## 7. Incident playbooks

### 7.1 "Something is wrong with a database" — start here
1. `fbmcpctl doctor <config>` — config, secrets, dirs, posture, schedules.
2. `fb_db_list` → `fb_db_health <db>`; `fb_service_status` for the instance.
3. `fb_sessions` / `fb_transactions` for attachment or OAT-gap trouble.

### 7.2 Suspected tamper or crash corruption — audit first
1. Verify the chain before touching anything:
   `fbmcpctl verify <config>` — rehashes every entry and the head sidecar;
   prints `audit chain OK: N entries` or `BROKEN` with the reason.
2. The kernel itself also refuses to start on any chain mismatch — a
   startup refusal naming `audit:` is the same signal.
3. If verification fails: preserve the whole state dir (it is the evidence),
   then investigate. Do not delete `audit.jsonl` to "fix" the startup — that
   destroys the tamper record.

### 7.3 Restore drill (practice quarterly)
1. `fb_backup_start` → wait `fb_job_status` = succeeded.
2. `fb_restore_test` → catalog entry becomes *verified* (also proves the
   backup is restorable).
3. Open a window: `fb_window_open` (Tier ≥ 2 tools require it).
4. `fb_restore_replace` → approve out-of-band (§4). Downtime until online.
5. Verify: `fb_db_health`, then a real read (`fb_schema_list`, `fb_info`).
6. The previous database file remains as `<db>.pre-restore` until removed.

### 7.4 Kernel down / MCP client cannot connect
1. `fbmcpctl status <config>` — is a kernel alive? Check `instance.lock` PID.
2. Stderr of the service/client log names the failing component (config,
   state, audit, attach). Each failure message names the file.
3. If another fbmcp is active: extra piped clients **attach** automatically;
   a second *kernel* fails fast by design — run one per state dir.

## 8. Troubleshooting table

| Symptom | Meaning / fix |
|---|---|
| Tier-2 "confirmation rejected: channel policy" | Expected in-band. Use `fbmcpctl approve` or the tray (§4). |
| "another fbmcp instance is active" | A kernel holds the lock. Use `fbmcpctl status` to find it; do not start a second kernel on the same state dir. |
| Attach fails with "no attach endpoint" | The lock-holder is an old binary or died between lock and attach listener. Stop stale processes; retry. |
| `Error loading plugin MySQLEngine … module could not be found` when attaching | **Misleading**: on HQBird builds this usually means the database *file does not exist* (engine-provider scan fails first). Check the file path before blaming plugins. |
| Job stuck "running" | Same-db jobs serialize; check a live job via `fb_job_status`, or `fb_cancel`. Interrupted-after-restart jobs never resume alone. |
| `fb_restore_replace` denied: preconditions | Need a *verified* backup < 24 h old: run `fb_backup_start` + `fb_restore_test`, and open a maintenance window. |
| Service control denied | `posture.verified` marker missing (run `fbmcpctl setup --write-posture`) or the OS account lacks service rights (ADR-017). |
| Tools missing for one database only | That database is offline (`fb_db_health`) — tools degrade per-database by design. |

## 9. State directory reference

| Path | Meaning |
|---|---|
| `state.json` (+ `.tmp`, `.corrupt-*`) | Kernel snapshot: pendings, jobs, catalog, workflows, schedules. Atomic writes; corrupt ⇒ refuses to start |
| `audit.jsonl` (+ `.head`) | Hash-chained audit log. `.head` sidecar detects truncation |
| `approvals\<request_id>` | OOB approval markers (consumed within ~2 s) |
| `instance.lock` | Single-kernel lock (PID inside is diagnostic only) |
| `attach.addr`, `attach.token` | Attach endpoint for extra piped MCP clients |

## 10. Walkthrough status (P6.3 T6)

Pending: a solo (24 h cooling + checklist) or two-engineer independent
walkthrough on a fresh Windows host using this runbook only. Findings fold
back into this file.
