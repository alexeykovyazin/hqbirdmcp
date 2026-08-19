# ADR-005 — Instance model (D8)

Status: accepted (2026-08-16) · Fed by: P0.3

## Context
MCP stdio spawns one server process per client. Kernel state (audit log,
pending actions, job store) must never be dual-written.

## Evidence (P0.3)
Structural: each stdio client gets its own server process; the SDK offers no
shared-state facility. Two live processes would interleave audit-chain writes
and pending-action mutations.

## Decision
**Single kernel per state dir**, enforced by the lock file. A second process
must never open the store, audit chain, or job runner.

Claude Desktop (and some other MCP hosts) spawn **two** stdio children for
one configured server. The first child is the kernel; a later piped stdio
process **attaches** as a thin client (`internal/attach`: localhost TCP +
token files under `state.dir`) instead of exiting. An interactive console
that hits the lock still fails fast.

This is the daemon+thin-client fallback named when the ADR was accepted.

## Consequences
- Safety fuse #6 (CI): second *kernel* acquire must fail; attach clients
  share the lock-holder's MCP server and do not dual-write.
- The approval surface (A8) serves the *state dir*, not a process — P1.6
  design must read the pending-action store under the same lock discipline.
- P5.3 scheduler runs inside the single server process; a separate cron
  spawning a second process must instead call the server (documented).
- Attach is loopback-only and token-gated; same-account `state.dir` read
  remains T-11 (already accepted).
