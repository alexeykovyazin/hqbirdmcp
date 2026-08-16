# ADR-001 — Go Firebird driver

Status: accepted (2026-08-16) · Fed by: P0.1

## Context
Need a pure-Go (`CGO_ENABLED=0`) driver for the read pool and admin Services
API on FB 3.0–5.0 (2.5 best-effort), Windows + Linux.

## Options
`github.com/nakagami/firebirdsql` (pure Go, database/sql) · cgo fbclient
wrappers · forks.

## Decision
**firebirdsql v0.9.19**, pinned. MIT, zero cgo, cross-compiles to
windows/amd64, linux/amd64, linux/arm64.

## Capabilities & limits (spike-verified)
- RO TPB via `sql.TxOptions{ReadOnly:true}` — engine refuses DML *and* DDL on
  FB 2.5/3/4/5 (fuse #1 evidence).
- MON$ readable on all versions (column sets vary).
- Services API managers: backup, nbackup, restore, maintenance, trace, user,
  stats — API-first admin route (ADR-003).
- **No** execution-plan retrieval (`isc_info_sql_get_plan` unused) → isql
  subprocess fallback for P2.4. **No** CREATE DATABASE via database/sql →
  isql route for P3.8.
- Engine version: use `ServiceManager.GetServerVersionString` (SQL columns
  `RDB$ENGINE_VERSION`/`MON$SERVER_VERSION` do not exist).
- Pitfall: `BeginTx(nil, …)` panics + Close deadlock — always pass contexts.
- Linux runtime unverified this host (Docker not running); cross-compile
  green; Linux CI leg to be restored in Phase 1+ (consequence, tracked).

## Consequences
Kernel conn layer templates on this driver; a fork/patch path remains open if
plan retrieval is later wanted in-process.
