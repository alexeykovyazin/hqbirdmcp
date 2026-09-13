# ADR-031: fbagent bridge — config section and SSRF stance

Status: accepted (2026-09-13, Phase 9 / A1)

## Context

Phase 9 adds MCP proxies for the co-located fbagent Local API
(loopback HTTP + Bearer, documented in the fbagent repo at release 2.21.0).
The endpoint address, credential and instance identity need a home in
`fbmcp.yaml`, and the tool surface must never become a vector for
server-side request forgery: a tool that accepts a host/URL argument would
let a prompt move authenticated calls off the loopback.

## Decision

1. **New top-level `agents:` list** (`internal/config.Agent`), fields
   `id`, `base_url`, `token_env`, `instance_id` (default `fb1`),
   `timeout_seconds` (default 130 — must exceed the agent's
   `restart_timeout_seconds`, 120; `Validate()` clamps), `tier2_jobs`
   (ADR-032) and `allow_non_loopback` (default false). The bridge is
   disabled when the list is empty; one entry → implicit default target;
   several → tools take an `agent` selector argument.
2. **SSRF stance:** `base_url` comes only from config. No bridge tool
   accepts a host, URL or base-path argument. `Validate()` enforces scheme
   http(s) and a loopback host unless `allow_non_loopback: true` is set
   explicitly (not recommended — the agent itself rejects non-loopback
   binds).
3. **Token hygiene:** config stores only the env-var **name**
   (`token_env`); the value resolves via `config.SecretFromEnv` (env
   wins, OS keyring fallback, D4/E.4). Audit scrubbing already drops
   `token`-keyed Detail fields.
4. **Artifact path guards client-side:** database folder keys refuse
   separators and `..` (mirroring the agent's own pathguard), and
   per-endpoint query parameters (`tail`, `date`, `at`, `limit`) are
   validated against their documented routes before any network call.

## Consequences

- The lab/live deployments are pure config: no code change to point the
  bridge at another agent; the phase9L live suite injects the entry from
  environment variables without touching `fbmcp.yaml`.
- A misconfigured `instance_id` fails per-call with
  `agent-not-found` (probed on the lab: the real agent uses
  `fb-master-3055`, not the `fb1` default).
- Responses decode into loose types / `RawMessage` on purpose: the agent
  evolves independently and unknown fields never break the bridge
  (`/v1/instances` is already a bare array where the docs show an object
  wrapper).
