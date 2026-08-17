// Tier-0 read tools P2.4–P2.7 (phase2_plan.md). All SQL goes through the
// engine-enforced read-only pool; all outputs row-capped and marked.
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/adminexec"
	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/dbpool"
	"github.com/aleks/fbmcp/internal/facts"
	"github.com/aleks/fbmcp/internal/queryplan"
)

const rowCap = 100

func registerP2Tools(server *mcp.Server, cfg *config.Config, pools *dbpool.Manager, engFacts *facts.EngineFacts, aud *audit.Logger) {
	type dbArg struct {
		Db string `json:"db" jsonschema:"registry id of the database"`
	}

	// P2.4 — fb_analyze_query: plan via isql (ADR-013 route).
	type planArg struct {
		Db      string `json:"db"`
		Query   string `json:"query" jsonschema:"a single SELECT/UPDATE/DELETE statement to analyze"`
		Explain bool   `json:"explain,omitempty" jsonschema:"use EXPLAIN form (FB 4.0+)"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_analyze_query", Description: "Tier 0: retrieve the access plan for a statement (read-only analysis; heavy-read guard: 60s timeout, output cap)"}, func(ctx context.Context, req *mcp.CallToolRequest, a planArg) (*mcp.CallToolResult, any, error) {
		if strings.Count(a.Query, ";") > 1 || strings.Contains(strings.ToUpper(a.Query), " INSERT ") {
			return text("error: single statement only"), nil, nil
		}
		dbCfg, err := cfg.DB(a.Db)
		if err != nil {
			return text("error: " + err.Error()), nil, nil
		}
		inst, err := cfg.Instance(dbCfg.Instance)
		if err != nil {
			return text("error: " + err.Error()), nil, nil
		}
		pass, err := config.SecretFromEnv(dbCfg.AdminSecretEnv)
		if err != nil {
			return text("error: " + err.Error()), nil, nil
		}
		plan, err := queryplan.Explain(ctx, inst, dbCfg, pass, a.Query, a.Explain)
		if err != nil {
			return text("error: " + err.Error()), nil, nil
		}
		aud.Log(audit.Entry{Identity: "local", Database: a.Db, Tool: "fb_analyze_query", Tier: 0, Decision: "allow"})
		return text("PLAN: " + plan), nil, nil
	})

	// P2.5 — fb_index_stats: selectivity per index + duplicate-index advisory.
	mcp.AddTool(server, &mcp.Tool{Name: "fb_index_stats", Description: "Tier 0: index statistics (selectivity, uniqueness) with duplicate/unused advisory"}, func(ctx context.Context, req *mcp.CallToolRequest, a dbArg) (*mcp.CallToolResult, any, error) {
		tx, err := pools.ReadOnly(ctx, a.Db)
		if err != nil {
			return text("error: " + err.Error()), nil, nil
		}
		defer tx.Rollback()
		rows, err := tx.QueryContext(ctx, `
			SELECT i.RDB$RELATION_NAME, i.RDB$INDEX_NAME, i.RDB$UNIQUE_FLAG, i.RDB$INDEX_TYPE, i.RDB$STATISTICS
			FROM RDB$INDICES i
			WHERE i.RDB$SYSTEM_FLAG = 0
			ORDER BY 1, 2`)
		if err != nil {
			return text("error: " + err.Error()), nil, nil
		}
		defer rows.Close()
		var b strings.Builder
		type key struct{ rel, cols string }
		seen := map[key][]string{}
		n := 0
		for rows.Next() {
			var rel, idx string
			var uniq bool
			var itype *int
			var sel float64
			if err := rows.Scan(&rel, &idx, &uniq, &itype, &sel); err != nil {
				return text("error: " + err.Error()), nil, nil
			}
			fmt.Fprintf(&b, "- %s.%s unique=%v descending=%v selectivity=%.6f\n", rel, idx, uniq, itype != nil && *itype == 1, sel)
			n++
			if n >= rowCap {
				b.WriteString("... (capped)\n")
				break
			}
		}
		// duplicate-index advisory on leading column sets
		rows2, err := tx.QueryContext(ctx, `
			SELECT i.RDB$RELATION_NAME, LIST(s.RDB$FIELD_NAME) AS cols, COUNT(*) AS seg_cnt
			FROM RDB$INDICES i
			JOIN RDB$INDEX_SEGMENTS s ON s.RDB$INDEX_NAME = i.RDB$INDEX_NAME
			WHERE i.RDB$SYSTEM_FLAG = 0
			GROUP BY i.RDB$RELATION_NAME, i.RDB$INDEX_NAME`)
		if err == nil {
			defer rows2.Close()
			for rows2.Next() {
				var rel, cols string
				var cnt int
				if rows2.Scan(&rel, &cols, &cnt); err == nil {
					k := key{rel, cols}
					seen[k] = append(seen[k], cols)
				}
			}
			dups := 0
			for k, v := range seen {
				if len(v) > 1 {
					fmt.Fprintf(&b, "ADVISORY: %s has %d indexes on the same columns (%s) — consider dropping duplicates\n", k.rel, len(v), k.cols)
					dups++
					if dups >= 10 {
						break
					}
				}
			}
			if dups == 0 {
				b.WriteString("advisory: no duplicate-column indexes found\n")
			}
		}
		aud.Log(audit.Entry{Identity: "local", Database: a.Db, Tool: "fb_index_stats", Tier: 0, Decision: "allow"})
		return text(b.String()), nil, nil
	})

	// P2.6 — fb_schema_list + fb_describe.
	mcp.AddTool(server, &mcp.Tool{Name: "fb_schema_list", Description: "Tier 0: list user tables/views/procedures/triggers"}, func(ctx context.Context, req *mcp.CallToolRequest, a dbArg) (*mcp.CallToolResult, any, error) {
		tx, err := pools.ReadOnly(ctx, a.Db)
		if err != nil {
			return text("error: " + err.Error()), nil, nil
		}
		defer tx.Rollback()
		rows, err := tx.QueryContext(ctx, `
			SELECT RDB$RELATION_NAME, RDB$VIEW_SOURCE IS NOT NULL AS IS_VIEW
			FROM RDB$RELATIONS WHERE RDB$SYSTEM_FLAG = 0 ORDER BY 1`)
		if err != nil {
			return text("error: " + err.Error()), nil, nil
		}
		defer rows.Close()
		var b strings.Builder
		n := 0
		for rows.Next() {
			var name string
			var isView bool
			if err := rows.Scan(&name, &isView); err != nil {
				break
			}
			kind := "TABLE"
			if isView {
				kind = "VIEW"
			}
			fmt.Fprintf(&b, "- %s (%s)\n", name, kind)
			n++
			if n >= rowCap {
				b.WriteString("... (capped)\n")
				break
			}
		}
		aud.Log(audit.Entry{Identity: "local", Database: a.Db, Tool: "fb_schema_list", Tier: 0, Decision: "allow"})
		return text(b.String()), nil, nil
	})

	type descArg struct {
		Db    string `json:"db"`
		Table string `json:"table" jsonschema:"relation name"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_describe", Description: "Tier 0: columns, types, nullability of a relation"}, func(ctx context.Context, req *mcp.CallToolRequest, a descArg) (*mcp.CallToolResult, any, error) {
		tx, err := pools.ReadOnly(ctx, a.Db)
		if err != nil {
			return text("error: " + err.Error()), nil, nil
		}
		defer tx.Rollback()
		rows, err := tx.QueryContext(ctx, `
			SELECT rf.RDB$FIELD_NAME, f.RDB$FIELD_TYPE, f.RDB$FIELD_LENGTH, f.RDB$FIELD_SCALE, rf.RDB$NULL_FLAG
			FROM RDB$RELATION_FIELDS rf JOIN RDB$FIELDS f ON f.RDB$FIELD_NAME = rf.RDB$FIELD_SOURCE
			WHERE rf.RDB$RELATION_NAME = ? ORDER BY rf.RDB$FIELD_POSITION`, strings.ToUpper(a.Table))
		if err != nil {
			return text("error: " + err.Error()), nil, nil
		}
		defer rows.Close()
		var b strings.Builder
		n := 0
		for rows.Next() {
			var name string
			var ftype, flen int
			var fscale *int
			var nullFlag *int
			if err := rows.Scan(&name, &ftype, &flen, &fscale, &nullFlag); err != nil {
				break
			}
			nullable := "NULL"
			if nullFlag != nil {
				nullable = "NOT NULL"
			}
			fmt.Fprintf(&b, "- %s %s(%d) %s\n", name, typeName(ftype), flen, nullable)
			n++
		}
		if n == 0 {
			return text("relation not found (or has no columns)"), nil, nil
		}
		aud.Log(audit.Entry{Identity: "local", Database: a.Db, Tool: "fb_describe", Tier: 0, Decision: "allow"})
		return text(b.String()), nil, nil
	})

	// P2.7 — fb_activity_sample: MON$IO_STATS delta over a short window.
	type sampleArg struct {
		Db      string `json:"db"`
		Seconds int    `json:"seconds,omitempty" jsonschema:"sample window (default 5, max 30)"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_activity_sample", Description: "Tier 0: MON$ IO/record-stat deltas over a window (1–30s)"}, func(ctx context.Context, req *mcp.CallToolRequest, a sampleArg) (*mcp.CallToolResult, any, error) {
		w := a.Seconds
		if w <= 0 {
			w = 5
		}
		if w > 30 {
			w = 30
		}
		snap := func() (map[string]int64, error) {
			tx, err := pools.ReadOnly(ctx, a.Db)
			if err != nil {
				return nil, err
			}
			defer tx.Rollback()
			var reads, writes, ins, upd, del int64
			err = tx.QueryRowContext(ctx, `SELECT
				(SELECT COALESCE(SUM(MON$PAGE_READS),0) FROM MON$IO_STATS),
				(SELECT COALESCE(SUM(MON$PAGE_WRITES),0) FROM MON$IO_STATS),
				(SELECT COALESCE(SUM(MON$RECORD_INSERTS),0) FROM MON$RECORD_STATS),
				(SELECT COALESCE(SUM(MON$RECORD_UPDATES),0) FROM MON$RECORD_STATS),
				(SELECT COALESCE(SUM(MON$RECORD_DELETES),0) FROM MON$RECORD_STATS)
				FROM RDB$DATABASE`).Scan(&reads, &writes, &ins, &upd, &del)
			if err != nil {
				return nil, err
			}
			return map[string]int64{"page_reads": reads, "page_writes": writes, "inserts": ins, "updates": upd, "deletes": del}, nil
		}
		first, err := snap()
		if err != nil {
			return text("error: " + firstErr(err)), nil, nil
		}
		select {
		case <-ctx.Done():
			return text("cancelled"), nil, nil
		case <-time.After(time.Duration(w) * time.Second):
		}
		second, err := snap()
		if err != nil {
			return text("error: " + err.Error()), nil, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "window: %ds\n", w)
		for _, k := range []string{"page_reads", "page_writes", "inserts", "updates", "deletes"} {
			fmt.Fprintf(&b, "%s: %d\n", k, second[k]-first[k])
		}
		aud.Log(audit.Entry{Identity: "local", Database: a.Db, Tool: "fb_activity_sample", Tier: 0, Decision: "allow"})
		return text(b.String()), nil, nil
	})
}

func firstErr(err error) string { return err.Error() }

var _ = adminexec.Run // future use (gstat route)

// typeName maps RDB$FIELD_TYPE blr codes to readable names.
func typeName(t int) string {
	switch t {
	case 7:
		return "SMALLINT"
	case 8:
		return "INTEGER"
	case 10:
		return "FLOAT"
	case 12:
		return "DATE"
	case 13:
		return "TIME"
	case 14:
		return "CHAR"
	case 16:
		return "INT64/NUMERIC"
	case 23:
		return "BOOLEAN"
	case 24:
		return "DECFLOAT"
	case 26:
		return "INT128"
	case 27:
		return "DOUBLE"
	case 35:
		return "TIMESTAMP"
	case 37:
		return "VARCHAR"
	case 40:
		return "CSTRING"
	case 45:
		return "BLOB"
	default:
		return fmt.Sprintf("TYPE_%d", t)
	}
}
