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
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/fbparse"
	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/qlog"
	"github.com/aleks/fbmcp/internal/queryplan"
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
	cols      []string
	rows      [][]string
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
	mcp.AddTool(server, &mcp.Tool{Name: "fb_query", Description: "Tier 0: run one SELECT (or EXECUTE PROCEDURE) on an engine-enforced read-only transaction; returns rows, access plan and execution statistics (per-table on FB 5+); every call is logged to query-log.jsonl. A procedure the engine refuses on the RO transaction is routed to fb_write (confirmation required)"}, func(ctx context.Context, req *mcp.CallToolRequest, a queryArg) (*mcp.CallToolResult, any, error) {
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
			"columns": res.cols, "rows": res.rows, "row_count": len(res.rows), "truncated": res.truncated,
			"elapsed_ms": res.elapsedMS, "stats": res.stats, "per_table_stats": res.perTable, "plan": res.plan,
		}
		return text(formatQueryResult(res)), structured, nil
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
