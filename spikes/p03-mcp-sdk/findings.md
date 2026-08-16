# P0.3 MCP SDK spike — findings

Run date: 2026-08-16. SDK: `github.com/modelcontextprotocol/go-sdk` **v1.7.0**
(Apache-2.0), Go 1.25 toolchain required by it. Self-test: in-process client
spawns the server over stdio (`CommandTransport`) and calls both tools.

## Answers

- **T1 SDK health:** official Go SDK, v1.x stable line, active development.
  Pinned v1.7.0. Cross-compiles clean with CGO_ENABLED=0 to windows/amd64,
  linux/amd64, linux/arm64.
- **T2 skeleton:** typed tool handlers via generic `mcp.AddTool` (JSON schema
  generated from Go types) — exactly the pattern for our tool registry.
  stdio transport verified on Windows.
- **T3 elicitation:** **major protocol-semantics finding** — under protocol
  version 2026-07-28, mid-request `session.Elicit` is forbidden (SEP-2322);
  elicitation is now *multi-round-trip*: the tool returns
  `CallToolResult{InputRequests: {"confirm": &ElicitParams{...}}}` (content and
  inputRequests are mutually exclusive in one result), the client elicits,
  then retries with `Params.InputResponses`. Verified working in the self-test.
  Consequences for D3/ADR-006:
  - Elicitation UX depends on client support for input-requests (older clients
    are handled by the SDK's server middleware, which calls elicitation
    directly on old protocol versions — still works).
  - **Elicitation proves only "someone clicked in the client UI" — same trust
    level as client-approval UX**, reinforcing the plan's stance: Tier ≥ 2
    stays out-of-band regardless.
- **T4 errors/progress:** tool errors via Go error returns; progress
  notifications exist (`CallToolResult`/handler ctx hooks) — adequate for P1.7
  job UX. Not deeply exercised (deliberate: job manager is P1.7 work).
- **T5 D8 evidence:** stdio = one server process per client is structural
  (each `CommandTransport` spawns its own process). With kernel state on disk
  (audit log, pending actions) two concurrent server processes would
  dual-write. Lock-file single-instance model (D8) confirmed as necessary;
  the SDK offers nothing that changes this.
- **T6 logging:** server-side structured logging is ours; MCP logging
  notifications exist for client-visible logs. Audit hook will wrap the tool
  handler layer (P1.4 design), independent of SDK logging.
- **T7 layout:** confirmed `cmd/`+`internal/` layout for Phase 1 (see
  `phase1_plan.md` handoff).

## Deviations

- MCP Inspector + real-client smoke deferred to Phase 1 dogfooding (no GUI
  client available in this session). The in-process self-test exercises the
  same code paths (initialize handshake, tool call, elicitation retry).
- HTTP/SSE transport not exercised in code; SDK supports it with auth
  middleware hooks (verified by API inspection). P5.1 will implement.

## Decisions fed

- ADR-005 (D8): lock-file single instance confirmed.
- ADR-006 (D3): confirmation channels = client elicitation/approval (Tier 1
  only), in-band token (Tier 1), out-of-band surface (Tier 2+). Elicitation's
  new round-trip semantics documented above.
