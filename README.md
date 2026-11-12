# fbmcp — Firebird DBA MCP Server

fbmcp is an MCP (Model Context Protocol) server that lets an LLM client —
Claude Desktop, Claude Code, ZCode, any MCP host — operate Firebird
databases as a DBA: monitoring, backups and verified restores, SQL/DDL/DCL,
security, indexes, migrations and a scheduler, across Firebird 2.5–5.0.
Every mutating call passes a tiered policy engine and a **human confirmation
gate** before it touches the database, and every decision is written to a
hash-chained audit log. 109 tools in total (54 read-only, 38 Tier 1, 16
Tier 2, 1 disabled-by-design Tier 3), including the fbagent bridge
([docs/agent-bridge.md](docs/agent-bridge.md)), the HQbird cluster tools
([docs/tool-reference.md](docs/tool-reference.md#hqbird-cluster-rcm)) and a result grid in the
chat ([docs/grid.md](docs/grid.md)). A client sees only the tools its
identity may use. All tools: [docs/tool-reference.md](docs/tool-reference.md).

## Start here: choose your path

| Your setup | Guide | First command |
|---|---|---|
| HQbird server | [docs/hqbird.md](docs/hqbird.md) | `fbmcp ctl hqbird-install` |
| Windows PC, MCP client on the same PC | [English](docs/quickstart-en.md) · [Русский](docs/quickstart-ru.md) · [Português (Brasil)](docs/quickstart-pt.md) | `winget install GoLang.Go` |
| Linux host | [docs/runbook.md](docs/runbook.md) §4–§6 | `git clone https://github.com/alexeykovyazin/hqbirdmcp.git` |
| Firebird server, client on another PC | [docs/remote.md](docs/remote.md) | `fbmcp ctl doctor --remote` |

Every path ends with `fbmcp ctl doctor`: the last line must be
`doctor: green`. Each FAIL line ends with `Fix:` and the command to run.
`fbmcp ctl` and `fbmcpctl` are the same program (`fbmcpctl help`).

More: [docs/runbook.md](docs/runbook.md) (all setups, daily use,
recovery; in Portuguese (Brazil):
[docs/runbook-pt-br.md](docs/runbook-pt-br.md)), [docs/tool-reference.md](docs/tool-reference.md) (all tools),
[docs/status.md](docs/status.md) (project status and plans).

## Build from source

Requirements: Go ≥ 1.25.13, CGO not needed (pure-Go Firebird driver),
a reachable Firebird 2.5–5.0 server. Windows and Linux.

```bash
git clone https://github.com/alexeykovyazin/hqbirdmcp.git
cd hqbirdmcp
mkdir -p dist
CGO_ENABLED=0 go build -trimpath -o dist/fbmcp.exe ./cmd/fbmcp        # the MCP server (stdio)
CGO_ENABLED=0 go build -trimpath -o dist/fbmcpctl.exe ./cmd/fbmcpctl  # operator CLI
# Windows tray for Tier >= 2 approvals — GUI subsystem, no console window:
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -H=windowsgui" -o dist/fbmcp-tray.exe ./cmd/fbmcp-tray

# tests (live Firebird suites are opt-in via FBMCP_*_LIVE env vars)
go test ./...
```

On the dev host, `packaging/rebuild-install.ps1` does the whole loop in one
command: rebuild all four binaries into `dist\`, stop/update/restart the
running kernel and tray, merge the MCP client config, then `fbmcpctl doctor`.

## How it works

fbmcp runs as **one kernel process** per state directory (ADR-005): the
first client launches it over stdio, extra stdio clients and a remote
listener (TLS + API keys, ADR-022) attach to the same kernel. Configuration
lives in one `fbmcp.yaml`; edits are applied in-process (ADR-012) — no
client or process restarts. Databases are referenced by registry id
(`db`, `instance`), never by path in a client conversation; secrets come
from environment variables with an OS-keyring fallback.

The safety model (§5.5 / ADR-006/007/019/021/029/030): every tool has a
tier — **0** reads (engine-enforced read-only transactions, no
confirmation), **1** mutations (single human confirmation, in-band token),
**2** high impact (out-of-band confirmation only, maintenance window +
verified backup < 24 h preconditions), **3** critical (disabled by design).
A gated call never executes directly: it returns an impact statement plus a
pending action that a human confirms through a channel the LLM cannot
reach for Tier ≥ 2. The confirmed body runs as a job on the admin pool,
with progress via `fb_job_status`; previews (`mode: preview`) are
informational only. Every decision — allow / pending / approved / denied —
is appended to a hash-chained `audit.jsonl`.

Host-side surfaces (not MCP tools): `fbmcp ctl` / `fbmcpctl` (approve,
status, setup, doctor, verify, repair, secret, apikey, tls, ping; see
below) and the Windows `fbmcp-tray`
Approve/Deny popup — these exist because Tier ≥ 2 confirmation must be
out of the model's reach. Scheduled jobs run from durable pre-authorized
grants (confirmed once at create time); Tier 3 and restore-replace can
never be scheduled.

Everything under `spikes/` is throwaway/reference code — it intentionally
cuts corners (hardcoded ports, `masterkey` passwords) that production code
must not.

### Environment (this dev host)

- Go 1.25.13, Windows amd64
- Local Firebird instances (HQbird installs, all running as services):
  - FB 2.5 — port 3052 (`C:\HQbird\Firebird25`)
  - FB 3.0 — port 3053 (`C:\HQbird\Firebird30`)
  - FB 4.0 — port 3054 (`C:\HQbird\Firebird40`)
  - FB 5.0 — port 3055 (`C:\HQbird\Firebird50`)
- Linux side: Docker Desktop present but not running; FB-version coverage on
  Linux is deferred until containers are brought up (plan E2 tracks this).

Spikes run only against dedicated spike databases created under
`C:\HQbirdData\output\fbmcp-spike\` — never against real databases.

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

**`fbmcp ctl`** = **`fbmcpctl`** (cross-platform). `fbmcpctl help` lists
the commands, `fbmcpctl <command> -h` shows one:

```
fbmcpctl approve [request_id] [--yes]                 # list, or approve one (asks y/N)
fbmcpctl approve --deny <request_id> [--reason TEXT] [--yes]
fbmcpctl status | ping [timeout] | verify
fbmcpctl doctor [--remote]                            # every FAIL line ends with "Fix:"
fbmcpctl setup [--write-posture]
fbmcpctl repair --from-audit
fbmcpctl secret set|get|drop [--dir <secrets.dir>] NAME
fbmcpctl apikey new NAME [--max-tier N] [--profile autonomous] [--allow-from CIDR,...] [--config fbmcp.yaml] [--no-show]
fbmcpctl apikey rotate NAME [--config fbmcp.yaml] [--no-show]
fbmcpctl apikey list
fbmcpctl tls selfsign --out DIR --dns a,b --ip x,y [--grant ACCOUNT] [--days 825] [--renew|--force]
fbmcpctl tls show <cert.pem>
fbmcpctl hqbird-install
```

**Which config.** Every command that reads `fbmcp.yaml` takes
`--config <file>`. Without it: `$FBMCP_CONFIG`, then `fbmcp.yaml` in the
current folder, then `fbmcp.yaml` next to the program, then the HQbird
default (`C:\HQbirdData\fbmcp\fbmcp.yaml`, `/etc/fbmcp/fbmcp.yaml`). The
command prints `using config <path>`. The old forms (`approve <state_dir>`,
a config path as the last argument) still work.

**`fbmcp bridge`** (admin workstation): a stdio MCP server for Claude
Desktop and other stdio clients that relays every message to a remote
fbmcp over HTTPS with an API key; `--pin sha256:<hex>` trusts a
self-signed server key without CA setup.

```
fbmcp bridge --url https://srv1:8443/mcp --key-env FBMCP_KEY_SRV1 [--pin sha256:<hex> | --ca srv1.crt] [--secrets-dir DIR]
```

Remote mode install, keys, autonomous agents, troubleshooting:
[`docs/remote.md`](docs/remote.md).


Playbooks (how to combine tools): [`prompts/`](prompts/).
