# ADR-018 — Trace configuration model (template-only)

Status: accepted (2026-08-17) · Fed by: P3.6 T1

## Context
Firebird trace configuration is a small language the engine interprets.
Accepting free-form config from the MCP client would let the model inject
arbitrary tracing (and, at worst, overwhelm the host with log volume).

## Decision
Trace sessions are started **only from named templates** shipped with the
server. The client passes `template: audit-lite | performance | errors`.
Unknown names are refused. No config text is accepted from the tool args.

Templates (bounded: `max_log_size = 8` MiB at the engine, plus a server-side
8 MiB drain cap that stops the session):

- **audit-lite** — connections + statement finish (no parameters)
- **performance** — statement finish with `time_threshold = 100` ms
- **errors** — `log_errors = true` only

Output is written under the database work dir (`trace_<id>.log`). Session
identity (engine id ↔ job/state record) is persisted; on restart, engine
`List()` is compared and orphans are reported (not auto-killed).

## Consequences
`fb_trace_start/stop/list` implement this. Fuzzing in P6.1 must include
template-name injection (`../`, SQL, config fragments) — all refused.
