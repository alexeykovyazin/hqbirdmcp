# ADR-009 — Secret store rollout (D5) + residual file-lock policy (D6)

Status: accepted (2026-08-16) · Fed by: P0.1, P0.3

## D5 — secret store
- **M1:** environment-only (`FBMCP_SYSDBA_PW`, per-DB vars) — documented
  convention; nothing in config files.
- **Before M3:** OS keyring (Windows Credential Manager / freedesktop Secret
  Service) via a small pure-Go binding, keyring entries created by the
  bootstrap CLI (P5.6); env remains the fallback.
- Secrets never in argv (spike-proven env pattern), never in job output,
  scrubbed from audit/log/errors (P1.4 scrubber + tests).

## D6 — residual file-lock policy (with D8/ADR-005)
The single-instance lock file guards kernel state; within the one process,
state files are written by their owning component only. Residual: config file
hot-reload and audit reader may be read concurrently by tools — readers use
rename-atomic open; writers write-temp + rename. No cross-process file locks
beyond the instance lock (v1); documented operator responsibility to point
all invocations at one state dir.

## Consequences
Threat T-06/T-15 mitigations testable in P1.4; P5.6 owns keyring bootstrap.
