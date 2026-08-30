// fb_diff_schema / fb_diff_data (C.3, Tier 0): compare two registered
// databases, or one database against its persisted schema snapshot
// (<state.dir>/schema-snapshots/<db>.json). Both tools are read-only; the
// data diff refuses tables above the row cap instead of truncating.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/dbpool"
	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/schemadiff"
)

const diffDataDefaultRowCap = 100_000

// snapshotPath is where a db's schema snapshot persists. Snapshots are
// advisory drift-detection artifacts, not backups — deleted with the state
// dir like any other kernel state.
func snapshotPath(stateDir, dbID string) string {
	return filepath.Join(stateDir, "schema-snapshots", dbID+".json")
}

func saveSchemaSnapshot(stateDir, dbID string, s *schemadiff.Schema) error {
	p := snapshotPath(stateDir, dbID)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

func loadSchemaSnapshot(stateDir, dbID string) (*schemadiff.Schema, error) {
	b, err := os.ReadFile(snapshotPath(stateDir, dbID))
	if err != nil {
		return nil, fmt.Errorf("no schema snapshot for %s — run fb_diff_schema with save_snapshot:true once", dbID)
	}
	var s schemadiff.Schema
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("snapshot for %s corrupt: %v", dbID, err)
	}
	return &s, nil
}

// captureRegistryDB captures one registered database's schema via its read pool.
func captureRegistryDB(ctx context.Context, pools *dbpool.Manager, dbID string) (*schemadiff.Schema, error) {
	pool, err := pools.ReadPool(ctx, dbID)
	if err != nil {
		return nil, err
	}
	return schemadiff.Capture(ctx, pool)
}

func registerDiffTools(server *mcp.Server, cfg *config.Handle, pools *dbpool.Manager, aud *audit.Logger) {
	type diffSchemaArg struct {
		Db           string `json:"db" jsonschema:"registry id of the database (left side / source)"`
		VsDb         string `json:"vs_db,omitempty" jsonschema:"second registered database id to compare against (default: the stored snapshot when use_snapshot, else required)"`
		UseSnapshot  bool   `json:"use_snapshot,omitempty" jsonschema:"compare db against its stored snapshot (taken with save_snapshot) instead of vs_db"`
		SaveSnapshot bool   `json:"save_snapshot,omitempty" jsonschema:"persist db's current schema as the comparison snapshot"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_diff_schema", Description: "Tier 0: schema diff between two registered databases (or db vs its stored snapshot) — grouped 'would need CREATE/ALTER/DROP' output; also saves snapshots for drift detection. Read-only"}, func(ctx context.Context, req *mcp.CallToolRequest, a diffSchemaArg) (*mcp.CallToolResult, any, error) {
		if _, err := cfg.DB(a.Db); err != nil {
			return errText("error: " + err.Error())
		}
		useSnapshot := a.UseSnapshot
		if a.VsDb == "" && !useSnapshot {
			useSnapshot = true // snapshot-vs-now is the default when no vs_db
		}
		if a.VsDb != "" && useSnapshot {
			return errText("error: give either vs_db or use_snapshot, not both")
		}
		if a.VsDb != "" {
			if _, err := cfg.DB(a.VsDb); err != nil {
				return errText("error: " + err.Error())
			}
		}
		stateDir := cfg.Current().State.Dir

		var schemaA, schemaB *schemadiff.Schema
		var err error
		schemaA, err = captureRegistryDB(ctx, pools, a.Db)
		if err != nil {
			return errText("error: " + err.Error())
		}
		if a.SaveSnapshot {
			if err := saveSchemaSnapshot(stateDir, a.Db, schemaA); err != nil {
				return errText("error: snapshot save: " + err.Error())
			}
		}
		if useSnapshot {
			schemaB, err = loadSchemaSnapshot(stateDir, a.Db)
			if err != nil {
				return errText("error: " + err.Error())
			}
		} else {
			schemaB, err = captureRegistryDB(ctx, pools, a.VsDb)
			if err != nil {
				return errText("error: " + err.Error())
			}
		}
		nameB := a.VsDb
		if useSnapshot {
			nameB = "snapshot(" + a.Db + ")"
		}
		res := schemadiff.Diff(schemaA, schemaB)
		structured := map[string]any{"diff": res, "saved_snapshot": a.SaveSnapshot}
		aud.Log(audit.Entry{Identity: identity.Caller(ctx).Name, Database: a.Db, Tool: "fb_diff_schema", Tier: 0, Decision: "allow",
			Detail: map[string]interface{}{"identical": res.Identical, "vs": nameB}})
		out := schemadiff.Render(res, a.Db, nameB)
		if a.SaveSnapshot {
			out += "\nsnapshot saved: " + snapshotPath(stateDir, a.Db)
		}
		return text(out), structured, nil
	})

	type diffDataArg struct {
		Db         string `json:"db" jsonschema:"registry id of the source database"`
		VsDb       string `json:"vs_db" jsonschema:"registry id of the database to compare against"`
		Table      string `json:"table" jsonschema:"one table to compare (must have a primary key on both sides)"`
		RowCap     int    `json:"row_cap,omitempty" jsonschema:"refuse when either side exceeds this many rows (default 100000)"`
		SampleRows int    `json:"sample_rows,omitempty" jsonschema:"sample rows kept per category (default 10)"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_diff_data", Description: "Tier 0: bounded key-based data diff of one table between two registered databases — streams both sides ordered by primary key, counts only-in-A / only-in-B / differing rows with samples; refuses above row_cap (default 100k). Read-only"}, func(ctx context.Context, req *mcp.CallToolRequest, a diffDataArg) (*mcp.CallToolResult, any, error) {
		if _, err := cfg.DB(a.Db); err != nil {
			return errText("error: " + err.Error())
		}
		if _, err := cfg.DB(a.VsDb); err != nil {
			return errText("error: " + err.Error())
		}
		table := strings.TrimSpace(a.Table)
		if table == "" || !safeIdent(table) {
			return errText("error: table must be a single plain identifier")
		}
		poolA, err := pools.ReadPool(ctx, a.Db)
		if err != nil {
			return errText("error: " + err.Error())
		}
		poolB, err := pools.ReadPool(ctx, a.VsDb)
		if err != nil {
			return errText("error: " + err.Error())
		}
		rowCap := a.RowCap
		if rowCap <= 0 {
			rowCap = diffDataDefaultRowCap
		}
		res, err := schemadiff.DiffData(ctx, poolA, poolB, table, schemadiff.DataDiffOptions{RowCap: rowCap, Samples: a.SampleRows})
		if err != nil {
			return errText("error: " + err.Error())
		}
		var b strings.Builder
		fmt.Fprintf(&b, "data diff %s.%s vs %s.%s: rows %d vs %d | only in %s: %d | only in %s: %d | differing: %d\n",
			a.Db, table, a.VsDb, table, res.RowsA, res.RowsB, a.Db, res.OnlyInA, a.VsDb, res.OnlyInB, res.Different)
		writeSamples := func(label string, samples []string) {
			if len(samples) == 0 {
				return
			}
			fmt.Fprintf(&b, "%s:\n", label)
			for _, s := range samples {
				b.WriteString("  " + s + "\n")
			}
		}
		writeSamples("only in "+a.Db, res.SamplesOnlyA)
		writeSamples("only in "+a.VsDb, res.SamplesOnlyB)
		writeSamples("differing (A → B)", res.SamplesDiff)
		if res.Truncated {
			b.WriteString("(sample list truncated — counts above are complete)\n")
		}
		structured := map[string]any{"diff": res}
		aud.Log(audit.Entry{Identity: identity.Caller(ctx).Name, Database: a.Db, Tool: "fb_diff_data", Tier: 0, Decision: "allow",
			Detail: map[string]interface{}{"table": table, "vs": a.VsDb, "only_a": res.OnlyInA, "only_b": res.OnlyInB, "different": res.Different}})
		return text(b.String()), structured, nil
	})
}

// openFirebirdPath opens an arbitrary database file path through the
// instance's server (used by the restore-test diff step for the verify copy).
func openFirebirdPath(instAddr, path, user, pass string) (*sql.DB, error) {
	return sql.Open("firebirdsql", dbpool.DSN(instAddr, path, user, pass))
}
