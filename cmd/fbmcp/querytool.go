// fb_query — the Tier-0 read path: one SELECT / WITH-SELECT / EXECUTE
// PROCEDURE statement on an engine-enforced read-only transaction
// (dbpool.ReadOnly: read-only TPB on the RO-user pool). The statement's
// access plan (MON$EXPLAINED_PLAN, FB 5+) and execution statistics —
// MON$IO_STATS / MON$RECORD_STATS, per-table via MON$TABLE_STATS on FB 5+ —
// are read from the statement's own monitoring group while its cursor is
// open, and every call is logged to <state.dir>/query-log.jsonl (NDJSON).
// An EXECUTE PROCEDURE that the engine refuses on the read-only transaction
// (a mutating procedure) is routed into the fb_write gated flow.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/fbparse"
	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/qlog"
	"github.com/aleks/fbmcp/internal/queryplan"
	"github.com/aleks/fbmcp/internal/uigrid"
)

const (
	queryDefaultRows = 100
	queryHardRowCap  = 1000
	queryValueMax    = 4096
	queryTimeout     = 30 * time.Second
)

type readKind int

const (
	readNone   readKind = iota
	readSelect          // exactly one high-confidence SELECT / WITH-SELECT
	readProc            // exactly one EXECUTE PROCEDURE (may still mutate — the RO tx is the fuse)
)

// classifyRead is fail-closed: it accepts exactly one high-confidence
// SELECT/WITH-SELECT (fbparse.IsReadOnly — no WITH LOCK, no parser doubt)
// or one high-confidence EXECUTE PROCEDURE. Everything else must take the
// fb_write gated path.
func classifyRead(sqlText string) (readKind, string) {
	if strings.TrimSpace(sqlText) == "" {
		return readNone, "empty sql"
	}
	if fbparse.IsReadOnly(sqlText) {
		return readSelect, ""
	}
	stmts := fbparse.Parse(sqlText)
	if len(stmts) != 1 {
		return readNone, "fb_query accepts exactly one statement; use fb_write for scripts"
	}
	s := stmts[0]
	if s.Verb == fbparse.VerbExecuteProc && s.Confidence == fbparse.ConfidenceHigh && len(s.Issues) == 0 && !s.Flags.WithLock {
		return readProc, ""
	}
	return readNone, "fb_query accepts a single SELECT (WITH…SELECT) or EXECUTE PROCEDURE; use fb_write for mutations"
}

func clampQueryRows(n int) int {
	if n <= 0 {
		return queryDefaultRows
	}
	if n > queryHardRowCap {
		return queryHardRowCap
	}
	return n
}

// queryResult carries everything one fb_query call produced, for both the
// tool response and the query-log entry.
type queryResult struct {
	cols []string
	// colsMeta is name + mapped grid type per column (driver ColumnTypes);
	// it rides in structuredContent and drives the grid payload.
	colsMeta  []uigrid.Col
	rows      [][]string
	typedRows [][]any // driver-typed cells for the grid payload (nil = NULL)
	truncated bool
	elapsedMS float64
	engine    string
	stats     *qlog.Stats
	perTable  []qlog.PerTable
	plan      string
	planErr   string
	execErr   error
}

func registerQueryTool(server *mcp.Server, gt *gatedTools) {
	type queryArg struct {
		Db      string `json:"db"`
		SQL     string `json:"sql" jsonschema:"single SELECT / WITH-SELECT / EXECUTE PROCEDURE statement (no bind parameters)"`
		MaxRows int    `json:"max_rows,omitempty" jsonschema:"row cap (default 100, max 1000)"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_query", Description: "Tier 0: run one SELECT (or EXECUTE PROCEDURE) on an engine-enforced read-only transaction; returns rows, access plan and execution statistics (per-table on FB 5+); every call is logged to query-log.jsonl. A procedure the engine refuses on the RO transaction is routed to fb_write (confirmation required). Composing a new query? Validate drafts with fb_query_check first (compile-only, never executes)"}, func(ctx context.Context, req *mcp.CallToolRequest, a queryArg) (*mcp.CallToolResult, any, error) {
		id := identity.Caller(ctx)
		if _, err := gt.cfg.DB(a.Db); err != nil {
			return errText("DENIED: " + err.Error())
		}
		maxRows := clampQueryRows(a.MaxRows)
		kind, why := classifyRead(a.SQL)
		entry := qlog.Entry{Tool: "fb_query", Identity: id.Name, Database: a.Db, Query: a.SQL,
			Params: map[string]any{"max_rows": maxRows}}
		if kind == readNone {
			entry.Outcome, entry.Error = "denied", why
			_ = gt.qlog.Log(entry)
			gt.aud.Log(audit.Entry{Identity: id.Name, Database: a.Db, Tool: "fb_query", Tier: 0, Decision: "denied",
				Detail: map[string]interface{}{"reason": why}})
			return errText("DENIED: " + why)
		}

		res := gt.runReadQuery(ctx, a.Db, a.SQL, maxRows, kind)
		entry.Engine, entry.Rows, entry.Truncated, entry.ElapsedMS = res.engine, len(res.rows), res.truncated, res.elapsedMS
		entry.Stats, entry.PerTable, entry.Plan, entry.PlanError = res.stats, res.perTable, res.plan, res.planErr

		if res.execErr != nil {
			entry.Error = res.execErr.Error()
			if kind == readProc {
				// The engine refused the call on the read-only transaction
				// (typically a mutating procedure) — route it into the
				// fb_write gated flow; confirmation is still required.
				// Drain the read pool first: the refused procedure's
				// compiled body pins its target objects on that attachment
				// (observed live), which would block any DDL on them for
				// the pool's idle lifetime.
				gt.pools.CloseRead(a.Db)
				entry.Outcome = "fallback"
				_ = gt.qlog.Log(entry)
				gt.aud.Log(audit.Entry{Identity: id.Name, Database: a.Db, Tool: "fb_query", Tier: 0, Decision: "fallback",
					Detail: map[string]interface{}{"reason": res.execErr.Error(), "routed_to": "fb_write"}})
				msg, denied := gt.requestWrite(ctx, id, a.Db, a.SQL, false)
				if denied {
					return errText("read-only execution failed (" + res.execErr.Error() + ") — fb_write: " + msg)
				}
				return text("read-only execution failed (" + res.execErr.Error() + ") — routed to fb_write gated flow:\n" + msg), nil, nil
			}
			entry.Outcome = "error"
			_ = gt.qlog.Log(entry)
			gt.aud.Log(audit.Entry{Identity: id.Name, Database: a.Db, Tool: "fb_query", Tier: 0, Decision: "error",
				Detail: map[string]interface{}{"error": res.execErr.Error()}})
			return errText("error: " + res.execErr.Error())
		}

		entry.Outcome = "ok"
		_ = gt.qlog.Log(entry)
		gt.aud.Log(audit.Entry{Identity: id.Name, Database: a.Db, Tool: "fb_query", Tier: 0, Decision: "allow"})
		structured := map[string]any{
			"columns": res.colsMeta, "rows": res.rows, "row_count": len(res.rows), "truncated": res.truncated,
			"elapsed_ms": res.elapsedMS, "stats": res.stats, "per_table_stats": res.perTable, "plan": res.plan,
		}
		out := text(formatQueryResult(res))
		// Grid (phase10 uigrid): store the typed payload for the
		// ui://fbmcp/results/{id} resource and point Apps-capable clients
		// at it via _meta.ui. Additive: the text and the rest of
		// structuredContent are unchanged. Nil registry (grid disabled in
		// config or in tests) simply skips both.
		if gid, ok := gt.storeGrid(id.Name, a.Db, a.SQL, res); ok {
			structured["result_id"] = gid
			out.Meta = mcp.Meta(uigrid.MetaUI(uigrid.URI(gid)))
		}
		return out, structured, nil
	})
	registerQueryCheckTool(server, gt)
}

// storeGrid builds and stores the grid payload for a successful fb_query
// result. ok=false when the grid is disabled or the result has no columns
// (zero-column statements render no grid).
func (gt *gatedTools) storeGrid(idName, dbID, sqlText string, res queryResult) (string, bool) {
	p := uigrid.Gridify(gridTitle(dbID, sqlText), res.colsMeta, res.typedRows, uigrid.Options{
		Truncated: res.truncated, ElapsedMS: res.elapsedMS, Plan: res.plan, Stats: res.stats, PerTable: res.perTable,
	})
	if p == nil {
		return "", false
	}
	return gt.grid.Put(idName, p.Title, p)
}

// registerQueryCheckTool — fb_query_check (phase10): compile-only draft
// validation for the prompt-driven query builder. The statement is prepared
// on the read-only pool and closed — wire-verified, nakagami's prepare is
// opAllocateStatement + opPrepareStatement only, so nothing executes (no
// rows read, no selectable-procedure bodies run; QueryContext on an EXECUTE
// PROCEDURE would run the body, which is exactly why this tool exists).
// Column names/types are a post-execute fact (stdlib sql.Stmt exposes no
// column metadata), so they come from fb_query on the real run.
func registerQueryCheckTool(server *mcp.Server, gt *gatedTools) {
	type checkArg struct {
		Db  string `json:"db" jsonschema:"registry id of the database"`
		SQL string `json:"sql" jsonschema:"single SELECT / WITH-SELECT / EXECUTE PROCEDURE statement to compile-validate (no bind parameters; never executed)"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_query_check", Description: "Tier 0: compile-validate one SELECT / WITH-SELECT / EXECUTE PROCEDURE draft — prepares it on the engine-enforced read-only transaction and reports kind + bind-parameter count; the statement is NEVER executed (no rows touched, no procedure bodies run). Compile errors come back with a structured {code, hint, remediation} envelope. Column names/types are reported by fb_query once executed. Run drafts here before fb_query"}, func(ctx context.Context, req *mcp.CallToolRequest, a checkArg) (*mcp.CallToolResult, any, error) {
		id := identity.Caller(ctx)
		if _, err := gt.cfg.DB(a.Db); err != nil {
			return errText("DENIED: " + err.Error())
		}
		entry := qlog.Entry{Tool: "fb_query_check", Identity: id.Name, Database: a.Db, Query: a.SQL}
		kind, why := classifyRead(a.SQL)
		if kind == readNone {
			entry.Outcome, entry.Error = "denied", why
			_ = gt.qlog.Log(entry)
			gt.aud.Log(audit.Entry{Identity: id.Name, Database: a.Db, Tool: "fb_query_check", Tier: 0, Decision: "denied",
				Detail: map[string]interface{}{"reason": why}})
			return errText("DENIED: " + why)
		}
		// Phase-9 convention: Evaluate even at Tier 0 (MaxTier ceiling +
		// unknown-tool guard). Denials are audited like the gated path.
		if d := gt.eng.Evaluate(id, a.Db, "fb_query_check"); d.Outcome == "deny" {
			entry.Outcome, entry.Error = "denied", d.Reason
			_ = gt.qlog.Log(entry)
			gt.aud.Log(audit.Entry{Identity: id.Name, Database: a.Db, Tool: "fb_query_check", Tier: 0, Decision: "denied",
				Detail: map[string]interface{}{"reason": d.Reason}})
			return errText("DENIED: " + d.Reason)
		}

		tx, err := gt.pools.ReadOnly(ctx, a.Db)
		if err != nil {
			entry.Outcome, entry.Error = "error", err.Error()
			_ = gt.qlog.Log(entry)
			return errText("error: " + err.Error())
		}
		defer tx.Rollback()
		pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		start := time.Now()
		stmt, err := tx.PrepareContext(pctx, a.SQL)
		if err != nil {
			entry.Outcome, entry.Error = "error", err.Error()
			entry.ElapsedMS = elapsedMS(time.Since(start))
			_ = gt.qlog.Log(entry)
			gt.aud.Log(audit.Entry{Identity: id.Name, Database: a.Db, Tool: "fb_query_check", Tier: 0, Decision: "error",
				Detail: map[string]interface{}{"error": err.Error()}})
			// errText maps the driver message (which embeds the engine's
			// text + SQLCODE) onto the sql-compile / sql-object-missing seeds.
			return errText("compile error: " + err.Error())
		}
		stmt.Close()                // explicit drop (DSQL_drop): a failed-later prepare must not pin metadata locks
		n := countBindParams(a.SQL) // stdlib sql.Stmt exposes no NumInput; count '?' outside literals
		kindStr := "select"
		if kind == readProc {
			kindStr = "procedure"
		}
		notes := []string{
			"compile-only: prepared on the read-only transaction and never executed (no rows touched, no procedure bodies run)",
			"column names and types are reported by fb_query once the statement runs",
		}
		if n > 0 {
			notes = append(notes, "statement carries bind parameters — fb_query accepts none; inline literal values instead")
		}
		if n < 0 { // driver could not report; do not claim zero
			n = 0
			notes = append(notes, "bind-parameter count unavailable from the driver")
		}
		entry.Outcome = "ok"
		entry.ElapsedMS = elapsedMS(time.Since(start))
		_ = gt.qlog.Log(entry)
		gt.aud.Log(audit.Entry{Identity: id.Name, Database: a.Db, Tool: "fb_query_check", Tier: 0, Decision: "allow"})
		structured := map[string]any{"kind": kindStr, "bind_params": n, "notes": notes}
		var b strings.Builder
		fmt.Fprintf(&b, "ok: compiles on %s (prepare-only — never executed)\nkind: %s\nbind_params: %d\n", a.Db, kindStr, n)
		for _, nt := range notes {
			fmt.Fprintf(&b, "note: %s\n", nt)
		}
		return text(b.String()), structured, nil
	})
}

// runReadQuery executes the accepted statement on a read-only transaction
// and captures its monitoring rows (aggregate stats, per-table stats on
// FB 5+, explained plan on FB 5+) while the cursor is open — the statement
// group disappears from MON$ once the statement is released.
func (gt *gatedTools) runReadQuery(ctx context.Context, dbID, sqlText string, maxRows int, kind readKind) queryResult {
	var res queryResult
	if db, err := gt.cfg.DB(dbID); err == nil {
		if inst, err := gt.cfg.Instance(db.Instance); err == nil {
			res.engine = inst.Version
		}
	}
	tx, err := gt.pools.ReadOnly(ctx, dbID)
	if err != nil {
		res.execErr = err
		return res
	}
	defer tx.Rollback()
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	start := time.Now()
	// Explicit prepare + defer Close (DSQL_drop): the driver's Queryer
	// fast path leaks the compiled statement on an execute error
	// (closeStmtOnClose is only set on success), pinning metadata locks on
	// the pooled attachment until idle reaping. Going through a stdlib stmt
	// guarantees the drop, so a failed call (e.g. a mutating procedure
	// refused by the RO transaction) never blocks later DDL on its objects.
	stmt, err := tx.PrepareContext(qctx, sqlText)
	if err != nil {
		res.elapsedMS = elapsedMS(time.Since(start))
		res.execErr = err
		return res
	}
	defer stmt.Close()
	rows, err := stmt.QueryContext(qctx)
	if err != nil {
		res.elapsedMS = elapsedMS(time.Since(start))
		res.execErr = err
		return res
	}
	res.cols, _ = rows.Columns()
	colTypes, _ := rows.ColumnTypes() // the driver (IBSurgeon fork of nakagami v0.9.19) implements the RowsColumnType* set
	res.colsMeta = gridCols(colTypes)
	scanAny := make([]any, len(res.cols))
	scanPtr := make([]any, len(res.cols))
	for i := range scanAny {
		scanPtr[i] = &scanAny[i]
	}
	// The statement's MON$ group (aggregate stats, per-table counters on
	// FB 5+, explained plan) is guaranteed to exist while the cursor is
	// open, but is released asynchronously once the row set is exhausted —
	// capturing only after the loop is a race (observed live). Captures:
	// after the first scanned row (always valid), re-captured at the row
	// cap and after the loop; the last successful capture wins.
	captured := false
	capture := func() {
		gt.captureStmtStats(qctx, tx, dbID, sqlText, kind, &res)
		captured = res.stats != nil || len(res.perTable) > 0 || res.plan != ""
	}
	for rows.Next() {
		if err := rows.Scan(scanPtr...); err != nil {
			res.execErr = err
			break
		}
		res.rows = append(res.rows, stringifyRow(scanAny))
		res.typedRows = append(res.typedRows, typedRow(scanAny, colTypes))
		if len(res.rows) == 1 {
			capture()
		}
		if len(res.rows) >= maxRows {
			res.truncated = rows.Next() // one more row exists past the cap
			capture()
			break
		}
	}
	if err := rows.Err(); err != nil && res.execErr == nil {
		res.execErr = err
	}
	if !captured {
		capture() // zero-row results: the statement may still be visible
	}
	res.elapsedMS = elapsedMS(time.Since(start))
	rows.Close()
	return res
}

func elapsedMS(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}

func stringifyRow(vals []any) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		switch x := v.(type) {
		case nil:
			out[i] = "<null>"
		case []byte:
			out[i] = truncateVal(string(x))
		default:
			out[i] = truncateVal(fmt.Sprintf("%v", x))
		}
	}
	return out
}

// gridCols maps driver column metadata onto the grid type vocabulary
// (uigrid.MapType). Runs on the executing path only — column metadata
// needs an open cursor (stdlib sql.Stmt exposes none).
func gridCols(colTypes []*sql.ColumnType) []uigrid.Col {
	if len(colTypes) == 0 {
		return nil
	}
	out := make([]uigrid.Col, len(colTypes))
	for i, ct := range colTypes {
		_, scale, hasScale := ct.DecimalSize()
		out[i] = uigrid.Col{Name: ct.Name(), Type: uigrid.MapType(ct.DatabaseTypeName(), scale, hasScale)}
	}
	return out
}

// typedRow converts one row of scanned driver values to grid cells: nil →
// nil (JSON null), numerics/bools pass through, time formatted per the
// declared column type, BLOB bytes as a size marker. NUMERIC/DECIMAL stay
// strings (the driver scans them as strings; the column type drives
// rendering only). Cell clipping to the payload cap happens in uigrid.
func typedRow(vals []any, colTypes []*sql.ColumnType) []any {
	out := make([]any, len(vals))
	for i, v := range vals {
		if v == nil {
			continue // out[i] is already nil = NULL
		}
		tn := ""
		if i < len(colTypes) && colTypes[i] != nil {
			tn = strings.ToUpper(colTypes[i].DatabaseTypeName())
		}
		switch x := v.(type) {
		case string:
			out[i] = x
		case []byte:
			if tn == "BLOB" {
				out[i] = "<blob " + strconv.Itoa(len(x)) + " bytes>"
			} else {
				out[i] = string(x)
			}
		case time.Time:
			switch tn {
			case "DATE":
				out[i] = x.Format("2006-01-02")
			case "TIME", "TIME WITH TIMEZONE":
				out[i] = x.Format("15:04:05")
			default:
				out[i] = x.Format("2006-01-02 15:04:05.000")
			}
		default:
			out[i] = x // int64/int32/float64/bool and the rare rest as-is
		}
	}
	return out
}

// gridTitle is the payload title: "<db>: <sql, 120 chars>".
func gridTitle(dbID, sqlText string) string {
	s := strings.TrimSpace(sqlText)
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return dbID + ": " + s
}

// countBindParams counts '?' bind markers outside string literals (single
// or double quoted; ” inside a string is an escaped quote). db_statement
// dialect-1 double quotes are identifiers, not strings, but a '?' inside
// either quoting is never a bind marker, so both are skipped uniformly.
func countBindParams(sqlText string) int {
	n, inQuote, q := 0, false, byte(0)
	for i := 0; i < len(sqlText); i++ {
		c := sqlText[i]
		if inQuote {
			if c == q {
				if i+1 < len(sqlText) && sqlText[i+1] == q { // '' escape
					i++
				} else {
					inQuote = false
				}
			}
			continue
		}
		switch c {
		case '\'', '"':
			inQuote, q = true, c
		case '?':
			n++
		case '-':
			if i+1 < len(sqlText) && sqlText[i+1] == '-' { // line comment
				for i < len(sqlText) && sqlText[i] != '\n' {
					i++
				}
			}
		case '/':
			if i+1 < len(sqlText) && sqlText[i+1] == '*' { // block comment
				i += 2
				for i+1 < len(sqlText) && !(sqlText[i] == '*' && sqlText[i+1] == '/') {
					i++
				}
				i++
			}
		}
	}
	return n
}

func truncateVal(s string) string {
	if len(s) > queryValueMax {
		return s[:queryValueMax] + "…[truncated]"
	}
	return s
}

// ownStatementStatID finds the caller's open statement in MON$STATEMENTS —
// same attachment (CURRENT_CONNECTION), matching SQL text — and returns its
// stat-group id (0 = not found).
func ownStatementStatID(ctx context.Context, tx *sql.Tx, sqlText string) (int, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT MON$STAT_ID, MON$SQL_TEXT FROM MON$STATEMENTS WHERE MON$ATTACHMENT_ID = CURRENT_CONNECTION`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	want := strings.TrimSpace(strings.ToUpper(sqlText))
	for rows.Next() {
		var sid int
		var txt []byte
		if err := rows.Scan(&sid, &txt); err != nil {
			return 0, err
		}
		if strings.TrimSpace(strings.ToUpper(string(txt))) == want {
			return sid, nil
		}
	}
	return 0, rows.Err()
}

// captureStmtStats reads the statement's monitoring rows. Failures degrade:
// statistics stay empty (normal pre-5.0 for per-table), the plan falls back
// to the isql route (SELECT only) and the reason lands in PlanError.
func (gt *gatedTools) captureStmtStats(ctx context.Context, tx *sql.Tx, dbID, sqlText string, kind readKind, res *queryResult) {
	statID, err := ownStatementStatID(ctx, tx, sqlText)
	if err != nil || statID == 0 {
		if res.plan == "" { // a re-capture after a successful one keeps its findings
			res.planErr = "statement not visible in MON$STATEMENTS"
		}
		return
	}
	var st qlog.Stats
	err = tx.QueryRowContext(ctx, `SELECT IO.MON$PAGE_READS, IO.MON$PAGE_WRITES, IO.MON$PAGE_FETCHES, IO.MON$PAGE_MARKS,
		RS.MON$RECORD_SEQ_READS, RS.MON$RECORD_IDX_READS, RS.MON$RECORD_INSERTS, RS.MON$RECORD_UPDATES,
		RS.MON$RECORD_DELETES, RS.MON$RECORD_BACKOUTS, RS.MON$RECORD_PURGES, RS.MON$RECORD_EXPUNGES
		FROM MON$STATEMENTS S
		LEFT JOIN MON$IO_STATS IO ON IO.MON$STAT_ID = S.MON$STAT_ID
		LEFT JOIN MON$RECORD_STATS RS ON RS.MON$STAT_ID = S.MON$STAT_ID
		WHERE S.MON$STAT_ID = ?`, statID).Scan(
		&st.PageReads, &st.PageWrites, &st.PageFetches, &st.PageMarks,
		&st.SeqReads, &st.IdxReads, &st.Inserts, &st.Updates,
		&st.Deletes, &st.Backouts, &st.Purges, &st.Expunges)
	if err == nil {
		res.stats = &st
	}

	if gt.perTableSupported(dbID) {
		rows, err := tx.QueryContext(ctx, `SELECT TS.MON$TABLE_NAME,
			RS.MON$RECORD_SEQ_READS, RS.MON$RECORD_IDX_READS, RS.MON$RECORD_INSERTS, RS.MON$RECORD_UPDATES,
			RS.MON$RECORD_DELETES, RS.MON$RECORD_BACKOUTS, RS.MON$RECORD_PURGES, RS.MON$RECORD_EXPUNGES
			FROM MON$TABLE_STATS TS
			JOIN MON$RECORD_STATS RS ON RS.MON$STAT_ID = TS.MON$RECORD_STAT_ID
			WHERE TS.MON$STAT_ID = ?`, statID)
		if err != nil {
			// pre-5.0 engines have no MON$TABLE_STATS — remember per db
			if strings.Contains(err.Error(), "MON$TABLE_STATS") {
				gt.markNoPerTable(dbID)
			}
		} else {
			res.perTable = nil // re-capture replaces, never appends
			for rows.Next() {
				var t qlog.PerTable
				if err := rows.Scan(&t.Table, &t.SeqReads, &t.IdxReads, &t.Inserts, &t.Updates,
					&t.Deletes, &t.Backouts, &t.Purges, &t.Expunges); err != nil {
					break
				}
				res.perTable = append(res.perTable, t)
			}
			rows.Close()
			sort.Slice(res.perTable, func(i, j int) bool { return res.perTable[i].Table < res.perTable[j].Table })
		}
	}

	var plan []byte
	if err := tx.QueryRowContext(ctx,
		`SELECT MON$EXPLAINED_PLAN FROM MON$STATEMENTS WHERE MON$STAT_ID = ?`, statID).Scan(&plan); err == nil {
		res.plan = strings.TrimSpace(string(plan))
		if res.plan != "" {
			return
		}
		res.planErr = "empty MON$EXPLAINED_PLAN"
	} else {
		res.planErr = err.Error()
	}
	if kind == readSelect {
		if p := gt.planFallback(ctx, dbID, sqlText); p != "" {
			res.plan, res.planErr = p, ""
		}
	}
}

func (gt *gatedTools) perTableSupported(dbID string) bool {
	gt.mu.Lock()
	defer gt.mu.Unlock()
	return !gt.noPerTable[dbID]
}

func (gt *gatedTools) markNoPerTable(dbID string) {
	gt.mu.Lock()
	defer gt.mu.Unlock()
	if gt.noPerTable == nil {
		gt.noPerTable = map[string]bool{}
	}
	gt.noPerTable[dbID] = true
}

// planFallback serves engines without MON$EXPLAINED_PLAN (FB 3/4): the
// ADR-013 isql route with the read-only user's credentials, cached per
// (db, query) so repeated calls don't respawn the subprocess.
func (gt *gatedTools) planFallback(ctx context.Context, dbID, sqlText string) string {
	key := dbID + "\x00" + hashOf(sqlText)
	gt.mu.Lock()
	cached, ok := gt.planCache[key]
	gt.mu.Unlock()
	if ok {
		return cached
	}
	db, err := gt.cfg.DB(dbID)
	if err != nil {
		return ""
	}
	inst, err := gt.cfg.Instance(db.Instance)
	if err != nil {
		return ""
	}
	pass, err := config.SecretFromEnv(db.ROSecretEnv)
	if err != nil {
		return ""
	}
	pctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	plan, err := queryplan.ExplainAs(pctx, inst, db, db.ROUser, pass, sqlText, true)
	if err != nil || strings.TrimSpace(plan) == "" {
		return ""
	}
	gt.mu.Lock()
	if gt.planCache == nil {
		gt.planCache = map[string]string{}
	}
	if len(gt.planCache) > 256 { // simple bound; plans are advisory telemetry
		gt.planCache = map[string]string{}
	}
	gt.planCache[key] = plan
	gt.mu.Unlock()
	return plan
}

func formatQueryResult(res queryResult) string {
	var b strings.Builder
	if len(res.cols) == 0 {
		b.WriteString("no result set (statement completed)\n")
	} else {
		b.WriteString(strings.Join(res.cols, " | ") + "\n")
		for _, r := range res.rows {
			b.WriteString(strings.Join(r, " | ") + "\n")
		}
	}
	trim := ""
	if res.truncated {
		trim = fmt.Sprintf(" (truncated — raise max_rows, up to %d)", queryHardRowCap)
	}
	fmt.Fprintf(&b, "rows: %d%s\n", len(res.rows), trim)
	if res.plan != "" {
		b.WriteString("\nplan:\n" + res.plan + "\n")
	} else if res.planErr != "" {
		fmt.Fprintf(&b, "\nplan: unavailable (%s)\n", res.planErr)
	}
	if res.stats != nil {
		s := res.stats
		fmt.Fprintf(&b, "\nstats: page_reads=%d page_fetches=%d page_writes=%d seq_reads=%d idx_reads=%d\n",
			s.PageReads, s.PageFetches, s.PageWrites, s.SeqReads, s.IdxReads)
	}
	if len(res.perTable) > 0 {
		b.WriteString("per-table:\n")
		for _, t := range res.perTable {
			fmt.Fprintf(&b, "  %-32s seq=%-6d idx=%-6d ins=%-3d upd=%-3d del=%d\n",
				t.Table, t.SeqReads, t.IdxReads, t.Inserts, t.Updates, t.Deletes)
		}
	}
	fmt.Fprintf(&b, "elapsed: %.1f ms\n", res.elapsedMS)
	return b.String()
}
