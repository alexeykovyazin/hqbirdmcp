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

That variable must be set in the MCP client `env` block (not in the YAML). For the spike DBs the password is `masterkey`. Do not use this against production databases.

Create the state directory once:

```powershell
New-Item -ItemType Directory -Force -Path C:\HQbirdData\output\fbmcp-spike\state
```

Optional check (from `fbmcp/`):

```powershell
$env:FBMCP_DEV_PW = "masterkey"
.\dist\fbmcpctl.exe doctor .\fbmcp.dev.yaml
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
        "FBMCP_DEV_PW": "masterkey"
      }
    }
  }
}
```

A copy lives at [`packaging/claude_desktop_config.example.json`](claude_desktop_config.example.json).

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
      "env": { "FBMCP_DEV_PW": "masterkey" }
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
| ≥ 2 | **Out-of-band only.** Claude cannot confirm. On the host: `fbmcpctl approve <state_dir> <request_id>` |

State dir for the spike config: `C:\HQbirdData\output\fbmcp-spike\state`.

```powershell
.\dist\fbmcpctl.exe approve C:\HQbirdData\output\fbmcp-spike\state
.\dist\fbmcpctl.exe approve C:\HQbirdData\output\fbmcp-spike\state <request_id>
```

The running server polls `state/approvals/` every 2 seconds and then executes.

## 6. One kernel (Claude may spawn two processes)

fbmcp takes a lock on `state.dir`. Only that process is the kernel. Extra **piped** stdio clients (Claude Desktop starts two MCP hosts) attach to it instead of exiting. A second copy typed in a terminal still fails fast.

If Claude cannot start the server, check:

```powershell
.\dist\fbmcpctl.exe status .\fbmcp.dev.yaml
```

`mcp-server-fbmcp.log` should show `initialize` / `tools/list`. A leftover `another fbmcp instance is active` on a **pipe** means the first process is an old binary without attach — replace `dist\fbmcp.exe` and fully quit Claude.

## 7. Troubleshooting

| Symptom | What to check |
|---|---|
| Tools never appear | JSON syntax (no trailing commas); absolute paths; full restart; `main.log` `Connected to fbmcp` |
| Server exits immediately | `%APPDATA%\Claude\logs\` MCP logs; `fbmcpctl doctor`; missing `FBMCP_DEV_PW` |
| `CONNECTION_CLOSED` / “another fbmcp instance is active” | Rebuild so attach is in the binary; stop a leftover `fbmcp.exe`; or use a different `state.dir` |
| Firebird offline | HQbird services on ports 3053/3054/3055; `fb_db_health` |
| Tier-2 “confirmation rejected” | Expected in-band. Use `fbmcpctl approve` |

Remote `/mcp` HTTP is **not** what Claude Desktop uses. Leave `listen` empty.
