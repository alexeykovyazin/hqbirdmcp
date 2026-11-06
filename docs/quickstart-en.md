# fbmcp — quick start: install and use

**English** | [Русский](quickstart-ru.md) | [Português](quickstart-pt.md)

fbmcp is an MCP server for Firebird 2.5–5.0. An MCP client (Claude
Desktop, Claude Code, Cursor, Codex, ZCode) uses it to work with databases
as a DBA: monitoring, backups and restore checks, SQL, indexes, users,
config, fbagent. Every change passes a policy check and a human
confirmation. Every decision is written to the audit log (`audit.jsonl`).

Details: [claude-desktop.md](claude-desktop.md) (local),
[remote.md](remote.md) (remote), [tool-reference.md](tool-reference.md) (all tools).

HQbird 2027 servers have fbmcp already (one service for all Firebird
lines): see [`hqbird.md`](hqbird.md) instead of sections 3–4.

## 1. Two modes

| Mode | Where fbmcp runs | When to use |
|---|---|---|
| Local (stdio) | On the same PC as the client | Firebird on your PC or in the local network |
| Remote (HTTPS + API keys) | A service on the database server | Several admins, a distant server, agents |

## 2. Get and build

1. Install **git** and **Go ≥ 1.25.13** (CGO is not needed). Windows:
   ```powershell
   winget install Git.Git
   winget install GoLang.Go
   go version        # go1.25.13 or later; if "go" is not found, open a new window
   ```
   Linux: git from your packages, Go from https://go.dev/dl/.
2. Get the source and build:
   ```powershell
   git clone https://github.com/alexeykovyazin/hqbirdmcp.git
   cd hqbirdmcp
   $env:CGO_ENABLED = "0"
   go build -trimpath -o dist/fbmcp.exe ./cmd/fbmcp          # server
   go build -trimpath -o dist/fbmcpctl.exe ./cmd/fbmcpctl    # operator CLI
   go build -trimpath -ldflags "-s -w -H=windowsgui" -o dist/fbmcp-tray.exe ./cmd/fbmcp-tray  # tray (Windows)
   ```
3. Copy the programs to a folder of their own (all examples use `C:\fbmcp\`):
   ```powershell
   mkdir C:\fbmcp
   copy dist\*.exe C:\fbmcp\
   ```

On Linux, build `fbmcp` and `fbmcpctl` the same way (no `.exe`, no tray)
and copy them to `/usr/local/bin/`.

`fbmcpctl` and `fbmcp ctl` are the same program:
`C:\fbmcp\fbmcp.exe ctl doctor` = `C:\fbmcp\fbmcpctl.exe doctor`.
`fbmcpctl help` lists the commands; `fbmcpctl <command> -h` shows one.

## 3. Local install (Windows)

1. **Config** `C:\fbmcp\fbmcp.yaml` (full example: `packaging/fbmcp.yaml.example`):
   ```yaml
   state:
     dir: C:\ProgramData\fbmcp\state
   instances:
     - id: fb5
       addr: localhost:3050
       bin_dir: C:\Program Files\Firebird\Firebird_5_0   # the folder with gbak, gfix, gstat
       version: "5.0"
       service: FirebirdServerDefaultInstance
   databases:
     - id: main                       # in the chat, a database is named by id, not by path
       instance: fb5
       path: D:\Data\main.fdb
       backup_dir: D:\fbmcp\backups
       work_dir: D:\fbmcp\work
       ro_user: FBMCP_RO              # a read-only user (not SYSDBA)
       ro_secret_env: FBMCP_MAIN_RO_PW
       admin_user: SYSDBA
       admin_secret_env: FBMCP_MAIN_ADMIN_PW
   ```
   Make the folders from the config (`doctor` also makes the state folder):
   ```powershell
   mkdir C:\ProgramData\fbmcp\state, D:\fbmcp\backups, D:\fbmcp\work
   ```
2. **Read-only user.** `ro_user` is a Firebird user that can only read.
   Create it once, as SYSDBA, in isql
   (`isql -user SYSDBA -password <password> localhost/3050:D:\Data\main.fdb`):
   ```sql
   CREATE USER FBMCP_RO PASSWORD 'choose-a-password';
   COMMIT;
   -- read access to all tables and views of this database;
   -- run it in every database, and again after you add tables
   SET TERM ^ ;
   EXECUTE BLOCK AS
     DECLARE r VARCHAR(63);
   BEGIN
     FOR SELECT TRIM(RDB$RELATION_NAME) FROM RDB$RELATIONS
         WHERE COALESCE(RDB$SYSTEM_FLAG, 0) = 0 INTO :r DO
       EXECUTE STATEMENT 'GRANT SELECT ON "' || r || '" TO FBMCP_RO';
   END^
   SET TERM ; ^
   COMMIT;
   ```
   Firebird 2.5: if `CREATE USER` fails, use gsec:
   `gsec -user SYSDBA -password <password> -add FBMCP_RO -pw <new password>`.
   A user that is not SYSDBA and not the database owner sees only its own
   connections in the MON$ tables, so `fb_sessions` shows less. On Firebird
   4.0 and 5.0, let it see all connections (in every database):
   ```sql
   CREATE ROLE FBMCP_MONITOR SET SYSTEM PRIVILEGES TO MONITOR_ANY_ATTACHMENT;
   GRANT DEFAULT FBMCP_MONITOR TO USER FBMCP_RO;
   COMMIT;
   ```
3. **Passwords** do not go in the YAML. Store them in the Windows credential
   store (type the value, then Ctrl+Z, Enter):
   ```powershell
   C:\fbmcp\fbmcpctl.exe secret set FBMCP_MAIN_RO_PW
   C:\fbmcp\fbmcpctl.exe secret set FBMCP_MAIN_ADMIN_PW
   ```
4. **Check:** `C:\fbmcp\fbmcpctl.exe doctor`. It finds `C:\fbmcp\fbmcp.yaml`
   next to the program (or give `--config <file>`) and logs in to every
   database with both users. Each FAIL line ends with `Fix:` and the command
   to run. Repeat until the last line is `doctor: green`.
5. **Claude Desktop:** Settings → Developer → Edit Config, add to `mcpServers`:
   ```json
   "fbmcp": {
     "command": "C:\\fbmcp\\fbmcp.exe",
     "args": ["-config", "C:\\fbmcp\\fbmcp.yaml"]
   }
   ```
   Quit Claude Desktop fully (the tray icon too) and start it again.
6. **Claude Code:**
   ```powershell
   claude mcp add --transport stdio fbmcp -- "C:\fbmcp\fbmcp.exe" -config "C:\fbmcp\fbmcp.yaml"
   ```
7. **Tray for approvals** (Tier 2): `C:\fbmcp\fbmcp-tray.exe C:\fbmcp\fbmcp.yaml`.

Use absolute paths everywhere. Do not set `listen` in local mode.
The passwords are in the Windows credential store (step 3), so the client
settings need no `env` block.

### Cursor

File `%USERPROFILE%\.cursor\mcp.json` (all projects) or `.cursor\mcp.json`
in the project folder. The format is the same as for Claude Desktop:
```json
{
  "mcpServers": {
    "fbmcp": {
      "command": "C:\\fbmcp\\fbmcp.exe",
      "args": ["-config", "C:\\fbmcp\\fbmcp.yaml"]
    }
  }
}
```
Then open Cursor Settings → MCP: the `fbmcp` server must be on (green dot).

### Codex (OpenAI Codex CLI / IDE)

With a command:
```powershell
codex mcp add fbmcp -- C:\fbmcp\fbmcp.exe -config C:\fbmcp\fbmcp.yaml
```
Or by hand in `%USERPROFILE%\.codex\config.toml` (single quotes, so you do
not need to double `\`):
```toml
[mcp_servers.fbmcp]
command = 'C:\fbmcp\fbmcp.exe'
args = ["-config", 'C:\fbmcp\fbmcp.yaml']
```
Check: `codex mcp list`.

### ZCode

File `.zcode\config.json` in the project folder (key `mcp.servers`, not `mcpServers`):
```json
{
  "mcp": {
    "servers": {
      "fbmcp": {
        "command": "C:\\fbmcp\\fbmcp.exe",
        "args": ["-config", "C:\\fbmcp\\fbmcp.yaml"]
      }
    }
  }
}
```
If the file holds passwords, add `.zcode/` to `.gitignore`.

After a config change, restart the client: it loads servers only at start.

## 4. Remote install (short)

**Windows server** (elevated PowerShell):
1. Copy `fbmcp.exe`, `fbmcpctl.exe`, `fbmcp-tray.exe` to `C:\fbmcp\`, write `fbmcp.yaml` (template: [remote.md §3](remote.md)).
2. Certificate (write down the printed pin `sha256:...`):
   `C:\fbmcp\fbmcpctl.exe tls selfsign --out C:\fbmcp\tls --dns srv1 --ip 10.0.0.5 --grant "NT SERVICE\fbmcp"`
3. One key per person or agent (the value is printed once):
   `C:\fbmcp\fbmcpctl.exe apikey new alice --max-tier 2 --secrets-dir C:\ProgramData\fbmcp\secrets`
4. Passwords and agent tokens: `C:\fbmcp\fbmcpctl.exe secret set --dir C:\ProgramData\fbmcp\secrets NAME`
5. Install the service, dry run first:
   `.\packaging\windows\install-remote.ps1 -Port 8443 -AllowFrom 10.0.0.0/24 -FirebirdService FirebirdServerDefaultInstance -WhatIf`, then without `-WhatIf`.
6. Check: `C:\fbmcp\fbmcpctl.exe doctor --remote C:\fbmcp\fbmcp.yaml` → no FAIL lines.
7. Tray for the person who approves: `.\packaging\windows\install-tray.ps1`.

**Linux server** (root):
```sh
install -m 0755 fbmcp fbmcpctl /usr/local/bin/
sh packaging/linux/install-remote.sh --user firebird --port 8443 --allow-from 10.0.0.0/24 --dry-run
sh packaging/linux/install-remote.sh --user firebird --port 8443 --allow-from 10.0.0.0/24
```
The script makes the folders, the certificate, a first config with an
`admin` key (printed once), the service, the sudo rule and the ufw rule.
Then add `databases:` and `agents:` to `/etc/fbmcp/fbmcp.yaml`. The config
reloads by itself.

**Admin PC:**
1. Store the key you got: `C:\fbmcp\fbmcpctl.exe secret set FBMCP_KEY_SRV1`.
2. Claude Desktop, through the bridge:
   ```json
   "fb-srv1": {
     "command": "C:\\fbmcp\\fbmcp.exe",
     "args": ["bridge", "--url", "https://srv1:8443/mcp", "--key-env", "FBMCP_KEY_SRV1", "--pin", "sha256:<hex>"]
   }
   ```
   Cursor, Codex and ZCode: the same entry in their file (§3), only with the bridge `args`:
   ```toml
   # Codex, config.toml
   [mcp_servers.fb-srv1]
   command = 'C:\fbmcp\fbmcp.exe'
   args = ["bridge", "--url", "https://srv1:8443/mcp", "--key-env", "FBMCP_KEY_SRV1", "--pin", "sha256:<hex>"]
   ```
   The bridge reads the key from the credential store and checks the
   certificate by its pin, so a self-signed certificate needs no CA setup
   in the client.
3. Check: ask the client to call `fb_ping`. The answer shows the server name and your access level.

## 5. Usage

Ask in plain words. Examples:
- "Call `fb_ping` and `fb_db_list`."
- "Check the health of database main" (`fb_db_health`).
- "Show active sessions and long transactions" (`fb_sessions`, `fb_transactions`).
- "Back up main and check the restore" (`fb_backup_start`, `fb_restore_test`).
- "Explain the plan of this query and suggest indexes" (`fb_analyze_query`, `fb_index_advice`).

**Tiers and confirmations:**

| Tier | What it is | How to confirm |
|---|---|---|
| 0 | Reads | Not needed |
| 1 | Changes (DML, DDL, backup) | The client shows a request id and a token; you agree, the client calls `fb_confirm` |
| 2 | High-risk actions (service restart, restore-replace, config) | Only outside the chat: tray (Approve) or `fbmcpctl approve <id>` on the server |
| 3 | Critical | Disabled |

Tier 2 usually needs an open maintenance window (`fb_window_open`) and a
verified backup less than 24 hours old (the server says what is missing).
A request expires after 15 minutes. You can run any risky call first with
`mode: preview`: it shows what will be done.

## 6. Daily operations

- **Approve a request:** `fbmcpctl approve` lists the requests with the exact commands;
  `fbmcpctl approve <id>` shows the request, asks y/N and approves;
  `fbmcpctl approve --deny <id> --reason "..."` denies. `--yes` does not ask (scripts, `ssh`).
- **Which config:** every command takes `--config <file>`. Without it: `FBMCP_CONFIG`, then
  `fbmcp.yaml` in the current folder, then next to the program, then the HQbird default.
  The command prints `using config <path>`.
- **Rotate a key:** `fbmcpctl apikey rotate alice --config <fbmcp.yaml>`. The old key stops working in a few seconds.
- **List keys:** `fbmcpctl apikey list` (values are never shown).
- **Renew the certificate:** `fbmcpctl tls selfsign --renew --out <dir> --dns ... --ip ...` (the pin does not change).
- **Check the audit:** `fbmcpctl verify`.
- **Logs:** Windows `<state_dir>\fbmcp.log`; Linux `journalctl -u fbmcp`.
- **Upgrade fbmcp:** stop the service, replace the exe files, start, run `doctor --remote`.

## 7. Troubleshooting

| Symptom | What to do |
|---|---|
| Tools do not appear in the client | Check the JSON, the absolute paths; restart the client fully |
| The server exits at once | `fbmcpctl doctor`: each FAIL line ends with `Fix:` and the command |
| 401 | Wrong key: `fbmcpctl apikey list`, `doctor --remote` |
| 403 | Your IP is not in `allow_from` or in the firewall rule |
| Pin mismatch | The server certificate key changed: get the new pin from the admin |
| Tier 2 "confirmation rejected" in the chat | Expected: approve in the tray or with `fbmcpctl approve` |

## 8. Security

- Passwords and keys go only to the credential store (`fbmcpctl secret set`) or to the server secrets folder. Never to the YAML or the client JSON.
- For autonomous agents (OpenClaw and similar): a separate key with `--profile autonomous --allow-from <IP>`. Such a key is read-only by default, and an operator confirms Tier 1.
- Windows: the fbmcp folder and the Firebird/HQbird folders must not give write access to all users (Authenticated Users: Modify). The install script protects only the fbmcp folder.
