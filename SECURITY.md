# Security policy

## Supported versions

| Version | Supported |
|---|---|
| `v1.0.x` (once tagged) | yes |
| `0.x` development builds | best-effort; treat as pre-release |

See [ADR-025](docs/decisions/ADR-025-release-and-disclosure.md).

## Reporting a vulnerability

**Do not** open a public GitHub issue for an unreleased vulnerability.

Email: **security@localhost** (replace with the operator mailbox before the
v1.0 tag — this placeholder is intentional until a public address exists).

Include: affected version / commit, impact, reproduction **without** a
weaponized exploit if possible, and whether you are requesting embargo.

## Embargo

90 days from private report, or until a patched release is available,
whichever is sooner ([ADR-025](docs/decisions/ADR-025-release-and-disclosure.md)).

## What we consider in-scope

- Bypass of the human gate (Tier ≥ 1 side effect without confirmation)
- In-band confirmation of Tier ≥ 2
- Read-pool writes succeeding
- Path escape from backup/work dirs
- Secret leakage into audit, logs, `events.jsonl`, argv, or `fbmcpctl` stdout
- Unauthenticated `/mcp` or `/sse`
- Remote start without the TLS + identity triple

## Out of scope (accepted residuals)

- Host write access to `state.dir` (T-11)
- Empty Origin allowlist and missing HTTP rate limits unless configured
- Dual-control for compiled-in Tier-3 stubs
- Bitwise-identical builds
