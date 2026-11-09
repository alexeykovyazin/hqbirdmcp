# fbmcp remote mode (HTTPS + API keys)

fbmcp runs on the database server, next to Firebird and fbagent. Admins and
agents connect over HTTPS from their own machines. Each person or agent has
its own API key. Tier 2 actions are still approved on the server (tray or
`fbmcpctl approve`), never by the client.

```
admin PC: Claude Desktop ──stdio──► fbmcp bridge ──HTTPS + Bearer──► srv1:8443 fbmcp ──► Firebird, fbagent
admin PC: Claude Code ─────────────── HTTPS + Bearer (direct) ──────►
```

Design: [ADR-022](decisions/ADR-022-remote-tls.md) (phase 12 addendum).
Plan and lab results: `../phase12_plan.md`.

On an HQbird 2027 server fbmcp is already installed: turn remote mode on
with `fbmcp ctl hqbird-install --allow-from <CIDR>` and one API key
([`hqbird.md`](hqbird.md)). Sections 1–2 below are for other servers.

## 0. Before you start

- On the server: Firebird and fbagent with the Local API on (fbagent ≥ 2.67).
- A free TCP port (examples: 8443).
- The admin addresses (CIDR) that may connect. The firewall rule allows only these.
- A certificate: corporate CA, or self-signed + pin (`fbmcpctl tls selfsign`).
- The service account: Windows `NT SERVICE\fbmcp` (default) or a local user;
  Linux the Firebird user (`firebird`), so fbmcp can read fbagent's
  `agent_config.json` (mode 0600 firebird).

## 1. Server — Windows (elevated PowerShell)

1. Copy `fbmcp.exe`, `fbmcpctl.exe`, `fbmcp-tray.exe` to `C:\fbmcp\`.
2. Write `C:\fbmcp\fbmcp.yaml` (template in §3). State dir: `C:\ProgramData\fbmcp\state`.
3. Certificate. Write down the printed pin `sha256:...`:
   ```
   C:\fbmcp\fbmcpctl.exe tls selfsign --out C:\fbmcp\tls --dns srv1,srv1.corp.local --ip 10.0.0.5 --grant "NT SERVICE\fbmcp"
   ```
   The grant works before the service exists (the virtual-account SID is computed).
4. One API key per person or agent. The value is printed once; give it to
   the person over a safe channel. `--config` adds the `identities:` entry
   and stores the key in `secrets.dir`:
   ```
   C:\fbmcp\fbmcpctl.exe apikey new alice --max-tier 2 --config C:\fbmcp\fbmcp.yaml
   ```
5. Agent token and DB passwords. Paste the value, then Ctrl+Z, Enter (it is read from stdin):
   ```
   C:\fbmcp\fbmcpctl.exe secret set --dir C:\ProgramData\fbmcp\secrets FBMCP_AGENT_TOKEN_P3055
   ```
6. Install. Dry run first, then the same command without `-WhatIf`:
   ```
   .\packaging\windows\install-remote.ps1 -Port 8443 -AllowFrom 10.0.0.0/24 -FirebirdService FirebirdServerDefaultInstance -WhatIf
   ```
   Options: `-Account .\fbmcp-svc -Credential (Get-Credential)` for a local
   user; `-GrantAgentConfig C:\HQbird\fbagent-p3055\agent_config.json` for the
   phase-11 agent config tools; `-OperatorGroup DOMAIN\user` for the person
   who approves in the tray (the tray runs without elevation, so membership
   in Administrators does not help under UAC).
   The script lets only SYSTEM and Administrators write to `C:\fbmcp`: a new
   folder under `C:\` lets every user change files, and the service runs
   them. Keep fbmcp in a folder of its own.
7. Check: `C:\fbmcp\fbmcpctl.exe doctor --remote --config C:\fbmcp\fbmcp.yaml` → no FAIL lines;
   `curl.exe --cacert C:\fbmcp\tls\tls.crt https://srv1:8443/healthz` → `ok`.
   The kernel log is `C:\ProgramData\fbmcp\state\fbmcp.log`.
8. Approvals: as the operator who approves, run `.\packaging\windows\install-tray.ps1`.

## 2. Server — Linux (root shell)

1. Copy the release folder to the server (`fbmcp`, `fbmcpctl`,
   `packaging/linux/`), for example to `/root/fbmcp-rel/`, and `cd` there.
   Then: `install -m 0755 fbmcp fbmcpctl /usr/local/bin/`
2. Install. Dry run first, then without `--dry-run`:
   ```
   sh packaging/linux/install-remote.sh --user firebird --port 8443 --allow-from 10.0.0.0/24 --dry-run
   ```
   The script makes the folders (`/etc/fbmcp`, `/etc/fbmcp/secrets`,
   `/var/lib/fbmcp`, `/var/lib/fbmcp-backups`, `/var/lib/fbmcp-work`), the
   certificate, a first config with an `admin` key (only if
   `/etc/fbmcp/fbmcp.yaml` does not exist), the unit, the sudoers rule for the
   Firebird unit, the ufw rule, starts the service and runs `doctor --remote`.
   - The `admin` key is printed **once**. Copy it to the admin workstation (§4).
   - The certificate gets the host name and the first `hostname -I` address.
     If clients use another name or address (DNS alias, NAT, port forward),
     add them: `--dns srv1,srv1.corp.local --ip 10.0.0.5,192.0.2.10`.
     The first `--ip` address is also the listen address.
   - If ufw is inactive, the script warns: the rule applies only after
     `ufw enable` (allow ssh first). Behind NAT (a VM, a container), the
     host firewall must also forward the port.
3. Agent token and DB passwords (value from stdin, Ctrl+D):
   ```
   fbmcpctl secret set --dir /etc/fbmcp/secrets FBMCP_AGENT_TOKEN_NUC3
   ```
4. Edit the config (§3). The script wrote instance `fb1` from the Firebird
   unit (`bin_dir` from its `ExecStart`, port from `firebird.conf`): check it,
   and uncomment `service_user` / `service_secret_env`. Add `databases:` and
   `agents:`. The kernel reloads the config by itself. `fb_agent_discover`
   lists the local fbagent.
5. For `fb_service_start/stop/restart`: `sudo -u firebird fbmcpctl setup --write-posture`.
6. Check: `fbmcpctl doctor --remote`; `journalctl -u fbmcp -n 50`.
   (`fbmcpctl` finds `/etc/fbmcp/fbmcp.yaml` by itself; for another file add
   `--config <file>`.)
7. Approvals (no tray on Linux): `sudo -u firebird fbmcpctl approve`
   lists pending requests; add the request id to approve one. From a
   workstation: `ssh -t srv1 sudo -u firebird fbmcpctl approve <id>`
   (`approve` asks y/N; without a terminal add `--yes`). Deny: `... approve --deny <id> --reason "..."`.
   A request expires after 15 minutes; then `approve` says
   `no such pending action` and exits 1. Ask the client to repeat the call.

Manual install (what the script does): unit `packaging/linux/fbmcp.service`
(`-serve`, `User=firebird`, optional `ReadWritePaths` with `-`, no
`NoNewPrivileges` because of sudo); sudoers `packaging/posture/sudoers.fbmcp`
(`visudo -c`); `ufw allow from 10.0.0.0/24 to any port 8443 proto tcp`.

## 3. Config template

```yaml
server_name: srv1                       # fb_ping, fb_db_list, webhooks (default: host name)
state:
  dir: C:\ProgramData\fbmcp\state       # Linux: /var/lib/fbmcp
secrets:
  dir: C:\ProgramData\fbmcp\secrets     # Linux: /etc/fbmcp/secrets
listen: 10.0.0.5:8443                   # a real interface address, never 127.0.0.1
tls:
  cert: C:\fbmcp\tls\tls.crt            # Linux: /etc/fbmcp/tls/tls.crt
  key: C:\fbmcp\tls\tls.key
identities:
  - name: alice
    key_env: FBMCP_KEY_ALICE
    max_tier: 2                         # default 1
  - name: openclaw-1                    # §6
    key_env: FBMCP_KEY_OPENCLAW_1
    profile: autonomous
    allow_from: [10.0.0.21/32]
allowed_origins:
  - https://fbmcp.invalid               # required; no browser client uses it
notify:
  webhook_url: https://chat.example/hooks/fbmcp
  webhook_secret_env: FBMCP_WEBHOOK_SECRET
agents:
  - id: p3055
    base_url: http://127.0.0.1:13055    # Linux default agent: http://127.0.0.1:13050
    token_env: FBMCP_AGENT_TOKEN_P3055
    instance_id: honor2025261022-p3055
instances:
  - id: fb5
    addr: localhost:3055
    bin_dir: C:\HQbird\Firebird50       # Linux: /opt/firebird/bin (the folder with gbak)
    service: FirebirdServerDefaultInstance   # Linux: firebird.opt_fb50
    service_user: SYSDBA                # instance tools (fb_firebird_log, ...)
    service_secret_env: FBMCP_FB5_SERVICE_PW
# databases: as in packaging/fbmcp.yaml.example
```

Secret lookup order: environment variable, then the file `<secrets.dir>/<NAME>`,
then the OS keyring of the service account. Use the secrets dir on servers:
a service account has no usable keyring, and a file change applies in a
few seconds without a restart.

## 4. Admin workstation

1. Copy `fbmcp.exe` and `fbmcpctl.exe` to `C:\fbmcp\`.
2. Store the key you got from the server admin (paste it, then Ctrl+Z, Enter):
   `C:\fbmcp\fbmcpctl.exe secret set FBMCP_KEY_SRV1`
3. Connect the client. Pick one:
   - **Bridge** (any stdio client: Claude Desktop, Cursor, Codex, ...). In
     `claude_desktop_config.json`:
     ```json
     {"mcpServers": {"fb-srv1": {"command": "C:\\fbmcp\\fbmcp.exe",
       "args": ["bridge", "--url", "https://srv1:8443/mcp", "--key-env", "FBMCP_KEY_SRV1", "--pin", "sha256:<hex>"]}}}
     ```
     `--pin` checks the server key, so a self-signed cert needs no CA setup.
     Without `--pin`: `--ca srv1.crt`, or the system CA store.
   - **Direct HTTP** (Claude Code and other HTTP clients). Corporate CA, or
     `NODE_EXTRA_CA_CERTS=C:\fbmcp\srv1.crt` for a self-signed cert. In `.mcp.json`:
     `{"mcpServers":{"fb-srv1":{"type":"http","url":"https://srv1:8443/mcp","headers":{"Authorization":"Bearer ${FBMCP_KEY_SRV1}"}}}}`.
     Here the key must be a user env var. Do not use
     `claude mcp add --header "Authorization: Bearer <key>"`: it stores the key
     in plain text in `~/.claude.json`.
4. Test: ask the client to run `fb_ping` (shows `server_name` and your
   identity), then `fb_db_list`.
5. Another server: one more entry with its own URL, key name and pin.

The bridge relays every message unchanged. It keeps the session id, opens
the server notification stream, sends `DELETE` when the client exits, and
starts a new session by itself once when the server restarted.

## 5. Daily operations

- **Approve Tier 2:** the webhook says a request waits (it expires in 15 min).
  Windows: tray. Both OS: `fbmcpctl approve <id>` on the server (`--deny <id> --reason ...` to refuse).
- **Rotate a key:** `fbmcpctl apikey rotate alice --config <fbmcp.yaml>` on the
  server. With the secrets dir it applies in seconds; the old key stops
  working then. Give the new value to Alice. It refuses when an env var with
  that name is set (env wins, so the rotation would do nothing).
- **List keys:** `fbmcpctl apikey list --config <fbmcp.yaml>`: names, sources, tiers. Never values.
- **Remove a person or agent:** delete the `identities:` entry (it reloads by
  itself), then delete the secret file (`fbmcpctl secret drop --dir <dir> NAME`).
- **Renew the cert:** `fbmcpctl tls selfsign --renew --out <dir> --dns ... --ip ...`.
  It keeps the key, so the pin does not change and `fbmcp bridge --pin`
  clients keep working. The kernel serves the new cert without a restart.
  Clients that trust the cert file itself (`NODE_EXTRA_CA_CERTS`, a CA file)
  need the new `tls.crt`: copy it to them again.
- **Upgrade fbmcp:** stop the service, replace the binaries, start, run `doctor --remote`.
- **Logs:** Windows `<state_dir>\fbmcp.log` (rotated, 5 × 10 MiB; crashes in
  `fbmcp-crash.log`); Linux `journalctl -u fbmcp`. Audit: `<state_dir>/audit.jsonl`
  (check with `fbmcpctl verify --config <fbmcp.yaml>`). Remote calls carry `remote_addr` and `profile`;
  `session.open` and `auth.denied` events are audited too.

## 6. Autonomous agents (OpenClaw, Hermes and similar)

An autonomous agent runs without a human and often reads messengers or
mail. A foreign message can carry a prompt injection to the DB tools. The
agent can also call `fb_confirm` itself, so in-band confirmation does not
protect anything for it.

Rules:
- Give each agent its own key with `profile: autonomous`:
  `fbmcpctl apikey new openclaw-1 --profile autonomous --allow-from 10.0.0.21/32 --secrets-dir <dir>`.
- The profile means: `max_tier` 0 (read-only) unless set; `inband_confirm`
  is always false, so every Tier 1 request waits for an operator (tray /
  `fbmcpctl approve`) like Tier 2, and `fb_confirm` from the agent is refused;
  limits 10 calls/min, burst 20, 2 concurrent calls, 4 live sessions.
- `allow_from` accepts the key only from these source addresses (TCP peer;
  X-Forwarded-For is ignored). Any other address gets 403 and an
  `auth.denied` audit entry.
- Run the agent inside the company network and allow only its address. Do
  not expose fbmcp to a cloud-hosted agent gateway.
- Read the audit: `profile: autonomous` entries, `auth.denied`, and pending
  requests the agent made. `doctor --remote` warns about an autonomous key
  with `max_tier ≥ 1` or without `allow_from`.

## 7. Limits and sessions

Per identity (global `limits:` block, or `limits:` on one identity):
`max_sessions` concurrent calls (8; a held notification stream does not
count), `max_streams` held streams (2), `rate_per_minute` / `rate_burst`
(30 / 60), `max_live_sessions` open MCP sessions (16). Over a limit: 429
with `Retry-After`. `session_idle_minutes` (global, 30): an idle session is
closed; the bridge then starts a new one by itself.

## 8. Config writes

On Linux the unit has `ProtectSystem=strict` and `/etc/fbmcp/fbmcp.yaml` is
owned by root. So `fb_db_register`, `fb_agent_register` and `fb_config_set`
cannot save files there. They refuse with a clear message. Make the change
by hand: the kernel reloads `fbmcp.yaml` by itself. To allow the tools
anyway: `chown firebird /etc/fbmcp/fbmcp.yaml` and the folder, and add
`ReadWritePaths=-/etc/fbmcp` (plus the Firebird config folder for
`fb_config_set`) to the unit. This lets the service change its own
config — decide it on purpose. `fb_config_get` / `fb_config_set` read
`firebird.conf` from `bin_dir`, or from the folder above it when `bin_dir`
is a `bin` folder (Linux: `/opt/firebird/firebird.conf`).

On Windows the service account writes only where the ACL lets it. Check
the Firebird folder: one under `C:\` (HQbird: `C:\HQbird\Firebird30`)
often inherits Modify for Authenticated Users. Then `fb_config_set` can
write `firebird.conf`, but so can any local user, and the engine runs as
LocalSystem: remove the inheritance and give Users read only.
`fb_config_set` changes only its own line (comments and line ends stay)
and keeps the old file as `firebird.conf.prev`.

## 9. Troubleshooting

| Symptom | Cause and action |
|---|---|
| 401 | Wrong key, or the server did not load it. `apikey list` / `doctor --remote` show each key's source |
| 403 | Source IP outside `allow_from`, or an Origin header not in `allowed_origins` |
| 429 | Limits. Wait for `Retry-After` (2 s for calls, 60 s for new sessions) |
| TLS error in Node | Set `NODE_EXTRA_CA_CERTS`, or use the bridge with `--pin` |
| Pin mismatch | The server key changed (`--force` makes a new key). Get the new pin from the server admin |
| Connection refused / timeout | Service down or firewall. Run `doctor --remote` on the server |
| Service starts, then stops | Start-up error (bad cert, busy port, missing key): see the log / journal |
| `fb_confirm` refused: out-of-band only | The identity is `profile: autonomous` or `inband_confirm: false`. Approve on the server |

## 10. Remove

- Windows: `sc.exe stop fbmcp`; `sc.exe delete fbmcp`; `Remove-NetFirewallRule -DisplayName fbmcp-remote`.
- Linux: `systemctl disable --now fbmcp`; remove `/etc/systemd/system/fbmcp.service`
  and `/etc/sudoers.d/fbmcp`; `ufw delete allow from 10.0.0.0/24 to any port 8443 proto tcp`.
- Keep the state dir (audit evidence) unless the owner says to remove it.

## Release notes (phase 12)

- An `identities:` entry without `max_tier` now gets **1** (was 2). Set
  `max_tier: 2` explicitly where Tier 2 is needed. `max_tier: 0` now means
  read-only (it was mapped to 2 before).
- `fb_ping` answers `pong`, `server: <name>`, `identity: <name> (max tier N)`;
  `fb_db_list` starts with `server: <name>`.
- A remote-mode start-up error (cert, port, key) now stops the process with
  a non-zero exit instead of running without a listener.
- New: `fbmcp bridge`, `fbmcpctl tls`, `fbmcpctl apikey`, `fbmcpctl doctor --remote`,
  `fbmcpctl secret --dir`, `secrets.dir`, `server_name`, identity `profile`,
  `inband_confirm`, `allow_from`, per-identity `limits`, Linux `fb_agent_discover`.
