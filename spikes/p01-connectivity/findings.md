# P0.1 connectivity spike — findings narrative

Run date: 2026-08-16. Host: Windows 10 x64, Go 1.24.5. Instances: HQbird
FB 2.5 (3052), 3.0.15 (3053), 4.0.8 (3054), 5.0.5 (3055), all running as
Windows services. Spike DBs: `C:/HQbirdData/output/fbmcp-spike/spike_*.fdb`
(created via isql; note: driver cannot CREATE DATABASE through `database/sql`).

Deviations from the phase plan: Docker Desktop is not running on this host, so
the Linux leg ran as cross-compile verification only (all 3 targets build with
CGO_ENABLED=0); runtime Linux verification is deferred and tracked in ADR-001
consequences. The Windows service leg is *stronger* than planned (4 real
versions on one host).

## Answers to the "must answer" rows

- **T1 build matrix:** green on windows/amd64, linux/amd64, linux/arm64,
  `CGO_ENABLED=0`. License MIT. Version pinned v0.9.19 (2026-05-17 tag).
- **T2 connectivity/auth:** TCP connect + Srp auth OK on 2.5/3/4/5; UTF8 DSN
  parameter accepted everywhere. WireCrypt not varied in this pass (default
  Enabled accepted by all instances) — acceptable; WireCrypt is only a config
  surface for us (op 39 → P4.6).
- **T3 RO enforcement (core proof):** PASS on all four versions —
  read-only TPB via `sql.TxOptions{ReadOnly:true}`; engine refuses DML with
  `attempted update during read-only transaction`, refuses DDL as well.
  Full texts in the capability matrix. (Read-only *user* belt-and-braces case
  deferred to P5.6 bootstrap script work — the engine-level proof is the
  safety-relevant half and it is green.)
- **T4 MON$ surface:** all six monitored tables readable on all versions;
  column sets differ (2.5 narrower — recorded). Snapshot/delta feasibility:
  plain queries work; delta logic is P2.7 work.
- **T5 plan retrieval:** **negative result** — driver defines
  `isc_info_sql_get_plan` but never requests it; `database/sql` has no surface
  to reach it anyway. Route for P2.4: isql subprocess (`SET PLANONLY` /
  EXPLAIN on 4+), parsed from output. Driver-side fork/PR is a possible later
  optimization.
- **T6 concurrency/pooling:** `database/sql` pooling works (parallel MON$
  queries over one handle); no goroutine issues observed. Full soak is P1.2
  work. Second pitfall recorded: `BeginTx(nil, …)` panics + `Close()` deadlock
  — always pass contexts.
- **T7 CREATE/DROP DATABASE:** not available via `database/sql`; isql
  subprocess route chosen for P3.8 (drop stays Tier-3 stub anyway).
- **T8 2.5 best-effort:** connects, MON$ core readable, RO enforcement works.
  2.5 keeps "best-effort" status; D1 floor recommendation stays 3.0 (2.5
  lacks packages/system privileges/EXPLAIN and would constrain P4.3/P2.4).

## Surprises

1. `RDB$ENGINE_VERSION` / `MON$SERVER_VERSION` **do not exist as SQL columns**
   (my initial assumption was wrong on all versions). Engine version must come
   from the Services API — which the driver exposes and which works
   (`WI-V5.0.5.1876 Firebird 5.0 HQbird` etc.). P2.1 design note.
2. The nil-context panic masquerading as a driver bug (see matrix).
3. Driver ships a much wider Services API than expected (backup, nbackup,
   trace, maintenance, user managers) — strengthens the "hybrid, API-first"
   option for ADR-003.

## Decisions fed

- ADR-001 (driver): firebirdsql v0.9.19, pinned.
- ADR-002 (D1): minimum Firebird 3.0; 2.5 best-effort read-only.
