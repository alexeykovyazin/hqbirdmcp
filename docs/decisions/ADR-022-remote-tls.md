# ADR-022 — Remote-mode defaults and TLS policy

Status: accepted (2026-08-17) · Fed by: P5.1 / phase5_plan_v2.md

## Context
mcpFirebird shipped an unauthenticated HTTP entry. Stdio is the fbmcp
default (one process per client, D8 lock). Remote mode is opt-in and must
be impossible to enable accidentally.

## Decision

**Start triple.** Setting `listen` (non-empty) is remote mode. The process
refuses to start unless all of: non-localhost bind, TLS cert+key paths, at
least one API-key identity. Self-signed certs are generated only by an
explicit `fbmcpctl` command that prints the fingerprint.

**Entries.** `/mcp` (streamable HTTP) and `/sse` are authenticated on every
request (Bearer). Completeness tests auto-enumerate `transport.MCPEntries`.
`/healthz` is unauthenticated liveness only (`ok\n`) — no version, no
registry.

**Identity.** Stdio stays `identity.Local`. HTTP middleware injects
`identity.APIKey` into the request context. X-Forwarded-For is untrusted.

**Approvals.** No remote approval HTTP page in v1. Operators SSH to the
host and run `fbmcpctl approve`. If a page is added later, localhost-only.

## Consequences
Threat model A7/A8 updated. Fuse-style auth battery covers no-key / wrong
key / valid key × every entry. Token relay from a remote session still
cannot confirm Tier ≥ 2 (fuse #7).
