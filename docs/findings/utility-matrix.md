# Utility & admin-execution matrix — P0.2 spike

Run date: 2026-08-16, Windows host, HQbird FB 3.0.15 / 4.0.8 / 5.0.5.
Utilities invoked with absolute paths from `C:\HQbird\Firebird{30,40,50}\`.

## Per-operation route table (feeds ADR-003 / D2)

| Operation | Route | Evidence |
|---|---|---|
| Backup (gbak full) | **Driver Services API** (`BackupManager.Backup`), subprocess fallback | both proven; API produced valid .fbk |
| Backup (nbackup levels) | Driver Services API (`nbackup_manager.go` exists) + subprocess fallback | API present, not exercised this pass |
| Verify backup | **No standalone gbak mode exists** — `gbak -v file.fbk` fails with "requires both input and output filenames"; verification = test-restore (P3.2) | captured error text |
| Restore | Driver Services API (RestoreManager) / gbak -r subprocess | API present, not exercised this pass |
| Validate / sweep / shutdown / properties | Driver `maintenance_manager.go` + gfix subprocess fallback | gfix -validate subprocess proven (silent success, exit 0) |
| Statistics (gstat-equivalent) | **Driver Services API** (`StatisticsOptions`, incl. header-only) + gstat subprocess (golden corpus captured) | gstat -h captured for 3/4/5 |
| Trace sessions | Driver `trace_manager.go` + fbtracemgr fallback | not exercised this pass |
| Users/roles | Driver `user_manager.go` (services API) | not exercised this pass |
| Plan retrieval | isql subprocess (from P0.1 finding) | driver has no plan API |
| CREATE DATABASE | isql subprocess | driver can't via database/sql |
| Engine version | Driver Services API `GetServerVersionString` | proven on 3/4/5 |

**Strategy (ADR-003): hybrid, API-first** — everything the driver's Services
API covers goes through it (in-process, no argv, no env creds, progress channel);
subprocess remains the fallback and the route for isql-only needs.

## Credential passing (T2)

- `ISC_PASSWORD` env var works for gbak/gstat/gfix (never in argv). ✔
- **Windows trusted-auth caveat:** `gstat` (and by extension other local
  utilities) **succeeds even with a wrong ISC_PASSWORD** when the invoking OS
  user is privileged (WinSSI/trusted auth). Local access control is therefore
  OS-account-based — reinforces §4.1 (dedicated OS account) rather than
  weakening it: the password is not the control boundary locally, the process
  owner is. Error texts for true auth failure still captured for remote-style
  connections (taxonomy below).

## Execution harness proof (T7)

- absolute path + argv array + env-only secret + wall-clock timeout +
  output-size cap — all implemented in `spikes/p02-admin-exec/main.go`.
- Timeout kill verified (1 ms deadline → process terminated, error returned).
- **os/exec Windows pitfall (spike-grade finding, must carry into P1.7):**
  assigning *different non-file writers* to `cmd.Stdout` and `cmd.Stderr`
  silently loses output on Windows; both must reference the same writer value
  (single shared pipe). Reproduced minimal in the spike; root cause in Go's
  os/exec pipe wiring — recorded as a kernel implementation constraint.

## Error taxonomy (first samples, golden corpus `golden/`)

| Class | Text sample | Exit |
|---|---|---|
| auth-failure (remote-style) | `Your user name and password are not defined...` (2.5 style) | 1 (not locally reproducible — trusted auth; see above) |
| bad invocation | `gbak: ERROR:requires both input and output filenames` | 1 |
| I/O error | `gbak: ERROR:cannot open status and error output file con` | 1 |
| success-verbose | `gbak:closing file, committing, and finishing. N bytes written` | 0 |
| validate-clean | (empty output) | 0 |

Golden corpus captured: gbak-backup-verbose, gbak-verify(error), gstat-header,
gfix-validate-full, gstat-auth-failure — × {FB3.0, FB4.0, FB5.0}. Manifest:
charset UTF8 console, locale en-US, Windows code page 437/65001 mixed (raw
captures preserved as-is).

## Plan corrections fed forward

1. **P3.1 "verify (gbak -v)" is a myth** — verification is a test-restore
   (already P3.2); P3.1's verify tool must be reframed as "verify = restore to
   scratch + page-read", or nbackup verify if applicable. → update phase3 plan.
2. P1.7 executor must use the shared-writer pattern for subprocess output.
3. Trusted-auth finding strengthens the §4.1 dedicated-account requirement and
   must appear in the threat model (P0.4): *any local process of the service
   account can admin the DBs regardless of credentials* — the service account
   is the real privilege boundary on Windows hosts.
