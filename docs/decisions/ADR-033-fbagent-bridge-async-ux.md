# ADR-033: fbagent bridge — async operation UX and error mapping

Status: accepted (2026-09-13, Phase 9 / A1, A4)

## Context

Every mutating fbagent endpoint returns `202` + an `operation_id` and runs
the work under the agent's `restart_timeout_seconds` (default 120 s, one
operation at a time, coalescing for engine ops). fbmcp's gated executors
already run inside the job runner with a progress channel (`prog`), and
`fb_confirm` has a wait mode that blocks to a terminal job state.

## Decision

1. **Poll inside the executor.** Mutating bridge tools POST, then poll
   `GET /v1/operations/{id}` on ~2 s ticks within
   `agents[].timeout_seconds` (default 130 > 120), emitting a `prog` tick
   per poll. The first poll is immediate, so already-finished operations
   return without waiting an interval.
2. **Timeout degrades to a handle, not a loss.** On poll-deadline the tool
   fails with the `agent-timeout` errdoc code and the client can resume
   later with Tier-0 `fb_agent_operation` using the returned
   `operation_id`. Engine-op coalescing defaults to `true`.
3. **Nested restarts surface.** `fb_agent_update_apply` extracts
   `restart_operation_id` from the operation `result` (or top-level
   field) and polls that leg too before reporting.
4. **Error translation at one choke point.** The client's `do()` maps the
   agent's error envelope + HTTP status onto errdoc markers
   (`agent-unreachable`, `agent-auth`, `agent-not-found`,
   `agent-remote-denied`, `agent-unavailable`, `agent-remote-error`,
   `agent-timeout`, `agent-payload-too-large`), preserving the agent's own
   code/message in the text. The markers are seeded at the TOP of the
   errdoc signal list: agent messages can contain substrings the generic
   signals would misclassify first (e.g. the job name `find_deadlocks`
   tripping the `deadlock` signal). Transport deadline during a poll is
   reported as `agent-timeout`, not as a transport failure.
5. **413 is a recovery path, not a dead end.** The agent's decoded-size
   cap (8 MiB) surfaces as `agent-payload-too-large` with a retry-with-
   `tail=` hint; tail is validated client-side to its documented routes.

## Consequences

- One MCP call blocks up to `timeout_seconds` when `wait` is in play;
  observed on the lab that inline job runs (config snapshot, reload,
  pprof-0) complete in well under a second of agent time.
- The bridge adds no local state beyond audit rows — no new killpoints,
  and the chaos harness strips `agents:` from its derived config so a
  killed kernel can never dial a live agent.
