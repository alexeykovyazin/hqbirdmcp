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
**Single active instance enforced by a lock file** (kernel state dir). A
second process fails fast with "another client is attached — use fbmcp status
or stop the other session". Daemon + thin-client is the documented fallback
if multi-client concurrency becomes a requirement.

## Consequences
- Safety fuse #6 (CI): second instance must fail fast, never dual-write.
- The approval surface (A8) serves the *state dir*, not a process — P1.6
  design must read the pending-action store under the same lock discipline.
- P5.3 scheduler runs inside the single server process; a separate cron
  spawning a second process must instead call the server (documented).
