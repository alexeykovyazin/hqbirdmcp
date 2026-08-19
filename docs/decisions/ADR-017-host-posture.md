# ADR-017 — Host privilege posture for service control

Status: accepted (2026-08-17) · Fed by: P3.7 leftover; written in P5.2

## Context
`fb_service_status` is read-only. Start/stop/restart need OS rights that the
Firebird DBA MCP server must not assume. Phase 3 named this ADR and deferred
the tools until packaging shipped verify-mode scripts.

## Decision

**Refuse without posture.** `fb_service_start` / `stop` / `restart` (Tier 2,
out-of-band) refuse unless `{state.dir}/posture.verified` exists. The error
tells the operator to run `fbmcpctl doctor` or `packaging/posture/verify`.

**Verify mode (no mutation).** Scripts check, they do not apply:
- Windows: service account can `sc query` the configured service names; ACL
  notes for backup/work/state dirs.
- Linux: sudoers allowlist would permit only the exact `systemctl start|stop|
  restart <unit>` lines; `ProtectSystem` paths exist.

**Apply** is opt-in via `fbmcpctl setup` (writes the marker after verify
green) or an operator-run script. The server never silently elevates.

## Consequences
P3.7 tools live behind this gate. P5.6 doctor re-runs verify. Dedicated
least-privilege service account remains the T-11 residual boundary.
