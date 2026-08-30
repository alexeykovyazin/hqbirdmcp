// fb_index_advice (C.2, closes the phase5-gap-notes row): analyze one
// SELECT's access plan, find natural scans with sargable predicates, and
// propose CREATE INDEX DDL — applied only via fb_write (no auto-create).
// recheck_of re-plans the query recorded in a previous advisory and reports
// which natural scans the human-applied index resolved.
package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/dbpool"
	"github.com/aleks/fbmcp/internal/fbparse"
	"github.com/aleks/fbmcp/internal/idxadvice"
	"github.com/aleks/fbmcp/internal/queryplan"
	"github.com/aleks/fbmcp/internal/state"
)

const adviceRowTimeout = 10 * time.Second

func registerIndexAdviceTool(server *mcp.Server, cfg *config.Handle, pools *dbpool.Manager, aud *audit.Logger, st *state.Store) {
	type adviceArg struct {
		Db        string `json:"db" jsonschema:"registry id of the database"`
		Query     string `json:"query,omitempty" jsonschema:"a single SELECT statement to analyze (required unless recheck_of)"`
		RecheckOf string `json:"recheck_of,omitempty" jsonschema:"advisory id from a previous fb_index_advice run: re-plan its recorded query and report which natural scans were resolved"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_index_advice", Description: "Tier 0: analyze a SELECT's access plan and propose CREATE INDEX DDL for natural (full) table scans with sargable predicates — estimate-only benefit (Firebird cannot simulate an uncreated index); DDL is never applied here, run it via fb_write; recheck_of re-plans a previously advised query and reports the delta"}, func(ctx context.Context, req *mcp.CallToolRequest, a adviceArg) (*mcp.CallToolResult, any, error) {
		dbCfg, err := cfg.DB(a.Db)
		if err != nil {
			return errText("error: " + err.Error())
		}
		inst, err := cfg.Instance(dbCfg.Instance)
		if err != nil {
			return errText("error: " + err.Error())
		}

		query := a.Query
		if a.RecheckOf != "" {
			if st == nil {
				return errText("error: advisory store unavailable")
			}
			adv, ok := st.Advisory(a.RecheckOf)
			if !ok {
				return errText("error: unknown advisory_id " + a.RecheckOf)
			}
			if adv.Tool != "fb_index_advice" || adv.Database != a.Db {
				return errText(fmt.Sprintf("error: advisory %s is not an fb_index_advice record for %s", a.RecheckOf, a.Db))
			}
			q, _ := adv.Detail["query"].(string)
			if q == "" {
				return errText("error: advisory predates recheck support (no recorded query); re-run advice")
			}
			if query == "" {
				query = q
			}
		}
		if strings.TrimSpace(query) == "" {
			return errText("error: query is required (unless recheck_of supplies a recorded one)")
		}

		// single high-confidence SELECT, no WITH LOCK — same shape fb_query
		// accepts, but strictly VerbSelect (plans for mutations are out of
		// scope here; fb_analyze_query covers raw plan retrieval)
		stmts := fbparse.Parse(query)
		if len(stmts) != 1 || stmts[0].Verb != fbparse.VerbSelect || stmts[0].Confidence != fbparse.ConfidenceHigh || len(stmts[0].Issues) != 0 || stmts[0].Flags.WithLock {
			return errText("error: fb_index_advice analyzes exactly one plain SELECT (no WITH LOCK; use fb_analyze_query for other shapes)")
		}

		pass, err := config.SecretFromEnv(dbCfg.AdminSecretEnv)
		if err != nil {
			return errText("error: " + err.Error())
		}
		planText, err := queryplan.Explain(ctx, inst, dbCfg, pass, query, false)
		if err != nil {
			return errText("error: " + err.Error())
		}
		planNode, err := idxadvice.ParsePlan(planText)
		if err != nil {
			return errText("error: unparseable plan: " + err.Error())
		}

		var scans []string
		var walk func(*idxadvice.Node)
		walk = func(n *idxadvice.Node) {
			if n == nil {
				return
			}
			if n.Kind == idxadvice.KindScan && len(n.Indexes) == 0 {
				scans = append(scans, n.Table)
			}
			for _, c := range n.Children {
				walk(c)
			}
		}
		walk(planNode)

		existing := loadExistingIndexes(ctx, pools, a.Db, scans)
		rowsFn := tableRowCount(ctx, pools, a.Db, scans)

		res := idxadvice.Analyze(query, planNode, existing, rowsFn)

		var b strings.Builder
		fmt.Fprintf(&b, "plan: %s\n", strings.TrimSpace(planText))
		if a.RecheckOf != "" {
			adv, _ := st.Advisory(a.RecheckOf)
			var before []string
			switch raw := adv.Detail["natural_scans"].(type) {
			case []string: // in-memory store (same process)
				before = raw
			case []any: // reloaded from state.json
				for _, v := range raw {
					if s, ok := v.(string); ok {
						before = append(before, s)
					}
				}
			}
			fmt.Fprintf(&b, "\nrecheck of %s (advised %s):\n", a.RecheckOf, adv.CreatedAt.Format(time.RFC3339))
			for _, old := range before {
				if containsFold(scans, old) {
					fmt.Fprintf(&b, "  UNRESOLVED: %s still scanned naturally\n", old)
				} else {
					fmt.Fprintf(&b, "  resolved: %s no longer scanned naturally\n", old)
				}
			}
			if len(before) == 0 {
				b.WriteString("  (no natural scans were recorded in the advisory)\n")
			}
		}

		structured := map[string]any{
			"plan": strings.TrimSpace(planText), "natural_scans": res.Scans, "sorts_over_scans": res.Sorts,
			"advice": res.Advice, "notes": res.Notes,
		}
		if len(res.Advice) == 0 {
			b.WriteString("\nno index advice\n")
			for _, n := range res.Notes {
				fmt.Fprintf(&b, "- %s\n", n)
			}
			aud.Log(audit.Entry{Identity: "local", Database: a.Db, Tool: "fb_index_advice", Tier: 0, Decision: "allow",
				Detail: map[string]interface{}{"advice_count": 0, "recheck_of": a.RecheckOf}})
			return text(b.String()), structured, nil
		}

		for i := range res.Advice {
			adv := &res.Advice[i]
			id := fmt.Sprintf("adv%d", time.Now().UnixNano())
			if st != nil {
				_ = st.PutAdvisory(state.Advisory{
					ID: id, Database: a.Db, Tool: "fb_index_advice", Object: adv.DDL,
					Reason:    fmt.Sprintf("%s; columns %v (%s)", adv.Reason, adv.Columns, adv.Kind),
					CreatedAt: time.Now().UTC(),
					Detail: map[string]any{
						"query": query, "natural_scans": res.Scans, "ddl": adv.DDL,
						"columns": adv.Columns, "kind": adv.Kind, "estimate": adv.Estimate,
					},
				})
			}
			adv.AdvisoryID = id
			fmt.Fprintf(&b, "\nADVISORY id=%s\n  finding: %s\n  columns: %s (%s)\n  %s\n  DDL (apply via fb_write — never applied here):\n    %s\n",
				id, adv.Reason, strings.Join(adv.Columns, ", "), adv.Kind, adv.Estimate, adv.DDL)
			if adv.SortNote != "" {
				fmt.Fprintf(&b, "  note: %s\n", adv.SortNote)
			}
		}
		b.WriteString("\nrisk: every new index adds write overhead and maintenance; check fb_index_stats for near-duplicates before applying; estimates assume column distinctness, they are not measured\n")
		aud.Log(audit.Entry{Identity: "local", Database: a.Db, Tool: "fb_index_advice", Tier: 0, Decision: "allow",
			Detail: map[string]interface{}{"advice_count": len(res.Advice), "recheck_of": a.RecheckOf}})
		return text(b.String()), structured, nil
	})
}

// loadExistingIndexes reads index definitions for the scanned tables so the
// analysis can suppress advice already covered by an existing index.
// Segment order comes from RDB$SEGMENT_SEQUENCE (LIST() order is undefined).
func loadExistingIndexes(ctx context.Context, pools *dbpool.Manager, db string, tables []string) []idxadvice.IndexDef {
	if len(tables) == 0 {
		return nil
	}
	tx, err := pools.ReadOnly(ctx, db)
	if err != nil {
		return nil // degrade: analysis proceeds without coverage suppression
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT CAST(i.RDB$RELATION_NAME AS VARCHAR(66)), CAST(i.RDB$INDEX_NAME AS VARCHAR(66)),
		       CAST(s.RDB$FIELD_NAME AS VARCHAR(66)), s.RDB$SEGMENT_SEQUENCE
		FROM RDB$INDICES i
		JOIN RDB$INDEX_SEGMENTS s ON s.RDB$INDEX_NAME = i.RDB$INDEX_NAME
		WHERE i.RDB$SYSTEM_FLAG = 0`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	want := map[string]bool{}
	for _, t := range tables {
		want[strings.ToUpper(t)] = true
	}
	type seg struct {
		table, name, col string
		seq              int
	}
	var segs []seg
	for rows.Next() {
		var s seg
		if err := rows.Scan(&s.table, &s.name, &s.col, &s.seq); err != nil {
			return nil
		}
		s.table, s.name, s.col = strings.TrimSpace(s.table), strings.TrimSpace(s.name), strings.TrimSpace(s.col)
		segs = append(segs, s)
	}
	sort.Slice(segs, func(i, j int) bool {
		if segs[i].name != segs[j].name {
			return segs[i].name < segs[j].name
		}
		return segs[i].seq < segs[j].seq
	})
	var out []idxadvice.IndexDef
	for _, s := range segs {
		if !want[strings.ToUpper(s.table)] {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Name == s.name {
			out[n-1].Columns = append(out[n-1].Columns, s.col)
			continue
		}
		out = append(out, idxadvice.IndexDef{Table: strings.ToUpper(s.table), Name: s.name, Columns: []string{s.col}})
	}
	return out
}

// tableRowCount counts rows per scanned table on the RO pool (bounded); any
// failure degrades that table to "unknown" — the estimate says so.
func tableRowCount(ctx context.Context, pools *dbpool.Manager, db string, tables []string) idxadvice.RowsFn {
	counts := map[string]int64{}
	return func(table string) (int64, bool) {
		if c, ok := counts[table]; ok {
			return c, c > 0
		}
		if !safeIdent(table) {
			counts[table] = 0
			return 0, false
		}
		tx, err := pools.ReadOnly(ctx, db)
		if err != nil {
			return 0, false
		}
		defer tx.Rollback()
		qctx, cancel := context.WithTimeout(ctx, adviceRowTimeout)
		defer cancel()
		var n int64
		if err := tx.QueryRowContext(qctx, fmt.Sprintf(`SELECT COUNT(*) FROM "%s"`, table)).Scan(&n); err != nil {
			counts[table] = 0
			return 0, false
		}
		counts[table] = n
		return n, n > 0
	}
}

func safeIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' || c == '$' || (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			continue
		}
		return false
	}
	return true
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}
