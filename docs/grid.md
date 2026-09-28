# Result grid in the AI chat (phase 10)

`fb_query` results render as a **sortable grid inside the chat** on
MCP-Apps clients, via a `ui://fbmcp/results/<result_id>` HTML resource
referenced from the tool result's `_meta.ui`. Plan:
[`phase10_plan.md`](../../../phase10_plan.md); ADRs 035 (UI resources) and
036 (payload schema).

## How it works

1. A successful `fb_query` with a result set builds a typed payload
   (`internal/uigrid`, `Gridify`) from the driver's column metadata and
   the scanned values: JSON `null` for NULL, numbers as numbers, dates
   formatted per the declared column type, BLOBs as `"<blob n bytes>"`.
2. The payload goes into an **in-memory, identity-scoped registry** —
   random 128-bit ids, TTL (default 30 min), ≤ 32 live results per
   identity (oldest evicted), 2 MiB byte cap (over-cap payloads drop
   trailing rows and the grid shows a "clipped" banner). Nothing is
   persisted; a kernel restart clears results (the client re-runs the
   query).
3. The tool result gains `result_id` + `columns[].type` (additive — the
   text view and the `rows` shape are unchanged) and
   `_meta.ui.resourceUri`.
4. Apps-capable clients fetch the resource and render the embedded
   single-file grid (sandboxed iframe, strict CSP, zero external
   requests, pure renderer — no tool calls from the grid). Other clients
   render `structuredContent` as before — the fallback is first-class.

`fb_query_check` completes the loop: it **compile-validates a draft**
(prepare on the read-only transaction, never executes — a selectable
procedure body would run under QueryContext, which is why the check tool
never calls it) and returns `{kind, bind_params}` or a structured
compile-error envelope (`sql-compile` / `sql-object-missing`). Compose
drafts with the `query-builder` prompt (playbook, auto-served as an MCP
prompt): intent → schema facts → draft → check → execute.

## Config (`fbmcp.yaml`)

```yaml
grid:
  enabled: true        # default; false removes the template + _meta.ui
  ttl_minutes: 30      # result fetchable window
  max_live: 32         # stored results per identity, oldest evicted
```

## Security

- Tier 0 rendering of data the client already holds — no new authority;
  `fb_query`'s RO-transaction, qlog and audit lines are untouched.
- Registry reads are **identity-scoped**: a result fetched under another
  identity is denied (`result-forbidden`); unknown/expired ids return
  `result-expired` (both are errdoc seeds).
- The bundle's CSP is `default-src 'none'; style-src 'unsafe-inline';
  script-src 'unsafe-inline'`; the payload is inlined `<`-escaped so no
  `</script>` sequence in the data can break out. Tests assert the bundle
  contains no external URL, no `eval`/`Function`/`fetch`.

## Client support matrix

| Client | `_meta.ui` fetch + iframe render | Fallback |
|---|---|---|
| Claude Desktop | to verify (phase 0 spike, manual) | structuredContent table |
| ZCode | to verify (phase 0 spike, manual) | structuredContent table |
| VS Code | to verify (tertiary) | structuredContent table |

The fallback path is the tool's existing `structuredContent`; the grid is
a progressive enhancement, never a blocker. Fill the matrix from the
manual spike and note per-client behavior here.

## Tests

- `internal/uigrid`: registry TTL / per-identity cap / sweep, forbidden +
  expired errors, cell clip, byte-cap row drop, type mapping table
  (`MapType`, grounded in the IBSurgeon fork of nakagami/firebirdsql
  v0.9.19 — `go.mod` replace — `xsqlvar.go`),
  render escaping, CSP / no-external-reference assertions.
- `cmd/fbmcp` (`grid_test.go`, hermetic): `formatQueryResult` golden
  (text path byte-locked), `countBindParams`, `typedRow` basics,
  `storeGrid` incl. nil-registry + zero-column skip, the MCP
  `resources/read` end-to-end (first in the tree), disabled-grid clean-off,
  `fb_query_check` denial envelope.
- Live: `FBMCP_GRID_LIVE=1 go test ./cmd/fbmcp -run GridLive -v` — full
  loop against the dev-host employee DB (query → grid resource → check
  valid/typo'd drafts).
