# ADR-028 — `adminexec`-shelled Firebird CLI tools (lightweight monitoring)

Status: accepted (2026-08-19) · Fed by: phase7_plan.md P7.1

## Context
Every existing backup/restore/sweep/validate/set-property tool in fbmcp goes
through the pinned driver's Services-API wrappers
(`internal/backupsvc.Client`, wrapping `github.com/nakagami/firebirdsql`'s
`BackupManager`/`MaintenanceManager`) — ADR-003's API-first route. That
route is not available for HQBird's lightweight-monitoring action
(`isc_action_svc_lwmonitoring`, 32): the driver's `ServiceManager` wire
dispatch (`serviceAttach`) is unexported, and there is no typed wrapper or
raw-SPB primitive reachable from outside the driver for actions the existing
typed methods don't cover. `internal/adminexec` (ADR-003's subprocess
backend: absolute paths, argv arrays never a shell, env-only credentials,
timeout, output cap) already exists but had exactly one real caller before
this change — OS service control (`sc.exe`/`systemctl`,
`cmd/fbmcp/p3tools.go`). One other package, `internal/queryplan`, already
shells to `isql` the same way for `fb_analyze_query` — so this is not the
first Firebird-CLI use of the subprocess pattern, just the first via
`fb_lwmonitoring`.

## Decision
`fb_lwmonitoring` (`internal/lwmonitoring`) shells to `fbsvcmgr` — the
sanctioned way to reach arbitrary Services-API actions from outside a
driver, per HQBird's own documentation example
(`fbsvcmgr service_mgr action_lwmonitoring lwm_query N`). It follows
`internal/queryplan`'s exact pattern:
- Absolute binary path via `inst.BinDir` (same resolution
  `configedit.ConfPath` already uses).
- `ISC_PASSWORD` via `adminexec.Run`'s `secretEnv` map — never on argv.
- A short timeout (15s; this is a read/monitoring call, not a
  potentially-long backup) and an output cap (64 KiB; JSON responses are
  small).
- `.exe`/no-extension fallback for cross-platform bin resolution.

This is a **documented exception** to the Services-API-first convention, not
a precedent for routing other tools through CLI shell-out by default — the
other three P7.x parts (concurrent index, materialized views, multi-thread
backup/restore) all extend the existing Services-API/`fbparse` surfaces
instead. Multi-thread sweep was explicitly scoped out of Phase 7 rather than
adding a second `adminexec`-shelled tool for it (`gfix -sweep -par/-parallel
N`) — see phase7_plan.md's "Scope explicitly excluded".

## Consequences
- `fb_lwmonitoring` is Tier 0 (read-only monitoring, no gate) — same
  registration pattern as `fb_sessions`/`fb_activity_sample` in
  `cmd/fbmcp/p2tools.go`.
- Response bodies are passed through as the raw JSON `fbsvcmgr` prints,
  rather than parsed into four different typed Go structs for the four
  query levels — precedent: `fb_config_get` already just prints key/value
  text rather than a structured type.
- If a future tool needs a Services-API action the driver doesn't wrap,
  this ADR is the reference for the same shell-out pattern rather than
  re-deriving it — but each case should still be evaluated on whether a
  driver-level addition (as P7.2's `ParallelWorkers` turned out to already
  exist) is possible first.
