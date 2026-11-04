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

## Addendum (WS2, 2026-08-20)
**Origin allowlist.** `allowed_origins` in fbmcp.yaml (reloadable) feeds the
transport's Origin check, which previously existed but was unreachable
(hardcoded nil at both call sites, no config key). Semantics: empty list =
any Origin; non-empty = an Origin header not in the list gets 403, and a
request with **no** Origin header is always allowed — the threat is a
browser being used as a confused deputy, and non-browser clients send no
Origin. Bearer auth remains the actual authentication either way.

**Local identity ceiling.** `local_max_tier` (default 2, clamped to 0–2)
caps the stdio-local fallback identity; `identity.FallbackCount()` exposes
how often handlers fell back to it (a non-zero delta in remote mode means a
handler lost its request context).

**fb_write identity.** fb_write previously hardcoded the local identity —
remote API-key calls were audited as "local", bypassed their configured
`max_tier`, and could not in-band-confirm their own requests. It now uses
`identity.Caller(ctx)` like every other gated tool.

## E.1 addendum (2026-08-25, phase8_plan D4.1)

Remote mode additionally requires a non-empty `allowed_origins` (default-deny: any Origin header not allowlisted is 403; no-Origin requests still pass), and enforces per-identity `limits:` — token-bucket rate (default 30/min, burst 60) and concurrent-request session cap (default 8) — returning structured 429s. Closes the C11 residual.

## Phase 12 addendum (2026-11-03, phase12_plan B0–B10)

**Correction (F13).** The WS2 sentence "empty list = any Origin" is
superseded by E.1: remote mode refuses to start with an empty
`allowed_origins`. Requests without an Origin header still pass.

**Identity defaults.** An identity without `max_tier` now gets 1 (was 2);
`max_tier: 0` means read-only (it was mapped to 2). `profile: autonomous`
sets `max_tier` 0 unless given, forces `inband_confirm: false` (every Tier
1 request then goes out of band like Tier 2; `fb_confirm` from that
identity is refused) and tighter default limits (2 calls, 10/min, burst
20, 4 live sessions). `allow_from` (CIDR list) checks the TCP peer
address; X-Forwarded-For is ignored. A refused peer gets 403 and an
`auth.denied` audit entry.

**Sessions and limits.** Held GET streams count against `max_streams`, not
`max_sessions`. `max_live_sessions` caps open MCP sessions per identity;
`session_idle_minutes` closes idle ones (SDK `SessionTimeout`). Calls carry
`remote_addr` and `profile` in the audit; `session.open` is audited.

**Secrets.** Lookup order: env, then `<secrets.dir>/<NAME>` file, then the
OS keyring. File changes apply without a restart (keys, cert/key pair).
`fbmcpctl apikey new|rotate|list` and `fbmcpctl tls selfsign|show` manage
keys and the self-signed pair; `--renew` keeps the key, so the pin stays.

**Start-up errors are synchronous.** A bad cert, busy port or missing key
now fails the start with a non-zero exit (no silent run without a
listener).

**Bridge.** `fbmcp bridge` is a stdio↔HTTPS relay for stdio-only clients
(Claude Desktop). It trusts the server by SPKI pin (`--pin sha256:<hex>`)
or a CA file, keeps the session id, opens the GET notification stream,
sends DELETE on exit and re-initializes once after a server restart. It
refuses plain `http://`. Operator guide: [docs/remote.md](../remote.md).
