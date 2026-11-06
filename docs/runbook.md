# fbmcp — full guide: install and use

Audience: the person who installs and runs fbmcp, and did **not** build it.
No Go or Firebird internals knowledge is needed. State as of 2026-11-04
(phases 1–12: local and remote mode, fbagent bridge, result grid).

Short versions: [English](quickstart-en.md) | [Русский](quickstart-ru.md) |
[Português](quickstart-pt.md). Details: [remote.md](remote.md) (remote
mode), [agent-bridge.md](agent-bridge.md) (fbagent),
[tool-reference.md](tool-reference.md) (every tool, its tier and arguments),
[claude-desktop.md](claude-desktop.md) (client wiring on the dev host),
ADRs in [decisions/](decisions/).

---

## 1. What fbmcp is

fbmcp is an MCP server for Firebird 2.5–5.0 on Windows and Linux. An MCP
client (Claude Desktop, Claude Code, Cursor, Codex, ZCode, others) uses it
to work as a DBA: monitoring, backups and verified restores, SQL/DDL/DCL,
indexes, users and grants, `firebird.conf`, a scheduler, migrations, and the
HQbird fbagent. It has **98 tools**: 51 read-only (Tier 0), 32 Tier 1,
14 Tier 2, 1 Tier 3 (disabled).

| Binary | Role |
|---|---|
| `fbmcp` | The server. Default: MCP over stdio (a client starts it). `-serve`: no stdio, runs as a service (scheduler, attach socket, HTTPS listener if configured). `fbmcp bridge`: a stdio client for a remote fbmcp |
| `fbmcpctl` | Operator CLI: `doctor`, `approve`, `secret`, `apikey`, `tls`, `verify`, `status`, `ping`, `setup`, `repair`, `gate` |
| `fbmcp-tray` | Windows tray: Approve/Deny pop-up for requests that need an operator |

One **kernel** per state directory. A second client with the same config
does not start a second kernel: it attaches to the running one.

## 2. Safety model

Every call passes policy → human confirmation → audited execution.

| Tier | What | How it is confirmed |
|---|---|---|
| 0 | Reads (engine-enforced read-only transactions) | Not needed |
| 1 | Routine changes: DML/DDL, backup, users, grants | In-band: the client shows a request id and a token; the person agrees; the client calls `fb_confirm` |
| 2 | High impact: service control, restore-replace, `firebird.conf`, exclusive window, registration, agent restarts | **Out-of-band only:** the tray or `fbmcpctl approve`. The client can never approve it |
| 3 | Critical (`fb_db_drop`) | Disabled by design |

- Tier 2 usually needs an open maintenance window (`fb_window_open`, on
  the same `db`) and a **verified** backup younger than 24 hours. The
  refusal says what is missing.
- A pending request expires after **15 minutes**. A wrong-channel attempt
  does not consume it; `fb_cancel` removes it.
- Every gated tool accepts `mode: preview`: it shows the impact and does
  nothing.
- Every decision (allow, pending, approved, denied) goes to the
  hash-chained `audit.jsonl`.
- There is no approval web page, and there must never be one: a page the
  model can reach would break the design.

## 3. Choose a setup

| Setup | Where fbmcp runs | Use it when | Section |
|---|---|---|---|
| A. Local stdio | Your PC; the client starts it | One admin, Firebird on this PC or in the LAN | §6 |
| B. Local service | Windows service on the DB host (`-serve`) | Schedules must run when no client is open | §7 |
| C. Remote | Service on the DB server, HTTPS + API keys | Several admins, a distant server, autonomous agents | §8 |

In setup A the kernel lives only while a client runs, so **schedules fire
only then**. Use B or C for unattended schedules (`nightly_verify`).

**fbmcp in HQbird.** HQbird 2027 setups and fbagent install fbmcp by
default: one service per server for all Firebird lines, set up by
`fbmcp ctl hqbird-install`. On such a server do not follow §6–§8; read
[`hqbird.md`](hqbird.md) (layout, remote mode with `--allow-from`, API keys).

## 4. Get the binaries

Requirements: Go ≥ 1.25.13 (to build), CGO not needed; Firebird 2.5–5.0;
fbagent ≥ 2.67 with the Local API for the agent tools (optional).

```powershell
git clone https://github.com/alexeykovyazin/hqbirdmcp.git
cd hqbirdmcp
$env:CGO_ENABLED = "0"
go build -trimpath -ldflags "-s -w" -o dist/fbmcp.exe ./cmd/fbmcp
go build -trimpath -ldflags "-s -w" -o dist/fbmcpctl.exe ./cmd/fbmcpctl
go build -trimpath -ldflags "-s -w -H=windowsgui" -o dist/fbmcp-tray.exe ./cmd/fbmcp-tray
```

Linux binaries (cross-build from Windows, or build on Linux without
`GOOS`):

```powershell
$env:GOOS = "linux"; $env:GOARCH = "amd64"
go build -trimpath -ldflags "-s -w" -o dist/fbmcp ./cmd/fbmcp
go build -trimpath -ldflags "-s -w" -o dist/fbmcpctl ./cmd/fbmcpctl
```

Checksums: `Get-FileHash dist/*` or `sha256sum dist/*`. More targets:
[packaging/README.md](../packaging/README.md). Put the binaries in a folder
of their own (examples use `C:\fbmcp\`): `mkdir C:\fbmcp; copy dist\*.exe C:\fbmcp\`
(Linux: `install -m 0755 dist/fbmcp dist/fbmcpctl /usr/local/bin/`).

`fbmcp ctl` and `fbmcpctl` are the same program. `fbmcpctl help` lists the
commands. Commands that read the config take `--config <file>`; without it
they use `FBMCP_CONFIG`, then `fbmcp.yaml` in the current folder, then next
to the program, then the HQbird default (`C:\HQbirdData\fbmcp\fbmcp.yaml`,
`/etc/fbmcp/fbmcp.yaml`), and print `using config <path>`.

## 5. Configure `fbmcp.yaml`

Full example: [packaging/fbmcp.yaml.example](../packaging/fbmcp.yaml.example).
Minimal Windows config:

```yaml
state:
  dir: C:\ProgramData\fbmcp\state
# secrets:
#   dir: C:\ProgramData\fbmcp\secrets   # setups B and C
instances:
  - id: fb5
    addr: localhost:3050
    bin_dir: C:\Program Files\Firebird\Firebird_5_0   # folder with gbak, gfix, gstat
    version: "5.0"
    service: FirebirdServerDefaultInstance            # Linux: firebird.opt_fb50
    service_user: SYSDBA                              # instance tools (fb_firebird_log, ...)
    service_secret_env: FBMCP_FB5_SERVICE_PW
databases:
  - id: main                 # tools and chats use the id, never the path
    instance: fb5
    path: D:\Data\main.fdb
    backup_dir: D:\fbmcp\backups
    work_dir: D:\fbmcp\work  # restore tests; Linux: not under /tmp
    ro_user: FBMCP_RO        # a read-only Firebird user
    ro_secret_env: FBMCP_MAIN_RO_PW
    admin_user: SYSDBA
    admin_secret_env: FBMCP_MAIN_ADMIN_PW
notify:
  webhook_url: ""            # optional: a chat webhook for pending requests
retention:
  keep_days: 0               # 0 = never delete backups
```

**Secrets.** The YAML holds only variable **names**. fbmcp looks up each
value in this order: environment variable → file `<secrets.dir>/<NAME>` →
OS keyring of the account that runs fbmcp.

```powershell
C:\fbmcp\fbmcpctl.exe secret set FBMCP_MAIN_RO_PW                                     # keyring of the current user (setup A)
C:\fbmcp\fbmcpctl.exe secret set --dir C:\ProgramData\fbmcp\secrets FBMCP_MAIN_RO_PW  # secrets dir (setups B, C)
```

The value is read from stdin (type it, then Ctrl+Z, Enter; Linux Ctrl+D).
A service account (LocalSystem, `NT SERVICE\...`) cannot see your keyring:
use `secrets.dir` for services. A changed secret file applies in a few
seconds without a restart.

**Read-only user.** Create a separate Firebird user for `ro_user` (SQL:
[quickstart-en.md §3 step 2](quickstart-en.md#3-local-install-windows):
`CREATE USER FBMCP_RO`, `GRANT SELECT` on every table, and on Firebird 4.0+
a role with `MONITOR_ANY_ATTACHMENT` so `fb_sessions` sees all connections).
SYSDBA as the RO user is for dev only; `doctor` warns about it.

**Live reload.** fbmcp applies changes to `fbmcp.yaml` by itself (file
watcher), or on `fb_config_reload`. No client or process restart. A reload
waits for jobs on the changed databases (30 s), then applies. Refused:
a `state.dir` change, duplicate database paths, an insecure remote config.

**Check:** `C:\fbmcp\fbmcpctl.exe doctor` must end with `doctor: green`
(remote mode: `doctor --remote`, no FAIL lines). It makes a missing state
folder, logs in to every database with both users, and each FAIL line ends
with `Fix:` and the command to run.

## 6. Setup A — local stdio

Use absolute paths. Do not set `listen`. Restart the client after a config
change: clients load servers only at start.

| Client | Where | Entry |
|---|---|---|
| Claude Desktop | Settings → Developer → Edit Config (`%APPDATA%\Claude\claude_desktop_config.json`) | `"mcpServers": {"fbmcp": {"command": "C:\\fbmcp\\fbmcp.exe", "args": ["-config", "C:\\fbmcp\\fbmcp.yaml"]}}` |
| Claude Code | command line | `claude mcp add --transport stdio fbmcp -- "C:\fbmcp\fbmcp.exe" -config "C:\fbmcp\fbmcp.yaml"` |
| Cursor | `%USERPROFILE%\.cursor\mcp.json` or `.cursor\mcp.json` in the project | same JSON as Claude Desktop |
| Codex | `codex mcp add fbmcp -- C:\fbmcp\fbmcp.exe -config C:\fbmcp\fbmcp.yaml` or `%USERPROFILE%\.codex\config.toml` | `[mcp_servers.fbmcp]` `command = 'C:\fbmcp\fbmcp.exe'` `args = ["-config", 'C:\fbmcp\fbmcp.yaml']` |
| ZCode | `.zcode\config.json` in the project | `"mcp": {"servers": {"fbmcp": { ...same command and args... }}}` |

Passwords are in the keyring (§5), so no `env` block is needed. If you put
a password into a client `env` block anyway, keep that file out of git.

Then start the tray for Tier 2 approvals:
`C:\fbmcp\fbmcp-tray.exe C:\fbmcp\fbmcp.yaml` (at every logon:
`.\packaging\windows\install-tray.ps1 -Bin C:\fbmcp\fbmcp-tray.exe -Config C:\fbmcp\fbmcp.yaml`).

Smoke test in the client: "call `fb_ping`" (shows the server name and your
identity with its max tier), then "`fb_db_list`", then "`fb_db_health` for main".

## 7. Setup B — local Windows service

Elevated PowerShell:

```powershell
.\packaging\windows\install-service.ps1 -Bin C:\fbmcp\fbmcp.exe -Config C:\fbmcp\fbmcp.yaml
sc.exe start fbmcp
```

- The service runs `fbmcp -serve` as LocalSystem, restarts on failure, and
  stops cleanly on a service stop.
- Use `secrets.dir` for passwords (§5).
- Lock the folder: only SYSTEM and Administrators may write to `C:\fbmcp`
  (the script prints the `icacls` command). A folder made under `C:\` lets
  every user change the files, and the service runs them as SYSTEM.
- The tray user and local stdio clients need read/write on the state dir:
  `icacls C:\ProgramData\fbmcp\state /grant "<user>:(OI)(CI)M"`. A stdio
  client with the same config then attaches to the service kernel.
- Readiness: `C:\fbmcp\fbmcpctl.exe ping C:\fbmcp\fbmcp.yaml`.
- Log: `<state_dir>\fbmcp.log` (5 × 10 MiB; crashes in `fbmcp-crash.log`).

For a service that admins reach over the network, use setup C instead.

## 8. Setup C — remote (HTTPS + API keys)

Full procedure: [remote.md](remote.md). Short form:

**Windows server** (elevated):
1. Binaries and `fbmcp.yaml` in `C:\fbmcp\` (template: remote.md §3: `listen`, `tls`, `identities`, `allowed_origins`, `secrets.dir`).
2. `C:\fbmcp\fbmcpctl.exe tls selfsign --out C:\fbmcp\tls --dns srv1 --ip 10.0.0.5 --grant "NT SERVICE\fbmcp"` — note the pin `sha256:...`.
3. One key per person or agent: `C:\fbmcp\fbmcpctl.exe apikey new alice --max-tier 2 --secrets-dir C:\ProgramData\fbmcp\secrets` (printed once). With `--config C:\fbmcp\fbmcp.yaml` it also adds the `identities:` entry and uses `secrets.dir`.
4. Passwords and agent tokens: `fbmcpctl secret set --dir C:\ProgramData\fbmcp\secrets NAME`.
5. `.\packaging\windows\install-remote.ps1 -Port 8443 -AllowFrom 10.0.0.0/24 -FirebirdService FirebirdServerDefaultInstance -WhatIf`, then without `-WhatIf`. It creates the service, the firewall rule, the folder ACLs and the Firebird service rights.
6. `C:\fbmcp\fbmcpctl.exe doctor --remote C:\fbmcp\fbmcp.yaml` → no FAIL; `curl.exe --cacert C:\fbmcp\tls\tls.crt https://srv1:8443/healthz` → `ok`.
7. Tray for the approver: `.\packaging\windows\install-tray.ps1`.

**Linux server** (root): copy `fbmcp`, `fbmcpctl` and `packaging/linux/`, then

```sh
install -m 0755 fbmcp fbmcpctl /usr/local/bin/
sh packaging/linux/install-remote.sh --user firebird --port 8443 --allow-from 10.0.0.0/24 --dry-run
sh packaging/linux/install-remote.sh --user firebird --port 8443 --allow-from 10.0.0.0/24
```

The script makes the folders, the certificate, a first config with an
`admin` key (printed once), the systemd unit, the sudo rule for the
Firebird unit and the ufw rule, then runs `doctor --remote`. Add
`databases:` and `agents:` to `/etc/fbmcp/fbmcp.yaml`. Approvals on Linux:
`sudo -u firebird fbmcpctl approve /var/lib/fbmcp [id]` (no tray; it asks y/N, `--yes` does not ask).

**Admin PC:**
1. `C:\fbmcp\fbmcpctl.exe secret set FBMCP_KEY_SRV1` (the key you got).
2. Any stdio client (Claude Desktop, Cursor, Codex, ZCode): the §6 entry
   with these `args`:
   `["bridge", "--url", "https://srv1:8443/mcp", "--key-env", "FBMCP_KEY_SRV1", "--pin", "sha256:<hex>"]`.
   The pin checks the server key, so a self-signed certificate needs no CA
   setup.
3. Claude Code can also connect directly over HTTP (`.mcp.json`
   `"type": "http"`, header `Authorization: Bearer ${FBMCP_KEY_SRV1}`,
   `NODE_EXTRA_CA_CERTS` for a self-signed cert). Do not pass the key with
   `claude mcp add --header`: it is stored in plain text.
4. Test: `fb_ping` shows the server name and your identity.

**Autonomous agents** (OpenClaw and similar): a separate key with
`apikey new openclaw-1 --profile autonomous --allow-from 10.0.0.21/32`.
Read-only by default; every Tier 1 request waits for an operator, and
`fb_confirm` from the agent is refused. Limits: remote.md §6–7.

## 9. fbagent bridge (optional)

The agent tools (`fb_agent_status`, `fb_agent_job_run`, ...) use the HQbird
fbagent Local API on the same host.
They are off until `agents:` has an entry.

1. In the client: "`fb_agent_discover`". It lists the local agents with
   `base_url`, `instance_id` and the suggested token name. It never reads
   tokens.
2. The operator stores the agent's **`local_api.token`** (not
   `agent.auth_token`): `fbmcpctl secret set [--dir <secrets.dir>] FBMCP_AGENT_TOKEN_P<port>`.
   Never type a token into the chat.
3. In the client: "`fb_agent_register` for that agent" → approve in the
   tray. fbmcp adds the `agents:` entry, reloads and checks the identity.
4. Test: `fb_agent_status`, `fb_agent_instances`.

`fb_agent_config_set` (change job settings: backup, sweep, ...) also needs
`config_path` on the entry and read access to the live `agent_config.json`
plus create rights on its folder. That file holds the token and Firebird
credentials, so this grant is the operator's decision
([agent-bridge.md](agent-bridge.md)). When the agent check fails, the tool
writes nothing.

## 10. Daily use

Ask the client in plain words; it picks the tools. Examples:

| Task | Tools |
|---|---|
| Is everything up? | `fb_ping`, `fb_db_list`, `fb_db_health`, `fb_service_status` |
| Read data | `fb_query` (read-only, rows + plan + per-table stats; a grid in MCP-Apps clients); check a draft with `fb_query_check` |
| Sessions, long transactions | `fb_sessions`, `fb_transactions`, `fb_session_kill` (Tier 1) |
| Slow query | `fb_analyze_query`, `fb_index_advice` → apply with `fb_write`; `fb_agent_perf_report` |
| Backup | `fb_backup_start` → job id → `fb_job_status` |
| Verify a backup | `fb_restore_test` (restores into `work_dir`, marks the backup verified) |
| Nightly backup + verify | `fb_schedule_create` with `target=nightly_verify` (confirmed once; setup B or C) |
| Schema / DDL | `fb_schema_list`, `fb_describe`, `fb_write` (each statement is classified and tiered), `fb_diff_schema` |
| Migrations | `fb_migration_status`, `fb_migration_plan`, `fb_migration_apply` |
| Server config | `fb_config_get`, `fb_config_diff`, `fb_config_set` (Tier 2) |
| Logs | `fb_firebird_log` |
| Capacity | `fb_trends`, `fb_gstat`, `fb_index_stats` |

Jobs are async: submit → `fb_job_status`. Jobs on one database run one at
a time. A job survives an fbmcp restart only as `interrupted`: submit it
again if it is still needed.

`fb_config_set` changes only its own line in `firebird.conf` (comments,
indentation and line ends stay), keeps the old file as `firebird.conf.prev`,
and never restarts Firebird. Restart with `fb_service_restart` (Tier 2): it
waits until the service is stopped and running again.

## 11. Operator tasks

| Task | Command |
|---|---|
| List / approve / deny requests | `fbmcpctl approve`; `fbmcpctl approve <id>`; `fbmcpctl approve --deny <id> --reason "..."` (or the tray) |
| Rotate a key | `fbmcpctl apikey rotate alice --config <fbmcp.yaml>` (old key stops in seconds) |
| List keys | `fbmcpctl apikey list` (names, sources, tiers; never values) |
| Remove a person | delete the `identities:` entry, then `fbmcpctl secret drop --dir <dir> NAME` |
| Renew the certificate | `fbmcpctl tls selfsign --renew --out <dir> --dns ... --ip ...` (pin unchanged, no restart) |
| Check the audit chain | `fbmcpctl verify` |
| Is the kernel alive? | `fbmcpctl ping`; `fbmcpctl status` (offline read of the state) |
| Allow service control | `packaging/posture/verify.ps1` or `verify.sh`, then `fbmcpctl setup --write-posture` |
| Upgrade | stop the service (or the clients), replace the binaries, start, `doctor` |
| Remove | Windows: `sc.exe stop fbmcp; sc.exe delete fbmcp; Remove-NetFirewallRule -DisplayName fbmcp-remote`. Linux: remote.md §10. Keep the state dir (audit) unless the owner says otherwise |

Dev host only: `.\packaging\rebuild-install.ps1 -Force` rebuilds `dist\`,
stops and replaces the running binaries (old ones kept as `*.exe~`),
merges client configs and runs `doctor`.

## 12. Crash and restart recovery

Verified by the chaos harness (`internal/killharness`).

| After a crash or kill | On restart | Operator action |
|---|---|---|
| Job was running or queued | Marked `interrupted`; never resumed | Submit again if needed |
| Pending request, not confirmed | Restored from `state.json` (TTL still applies) | Confirm or let it expire |
| `fb_restore_replace` in progress | Finishes: restore, validate, database online; `<db>.pre-restore` kept | Check `fb_db_health`; delete `.pre-restore` later |
| `fb_shutdown_window` in progress | Finishes the `gfix -online` step | Check online |
| Stale `instance.lock` | Taken over automatically | None |
| `state.json` corrupt | **Kernel refuses to start**; copy kept as `state.json.corrupt-<ts>` | Inspect; repair or remove `state.json` on purpose (`fbmcpctl repair` rebuilds schedule grants from the audit) |
| `audit.jsonl` corrupt or cut | **Kernel refuses to start** | §13.2: treat it as a tamper signal |
| Clock moved back | Scheduler skips; no double run | None |
| Windows service process killed | SCM restarts it in about 5 s | None |

The kernel fails **closed** on anything it cannot prove intact. A restart
loop is a finding: read the log.

## 13. Incident playbooks

### 13.1 Something is wrong with a database
1. `fbmcpctl doctor <config>`: config, secrets, folders, posture, schedules.
2. `fb_db_list` → `fb_db_health <db>`; `fb_service_status` for the instance.
3. `fb_sessions`, `fb_transactions` for attachment or OAT-gap problems;
   `fb_firebird_log` for engine errors.

### 13.2 Suspected tamper or corruption: audit first
1. `fbmcpctl verify <config>` → `audit chain OK: N entries` or `BROKEN` with the reason.
2. If it fails: keep the whole state dir (it is the evidence), then
   investigate. Do not delete `audit.jsonl` to make the kernel start.

### 13.3 Restore drill (every quarter)
1. `fb_backup_start` → `fb_job_status` = succeeded.
2. `fb_restore_test` → the backup is verified.
3. `fb_window_open` for the database.
4. `fb_restore_replace` → approve out-of-band. Downtime until online.
5. Check: `fb_db_health`, then a real read (`fb_schema_list`, `fb_info`).
6. The old file stays as `<db>.pre-restore` until you delete it.

### 13.4 Client cannot connect
1. `fbmcpctl ping <config>`, `fbmcpctl status <config>`.
2. Read the client's MCP log (stdio) or `<state_dir>\fbmcp.log` / `journalctl -u fbmcp` (service).
3. "another fbmcp instance is active": one kernel per state dir. Piped
   clients attach by themselves; a second kernel fails fast by design.

## 14. Troubleshooting

| Symptom | Meaning / fix |
|---|---|
| Tools do not appear | JSON syntax, absolute paths, full client restart; the client's MCP log |
| Server exits at once | `fbmcpctl doctor`; a missing secret; a service cannot read your keyring (use `secrets.dir`) |
| Tier 2 "confirmation rejected: channel policy" | Expected in the chat. Approve in the tray or with `fbmcpctl approve` |
| Tier 2 "no open maintenance window" | `fb_window_open` for the **same** `db` you pass to the tool |
| `fb_restore_replace` denied: preconditions | Needs a verified backup < 24 h: `fb_backup_start` + `fb_restore_test`, and a window |
| Service control denied | `posture.verified` missing (`fbmcpctl setup --write-posture`) or the account has no rights on the Firebird service |
| `fb_config_set`: cannot write | The account may not change that file (Linux unit `ProtectSystem=strict`; Windows ACL). Edit by hand, or see remote.md §8 |
| Tray shows nothing | It shows only requests that need an operator; check the tray user's rights on the state dir and `tray.log` there |
| 401 (remote) | Wrong key, or the server did not load it: `apikey list`, `doctor --remote` |
| 403 (remote) | Source IP not in `allow_from` / firewall, or an Origin header not in `allowed_origins` |
| 429 (remote) | Limits; wait for `Retry-After` |
| Pin mismatch | The server key changed; get the new pin from the server admin |
| `agent-unreachable` | Agent service down, `local_api.enabled: false`, or wrong `base_url` (default `127.0.0.1:(10000 + firebird port)`) |
| `agent-auth` | The stored token is not the agent's `local_api.token`; set it again |
| `agent-timeout` on restarts | `agents[].timeout_seconds` (130) must be larger than the agent's `restart_timeout_seconds` (120) |
| `Error loading plugin MySQLEngine …` on attach | On HQbird this usually means the database **file does not exist**; check the path |
| Bare `DROP INDEX` in `fb_write` needs a window | It is Tier 2 there; use `fb_index_drop` (Tier 1; refuses constraint indexes) |
| New index not used | Run statistics (`fb_index_rebuild` action=statistics); `ORDER BY … DESC` needs a `DESCENDING` index |
| Tools fail for one database only | It is offline (`fb_db_health`); tools degrade per database |

## 15. State directory reference

| Path | Meaning |
|---|---|
| `state.json` (+ `.tmp`, `.corrupt-*`) | Pending requests, jobs, backup catalog, workflows, schedules. Corrupt ⇒ refuses to start |
| `audit.jsonl` (+ `.head`) | Hash-chained audit log; `.head` detects truncation |
| `events.jsonl` | Job events (`job.succeeded`, `job.failed` with the message) |
| `approvals\`, `denials\` | Out-of-band decisions (tray / `fbmcpctl approve`), used within ~2 s |
| `instance.lock` | One-kernel lock |
| `attach.addr`, `attach.token` | Attach endpoint for extra stdio clients |
| `fbmcp.log`, `fbmcp-crash.log` | Service log (rotated) and crash output |
| `query-log.jsonl` | Every `fb_query` call |
| `trends\` | Sampler history for `fb_trends` |
| `config-journal\` | `fb_config_set` history (old and new value) |
| `posture.verified` | Marker that allows service control |
| `tray.log` | Tray errors |

## 16. Walkthrough status

Pending (M1): a walkthrough on a fresh Windows host using only this guide.
Findings go back into this file. Lab results for remote mode (Linux host,
fresh VM, Windows service): [findings/phase12-lab-2026-11-03.md](findings/phase12-lab-2026-11-03.md).
