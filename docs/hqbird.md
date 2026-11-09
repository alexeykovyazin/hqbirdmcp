# fbmcp in HQbird 2027

HQbird 2027 setups (Windows `.exe`, Linux `.run`) install fbmcp by default.
One fbmcp serves every Firebird line and every fbagent of the server. The
setup and fbagent only put the binary in place and call one command:
`fbmcp ctl hqbird-install`. That command owns `fbmcp.yaml`, the secrets,
the service, the posture marker, TLS and the firewall rule.

Plan: goafts `docs/fbmcp-install-plan.ru.md`. Versions: ADR-038.

## Layout

| | Windows | Linux |
|---|---|---|
| Binary | `<HQbird>\fbmcp\fbmcp.exe` (`<HQbird>` = folder above the agent folders) | `/opt/hqbird/fbmcp/fbmcp`, links `/usr/local/bin/fbmcp`, `/usr/local/bin/fbmcpctl` |
| Data folder (`--data-dir`) | `C:\HQbirdData\fbmcp` (`<data root>\fbmcp` with setup `--data-root`) | none: fixed paths below |
| Config | `<data>\fbmcp.yaml` | `/etc/fbmcp/fbmcp.yaml` |
| State | `<data>\state` | `/var/lib/fbmcp` |
| Secrets | `<data>\secrets` | `/etc/fbmcp/secrets` |
| TLS | `<data>\tls` | `/etc/fbmcp/tls` |
| Managed list | `<state>\hqbird-managed.json` | `/var/lib/fbmcp/hqbird-managed.json` |
| Client notes | `<data>\connect.txt` | `/etc/fbmcp/connect.txt` |
| Service | `fbmcp`, `NT SERVICE\fbmcp`, auto start, restart on failure | `fbmcp.service`, `User=firebird` |

The config never lies next to the binary: an old fbmcp without the
`version` command would start the kernel on `fbmcp version`.

## `fbmcp version`

Prints one JSON line and exits 0. It never reads a config and never starts
the kernel. fbagent reads it (`productupdate` `VersionFromCLI`, 2 s).

```
{"version":"2027.2.21"}
```

A build without `-ldflags "-X main.version=…"` prints `dev`.

## `fbmcp ctl hqbird-install`

One idempotent command. Run it as Administrator (Windows) or root (Linux)
after any line is installed or removed. It is safe to run again at any
time.

```
fbmcp ctl hqbird-install [--data-dir DIR] [--config FILE] [--state-dir DIR]
    [--secrets-dir DIR] [--agent-config FILE]... [--lines-stdin]
    [--allow-from CIDR]... [--port N] [--dns NAME]... [--ip ADDR]
    [--no-service] [--dry-run] [--json]
```

| Option | Meaning |
|---|---|
| `--data-dir` | Folder for config, state, secrets, TLS and `connect.txt` (layout above) |
| `--config`, `--state-dir`, `--secrets-dir` | Override one path |
| `--agent-config` | Also use this line (its service may not be registered yet). Repeatable |
| `--lines-stdin` | Read line facts and secrets from stdin as JSON (below). Secrets never go on the command line |
| `--allow-from` | Turn on remote mode for these admin addresses (IP or CIDR; repeat or comma list). Adds to the addresses already allowed |
| `--port` | Remote port, default `8650` |
| `--dns`, `--ip` | Extra names and the address for the certificate and `listen` |
| `--no-service` | Do not touch the OS service, the firewall or the posture marker (tests, containers) |
| `--dry-run` | Print what would change; change nothing |
| `--json` | Print the result as one JSON object on stdout. Progress goes to stderr |

Exit codes: `0` done (there can be warnings), `1` failed, `2` usage error.

### What it does

1. **Finds the lines.** Services `HQbirdFBAgent-*` (Windows) and units
   `fbagent*` (Linux), as `fb_agent_discover` does, plus `--agent-config`
   and the stdin lines. From each agent folder it reads
   `agent_config.json` (Firebird port, install path, Local API) and
   `hqbird2027-firebird.json` (Firebird service).
2. **Reconciles `fbmcp.yaml`.** One instance and one agent per line. It
   adds new lines, updates changed ones, removes lines that are gone. It
   only touches entries listed in `hqbird-managed.json`; entries the
   operator added stay. Ids:
   - instance id: the line name in lower case with `-` → `_` (`fb50`,
     `fb50_2`, `p3050`); a name that is still not a valid id gets
     `p<port>`;
   - agent id: `agent_p<firebird port>`; token secret:
     `FBMCP_AGENT_TOKEN_P<port>`; SYSDBA secret: `FBMCP_FB_P<port>_SYSDBA`.
   Each instance gets `service` (the Firebird service of the line). With
   a SYSDBA password, SYSDBA is also the default read and admin user
   (reads always run in a read-only transaction; `doctor` warns), and the
   default `backup_dir` / `work_dir` are `<backups>/<instance>` and
   `<work>/<instance>` (Linux `/var/lib/fbmcp-backups`,
   `/var/lib/fbmcp-work`; Windows `<data-dir>\backups`, `<data-dir>\work`).
   **Databases** of each line are registered: the aliases of
   `databases.conf` (not `security.db`, `$(macro)` or remote paths) and
   the databases the agent saw attached lately (newest
   `<storage.data_path>/server_level/GetActiveDatabases/getactdb_*.jsonl`).
   Only files that exist and no entry holds yet; the id is the file name
   (`sales`, else `sales_<instance>`). They are listed in
   `hqbird-managed.json` and go with their line. A line with no SYSDBA
   password gets no databases (a note says so).
3. **Secrets** go to the secrets folder: the Local API token of each line
   (from stdin, else from `agent_config.json` when it is plain text) and
   the SYSDBA password (stdin only).
4. **Service.** Windows: service `fbmcp` as `NT SERVICE\fbmcp`, folder
   ACLs, start/stop rights on the Firebird service of every line, read on
   each `agent_config.json`. Linux: unit, sudoers for the Firebird units.
   An existing service is updated, not recreated.
5. **Posture.** Runs the posture check and writes `posture.verified`
   (without it `fb_service_start`, `fb_service_stop` and
   `fb_service_restart` refuse).
6. **Remote mode** only with `--allow-from` (or when it is already on):
   `listen`, self-signed certificate and pin, `allowed_origins`,
   `server_name`, firewall rule `fbmcp-remote` for the allowed addresses
   only, `doctor --remote`. The allowed addresses are checked only by the
   firewall, so remote mode needs a working one: when the rule cannot be
   added or does not apply (ufw inactive, no firewalld or ufw, a Windows
   Firewall profile off), remote mode stays off with a warning; turn the
   firewall on and run `hqbird-install` again. A FAIL of `doctor --remote`
   turns remote mode off again and is a warning. API keys are never made here: `fbmcp ctl apikey new <name>
   --max-tier 2 --config <fbmcp.yaml>` stores the key in `secrets.dir` and
   adds the identity; the next `hqbird-install` turns remote mode on.
7. **Start and check.** Starts the service, waits for `fbmcp ctl ping`,
   writes `connect.txt` (URL, pin, client entries; no key values).
8. **No lines left.** Stops and disables the service, removes the firewall
   rule. Files, state and the audit log stay.

### `--lines-stdin`

```json
{
  "lines": [
    {
      "agent_config": "C:\\Program Files\\HQbird2027\\fbagent-fb50\\agent_config.json",
      "line": "fb50",
      "firebird_port": 3050,
      "firebird_service": "FirebirdServerfb50",
      "firebird_bin": "C:\\Program Files\\HQbird2027\\Firebird50",
      "firebird_version": "5.0",
      "local_api_url": "http://127.0.0.1:13050",
      "local_api_token": "…",
      "instance_id": "…",
      "sysdba_user": "SYSDBA",
      "sysdba_password": "…"
    }
  ]
}
```

Only `agent_config` is required. A field given here wins over the files.

### `--json` result

```json
{
  "version": "2027.2.21",
  "config": "C:\\HQbirdData\\fbmcp\\fbmcp.yaml",
  "state_dir": "C:\\HQbirdData\\fbmcp\\state",
  "service": "fbmcp",
  "service_state": "running",
  "lines": [
    {"line": "fb50", "instance": "fb50", "agent": "agent_p3050", "firebird_port": 3050}
  ],
  "remote": {
    "enabled": true,
    "url": "https://srv01.example.com:8650/mcp",
    "pin": "sha256:…",
    "allow_from": ["10.0.0.0/24"]
  },
  "connect_file": "C:\\HQbirdData\\fbmcp\\connect.txt",
  "warnings": []
}
```

`service_state`: `running`, `stopped` (no lines), `absent` (`--no-service`)
or `failed`.

## Updates

fbagent updates fbmcp like any provisioned product: stop the service,
keep the old binary, put the new one in place, start, check
`fbmcp ctl ping` and `fbmcp version`, roll back on failure. The update does
not change `fbmcp.yaml`, TLS, keys or `listen`. Any agent of the server
can do it; the host lock makes the second agent see the new version and
stop.

On Linux the binary is root's. The agent gives the downloaded build, the
version and its zone signature to the root helper. Root checks the zone
signature again on its own copy of the bytes (fbagent
`docs/LINUX_ROOT_TRUST.md`, "fbmcp update") before it puts the build in
place. A release without a zone signature is not installed on Linux.

## Approving Tier 2

A client cannot approve its own Tier-2 request. The approver runs:

- Windows, elevated window: `fbmcp ctl approve <id>`
- Linux: `sudo -u firebird fbmcp ctl approve <id>`

Without `<id>` it lists the pending requests. It finds the HQbird config
(`C:\HQbirdData\fbmcp\fbmcp.yaml`, `/etc/fbmcp/fbmcp.yaml`) by itself, asks
y/N before it writes (`--yes` does not ask), and
`--deny <id> --reason "..."` refuses a request.
