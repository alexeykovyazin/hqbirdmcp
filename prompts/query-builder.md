# Query-builder playbook

Prompt-driven composition loop: turn the user's information need into one
validated read statement, then show it. Reads only — mutations are never
composed here.

1. **Intent** — restate the need in one sentence: entities, filters, shape
   (rows vs. aggregate), ordering. Confirm before querying when any part is
   ambiguous.
2. **Facts** — `fb_schema_list` → `fb_describe` every object the query will
   touch. Never invent a table or column. If the need smells like "find the
   slow part", `fb_analyze_query` a candidate first (plan before rows).
3. **Draft** — compose exactly one statement: a `SELECT` / `WITH…SELECT`
   (or `EXECUTE PROCEDURE`). No bind parameters — inline literal values.
   Explicit `ORDER BY` so the result grid sorts deterministically.
4. **Check** — `fb_query_check` the draft. It compiles on the engine's
   read-only path without executing. On a structured error
   (`sql-compile` / `sql-object-missing`): fix and re-check. Cheap loop —
   prefer it over execute-and-fail.
5. **Execute** — `fb_query` (state the row cap to the user: default 100,
   max 1000). The result renders as a grid in chat; the plan and stats
   panels explain *how* the rows were produced.
6. **Guardrails** — reads only: any INSERT/UPDATE/DELETE/DDL goes through
   `fb_write` preview → confirm, never composed here. Never widen a filter
   "to see if data appears" without saying so. Bulk data needs the grid's
   copy/save — not repeated `max_rows` escalations.
