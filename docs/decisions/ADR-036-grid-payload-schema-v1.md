# ADR-036: Grid payload schema v1

Status: accepted (2026-09-16, phase 10)

## Context

The result grid (ADR-035) and the planned Plamenix grid bundle (companion
plan §5.4/§7.4) must be able to consume each other's payloads, and fbmcp
needs a stable wire shape for the inlined `window.__GRID_DATA__`.

## Decision

Schema version 1 (`internal/uigrid.Payload`, `schema: 1`):

```jsonc
{
  "schema": 1,
  "title": "<db>: <sql, 120 chars>",
  "columns": [{"name": "EMP_NO", "type": "integer"}],
  "rows": [[7360, "Smith", null]],       // typed cells
  "row_count": 5,
  "truncated": false,                     // max_rows cut it
  "clipped_rows": 0,                      // byte-cap dropped rows
  "elapsed_ms": 16.4,
  "plan": "…", "stats": {}, "per_table_stats": []
}
```

- **Type vocabulary** (column `type`): `integer | decimal | float |
  string | date | time | timestamp | boolean | blob | unknown` — one
  mapping table (`uigrid.MapType`), grounded in
  nakagami/firebirdsql v0.9.19 `xsqlvar.go`: NUMERIC/DECIMAL arrive as
  SHORT/LONG/INT64/INT128 with negative scale and scan as **strings**;
  the type drives alignment only, values are never re-typed.
- **Cells**: JSON `null` (NULL), int64/float64/bool, or string (dates
  pre-formatted; BLOBs → `"<blob n bytes>"`; cell strings clipped to 256
  chars with a marker). Full values stay in the tool's
  `structuredContent` — the grid clips, the AI doesn't.
- **Driver type metadata is a post-execute fact** (stdlib `sql.Stmt`
  exposes no column metadata), so `fb_query_check` reports
  `{kind, bind_params}` only; types appear with `fb_query`'s real run.

## Consequences

- Any server (fbmcp today, Plamenix later) can embed either bundle as
  long as it emits this schema; a version bump is a coordinated change
  recorded here.
- The 2 MiB byte cap drops trailing rows server-side and reports
  `clipped_rows`; over-cap zero-row payloads still serve.
