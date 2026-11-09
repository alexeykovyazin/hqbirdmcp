# Connect fbmcp to Claude (Desktop and Code)

fbmcp speaks **MCP over stdio** (the default). Claude Desktop launches it as a child process. Do **not** set `listen` in the YAML for this setup — remote HTTP is a different mode and refuses to start without TLS + an API key.

Use **absolute paths** everywhere. Claude does not inherit your shell working directory or PATH the way a terminal does.

## 1. Binaries

Built with `CGO_ENABLED=0 -trimpath` into `fbmcp/dist/`:

| File | Role |
|---|---|
| `fbmcp.exe` | MCP server (stdio) |
| `fbmcpctl.exe` | Operator CLI: `approve`, `status`, `setup`, `doctor` |
| `fbmcp-tray.exe` | Windows tray Approve/Deny popup for Tier ≥ 2 confirmations |

This host (Windows amd64):

- Server: `E:\Projects_2026\AIDBA\fbmcp\dist\fbmcp.exe`
- CLI: `E:\Projects_2026\AIDBA\fbmcp\dist\fbmcpctl.exe`

Rebuild (or do everything — build, stop/backup/replace in `dist\`, state dir, client config merge — in one command):

```powershell
cd E:\Projects_2026\AIDBA\fbmcp
$env:CGO_ENABLED = "0"
go build -trimpath -ldflags "-s -w -X main.version=dev" -o dist/fbmcp.exe ./cmd/fbmcp
go build -trimpath -ldflags "-s -w -X main.version=dev" -o dist/fbmcpctl.exe ./cmd/fbmcpctl
```

One-command rebuild + update + configure (see `packaging/rebuild-install.ps1`; dry-run with `-WhatIf`):

```powershell
.\packaging\rebuild-install.ps1 -WhatIf          # dry run
.\packaging\rebuild-install.ps1 -Force           # stop running dist\ binaries, rebuild, update, configure ZCode
.\packaging\rebuild-install.ps1 -Clients zcode,claude -Force
```

## 2. Config and secrets

Spike/dev YAML (this machine): [`fbmcp.dev.yaml`](../fbmcp.dev.yaml). It points at the HQbird spike databases under `C:\HQbirdData\output\fbmcp-spike\` and reads the password from **`FBMCP_DEV_PW`**.

That variable must be set in the MCP client `env` block (not in the YAML) — set it to the spike databases' SYSDBA password (the local dev value on this machine; it is deliberately not committed here — C10 keeps secrets out of the repo). Alternative: `fbmcpctl secret set FBMCP_DEV_PW` stores it in the OS keyring (entry `fbmcp/FBMCP_DEV_PW`), and then the `env` block is unnecessary. Do not use this against production databases.

Create the state directory once:

```powershell
New-Item -ItemType Directory -Force -Path C:\HQbirdData\output\fbmcp-spike\state
```

Optional check (from `fbmcp/`):

```powershell
$env:FBMCP_DEV_PW = "<spike SYSDBA password>"
.\dist\fbmcpctl.exe doctor --config .\fbmcp.dev.yaml
```

## 3. Claude Desktop

Config file (Windows):

`%APPDATA%\Claude\claude_desktop_config.json`

Open it from Claude: **Settings → Developer → Edit Config**. If the file is missing, that action creates it.

Merge this entry into `mcpServers` (keep any other servers you already have):

```json
{
  "mcpServers": {
    "fbmcp": {
      "command": "E:\\Projects_2026\\AIDBA\\fbmcp\\dist\\fbmcp.exe",
      "args": [
        "-config",
        "E:\\Projects_2026\\AIDBA\\fbmcp\\fbmcp.dev.yaml"
      ],
      "env": {
        "FBMCP_DEV_PW": "<spike SYSDBA password>"
      }
    }
  }
}
```

A copy lives at [`packaging/claude_desktop_config.example.json`](../packaging/claude_desktop_config.example.json).

**Fully quit Claude Desktop** (tray icon too) and start it again. Servers load only at startup.

In a new chat you should see **fbmcp** tools (hammer / tools menu). Full catalog: [`tool-reference.md`](tool-reference.md). Smoke prompts:

- “Call `fb_ping`.”
- “List databases with `fb_db_list`.”
- “Health-check `spike5` with `fb_db_health`.”

## 4. Claude Code

Same stdio binary. Project file `.mcp.json` (repo root or `fbmcp/`):

```json
{
  "mcpServers": {
    "fbmcp": {
      "command": "E:\\Projects_2026\\AIDBA\\fbmcp\\dist\\fbmcp.exe",
      "args": ["-config", "E:\\Projects_2026\\AIDBA\\fbmcp\\fbmcp.dev.yaml"],
      "env": { "FBMCP_DEV_PW": "<spike SYSDBA password>" }
    }
  }
}
```

Or CLI:

```powershell
claude mcp add --transport stdio fbmcp -- "E:\Projects_2026\AIDBA\fbmcp\dist\fbmcp.exe" -config "E:\Projects_2026\AIDBA\fbmcp\fbmcp.dev.yaml"
```

Then set `FBMCP_DEV_PW` in that server’s env (Claude Code settings or the JSON above).

## 5. Confirmations (safety gate)

| Tier | How the human confirms |
|---|---|
| 0 (reads) | No confirm |
| 1 | In-band: Claude shows a request id + token; you confirm with `fb_confirm` |
| ≥ 2 | **Out-of-band only.** Claude cannot confirm. On the host: `fbmcpctl approve <request_id>` (or the tray) |

State dir for the spike config: `C:\HQbirdData\output\fbmcp-spike\state`.

`fb_demo_write` (a gate demo that changes nothing) is listed only when the
kernel runs with `FBMCP_DEMO_TOOLS=1` (the smoke and kill harnesses set it).

```powershell
.\dist\fbmcpctl.exe approve --config .\fbmcp.dev.yaml                      # list
.\dist\fbmcpctl.exe approve --config .\fbmcp.dev.yaml <request_id>         # asks y/N
.\dist\fbmcpctl.exe approve --config .\fbmcp.dev.yaml --deny <request_id> --reason "..."
```

The running server polls `state/approvals/` every 2 seconds and then executes.

## 6. One kernel (Claude may spawn two processes)

fbmcp takes a lock on `state.dir`. Only that process is the kernel. Extra **piped** stdio clients (Claude Desktop starts two MCP hosts) attach to it instead of exiting. A second copy typed in a terminal still fails fast.

If Claude cannot start the server, check:

```powershell
.\dist\fbmcpctl.exe status --config .\fbmcp.dev.yaml
```

`mcp-server-fbmcp.log` should show `initialize` / `tools/list`. A leftover `another fbmcp instance is active` on a **pipe** means the first process is an old binary without attach — replace `dist\fbmcp.exe` and fully quit Claude.

## 7. Troubleshooting

| Symptom | What to check |
|---|---|
| Tools never appear | JSON syntax (no trailing commas); absolute paths; full restart; `main.log` `Connected to fbmcp` |
| Server exits immediately | `%APPDATA%\Claude\logs\` MCP logs; `fbmcpctl doctor` (each FAIL says the fix) |
| `CONNECTION_CLOSED` / “another fbmcp instance is active” | Rebuild so attach is in the binary; stop a leftover `fbmcp.exe`; or use a different `state.dir` |
| Firebird offline | HQbird services on ports 3053/3054/3055; `fb_db_health` |
| Tier-2 “confirmation rejected” | Expected in-band. Use `fbmcpctl approve` |

Remote `/mcp` HTTP is **not** what this local setup uses. Leave `listen` empty.

## 8. Remote server (fbmcp on another host)

When fbmcp runs on the database server in remote mode
([`remote.md`](remote.md)), Claude Desktop connects through the bridge.
Store the API key once: `fbmcpctl secret set FBMCP_KEY_SRV1` (stdin).

```json
{
  "mcpServers": {
    "fb-srv1": {
      "command": "C:\\fbmcp\\fbmcp.exe",
      "args": ["bridge", "--url", "https://srv1:8443/mcp",
               "--key-env", "FBMCP_KEY_SRV1", "--pin", "sha256:<hex>"]
    }
  }
}
```

The pin comes from the server admin (`fbmcpctl tls selfsign` prints it).
Claude Code can connect directly over HTTP; keep the key out of the file
with an env reference (the variable must be set for your user):

```json
{"mcpServers": {"fb-srv1": {"type": "http", "url": "https://srv1:8443/mcp",
  "headers": {"Authorization": "Bearer ${FBMCP_KEY_SRV1}"}}}}
```

A self-signed cert then needs `NODE_EXTRA_CA_CERTS=<srv1.crt>`. Tier 2
approvals still happen on the server (tray or `fbmcpctl approve`).
