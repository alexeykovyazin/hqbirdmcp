# ADR-035: Result grid via MCP Apps UI resources

Status: accepted (2026-09-16, phase 10; client matrix pending the manual
Phase 0 spike)

## Context

`fb_query` returned pipe-delimited text plus a stringified
`structuredContent`: a wall of characters in the chat. The plan
([`phase10_plan.md`](../../../phase10_plan.md)) adds a result-set grid for
MCP-Apps clients while keeping every existing view intact. go-sdk v1.7.0
supports what's needed: `CallToolResult.Meta` is a plain map (`_meta`) and
`Server.AddResourceTemplate` exists.

## Decision

1. **`ui://fbmcp/results/{result_id}` resource template** served by the
   same server (stdio and HTTP/SSE — the client fetches resources through
   its own MCP session, so ADR-022 auth already guards remote mode). The
   body is one embedded self-contained HTML file (strict CSP, no external
   requests) with the payload inlined as `window.__GRID_DATA__` (`<`-
   escaped against `</script>` breakout). It is fbmcp's first *template*,
   dynamic, identity-scoped, `text/html` resource — the four kernel
   resources (`fbmcp://*`, phase 8 D3.3) are static text.
2. **`_meta.ui.resourceUri` + additive structured content.** A successful
   `fb_query` with a result set gains `result_id` and typed
   `columns` (`{name, type}`); `rows` stays `[][]string` and the text view
   stays byte-identical (golden-locked). Typed cells live only in the
   resource payload, which has no compatibility surface.
3. **In-memory, identity-scoped registry.** Random 128-bit ids, 30-min
   TTL, ≤ 32 results per identity, 2 MiB byte cap (drops trailing rows,
   banner says so). Nothing persisted — the chaos harness stays
   kill-neutral, `state.json` untouched. Cross-identity reads are denied
   (`result-forbidden`); unknown/expired ids return `result-expired`
   (both errdoc seeds).
4. **Fallback first-class.** Non-Apps clients render the existing
   `structuredContent`; no server-side capability detection. The grid is
   a progressive enhancement.
5. **Grid adds no authority** — Tier-0 rendering of data the client
   already holds; `fb_query`'s RO transaction, qlog, and audit lines are
   untouched. Config: `grid: {enabled, ttl_minutes, max_live}`
   (`enabled` defaults on; `false` removes the template and `_meta.ui`).

## Consequences

- The bundle is pure Go (`go:embed`), no node toolchain in fbmcp's build.
- Per-client `_meta.ui` support varies and the spec is young; one
  constructor (`uigrid.MetaUI`) is the single place to update, and the
  client matrix lives in [`../grid.md`](../grid.md).
- The Plamenix grid bundle (companion plan §5.4/§7.4) can later replace
  the embedded bundle without touching the tool layer — the payload
  schema (ADR-036) is the contract.
