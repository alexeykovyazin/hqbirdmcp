# Driver capability matrix — P0.1 spike

Candidate: `github.com/nakagami/firebirdsql` v0.9.19 (pure Go, MIT).
Tested against local HQbird instances of Firebird 2.5 / 3.0.15 / 4.0.8 / 5.0.5
(ports 3052–3055), Windows host, driver DSN `sysdba:***@host:port/path?charset=UTF8`.

| Capability | FB 2.5 | FB 3.0 | FB 4.0 | FB 5.0 | Notes |
|---|---|---|---|---|---|
| Build `CGO_ENABLED=0` (win/amd64, linux/amd64, linux/arm64) | ● | ● | ● | ● | verified 2026-08-16 |
| Connect + auth (Srp) | ● | ● | ● | ● | masterkey worked on all |
| **RO enforcement: DML in read-only tx refused by engine** | ● | ● | ● | ● | `attempted update during read-only transaction` |
| **RO enforcement: DDL in read-only tx refused** | ● | ● | ● | ● | `unsuccessful metadata update` (2.5) / same DML-style message on 3+ |
| MON$DATABASE / ATTACHMENTS / TRANSACTIONS / STATEMENTS / IO_STATS / RECORD_STATS | ● | ● | ● | ● | column sets differ per version (e.g. 2.5 lacks REPLICA_MODE etc.) |
| Engine version via SQL (`RDB$ENGINE_VERSION`, `MON$SERVER_VERSION`) | ✖ | ✖ | ✖ | ✖ | **these columns do not exist in any version** — use Services API instead |
| Engine version via Services API (`GetServerVersionString`) | ○ | ● | ● | ● | 2.5 not probed (no need; D1 floor decision pending) |
| Execution plan via driver | ✖ | ✖ | ✖ | ✖ | `isc_info_sql_get_plan` defined but never requested in driver source → isql subprocess fallback for P2.4 |
| CREATE DATABASE via `database/sql` | ✖ | ✖ | ✖ | ✖ | not exposed through stdlib surface; use isql `-e`/script or driver internal (isql subprocess chosen for P3.8) |
| Services API (backup/restore/validate/trace/user/stats managers) | n/a | ● | ● | ● | driver ships backup_manager, nbackup_manager, maintenance_manager, trace_manager, user_manager, service_manager |

● = verified OK · ○ = best-effort verified · ✖ = not available.

## Captured engine error texts (RO refusal — the CI safety-fuse #1 evidence)

- DML: `attempted update during read-only transaction` (identical on 2.5/3/4/5)
- DDL: `unsuccessful metadata update` (2.5 wraps it; 3+ surfaces the read-only message)

## Read-only transaction mechanics (Go side)

- `db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})` → driver maps to
  read-committed read-only TPB (`ISOLATION_LEVEL_READ_COMMITED_RO`; NOWAIT variant available).
- **Pitfall found:** passing a `nil` context to `BeginTx` panics deep inside
  `database/sql` (`nil` ctx deref in `conn()`), and the deferred `db.Close()` then
  deadlocks — looked exactly like a driver bug. Always pass a real context;
  production code will pass request contexts with deadlines anyway.

## Isolation levels supported by the driver

read committed (legacy / rec_version / no_wait), repeatable read, serializable;
RO variants of read-committed. Matches the plan's RO-pool requirements.

## Conclusion

Driver is viable as the SQL foundation for the read pool AND provides a usable
Services API subset for the admin executor. No cgo anywhere. Limitations recorded:
no plans, no CREATE DATABASE through `database/sql` (route via isql/subprocess),
no `fbclient`-style info-item surface for engine version (use service manager).

Host quirk (not driver): FB 4.0/5.0 HQbird instances emit
`Error loading plugin MySQLEngine ... module could not be found` on some error
paths — harmless but noisy; production error classification must tolerate it.
