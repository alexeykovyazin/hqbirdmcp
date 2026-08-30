// Package executor is the P4.1 generic write service (K6 preview + gated
// execution). All statement execution routes through here (ADR-019/021).
package executor

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/aleks/fbmcp/internal/classify"
	"github.com/aleks/fbmcp/internal/dbpool"
	"github.com/aleks/fbmcp/internal/fbparse"
	"github.com/aleks/fbmcp/internal/gate"
	"github.com/aleks/fbmcp/internal/killpoint"
)

const (
	perStmtTimeout = 30 * time.Second
	totalTimeout   = 5 * time.Minute
)

// Service is the single execution/preview entry used by fb_write and the
// structured mutation front-ends.
type Service struct {
	Pools *dbpool.Manager
}

// Prepared is a classified, guard-checked script ready to preview or execute.
type Prepared struct {
	SQL     string
	Results []classify.Result
	MaxTier int
	HasDDL  bool
	// MinFB is the highest fbparse.Statement.MinVersion across the script
	// (P7.3/P7.4, phase7_plan.md), e.g. "5.0" for a CONCURRENTLY index build
	// or a materialized view. "" means no statement raised a version floor.
	// The caller wires this into policy.ToolMeta.MinFB so the existing
	// fail-closed engine_version check (policy.EvaluateMeta) denies the
	// script outright on an engine that doesn't support the construct,
	// instead of letting the SQL reach the engine and fail there.
	MinFB string
	// NeedsExclusive is true when a statement requires exclusive table
	// reservation the pooled connections' open snapshot transactions would
	// deny (WS3.1): a non-CONCURRENTLY REFRESH MATERIALIZED VIEW (exclusive
	// reload and DROP DATA modes — README.materialized_view.md). The caller
	// drains the target DB's pools (dbpool.CloseDB) before Exec, the same
	// primitive restore_replace uses.
	NeedsExclusive bool
}

// Prepare classifies and applies ADR-019 guard rails. It does not execute.
func Prepare(sqlText string) (Prepared, error) {
	results, maxTier, why, ok := classify.Script(sqlText)
	if !ok {
		return Prepared{}, fmt.Errorf("%s", why)
	}
	if maxTier == 0 {
		return Prepared{}, fmt.Errorf("fb_write is for mutations; use fb_query (Tier-0, read-only transaction) for SELECT")
	}
	if maxTier >= 3 {
		return Prepared{}, fmt.Errorf("script contains Tier-3 content (disabled by default)")
	}
	minFB := ""
	needsExclusive := false
	for _, r := range results {
		if versionGreater(r.Statement.MinVersion, minFB) {
			minFB = r.Statement.MinVersion
		}
		if r.Statement.Verb == fbparse.VerbRefresh && r.Statement.Flags.Extras["refresh_mode"] != "concurrently" {
			needsExclusive = true
		}
	}
	return Prepared{SQL: sqlText, Results: results, MaxTier: maxTier, HasDDL: classify.HasDDL(results), MinFB: minFB, NeedsExclusive: needsExclusive}, nil
}

// versionGreater compares "MAJOR.MINOR" version strings numerically ("" is
// the lowest). Malformed input can't occur here — raiseVersion only ever
// sets fbparse.Statement.MinVersion to a literal it wrote itself.
func versionGreater(a, b string) bool {
	if a == "" {
		return false
	}
	if b == "" {
		return true
	}
	var am, an, bm, bn int
	fmt.Sscanf(a, "%d.%d", &am, &an)
	fmt.Sscanf(b, "%d.%d", &bm, &bn)
	if am != bm {
		return am > bm
	}
	return an > bn
}

// Impact renders the ADR-021 preview text. It never uses the word "safe".
// Row estimates are best-effort via the read pool when dbID is set.
func (s *Service) Impact(ctx context.Context, dbID string, p Prepared) string {
	var b strings.Builder
	b.WriteString(classify.Preview(p.Results))
	fmt.Fprintf(&b, "execution: ")
	if p.HasDDL {
		b.WriteString("per-statement commits (script contains DDL; Firebird cannot mix DDL+DML in one transaction — partial application is reported on failure)\n")
	} else {
		b.WriteString("single transaction (atomic rollback on any error)\n")
	}
	b.WriteString("verification: row counts from the driver where reported; DDL read-back via fb_describe\n")
	fmt.Fprintf(&b, "confirmation channels: %s\n", strings.Join(gate.AllowedChannels(p.MaxTier), ", "))
	if s != nil && s.Pools != nil && dbID != "" {
		for i, r := range p.Results {
			q, ok := r.Statement.RowEstimateQuery()
			if !ok {
				continue
			}
			n, err := s.estimate(ctx, dbID, q)
			if err != nil {
				fmt.Fprintf(&b, "estimate[%d]: unavailable (%v)\n", i+1, err)
				continue
			}
			fmt.Fprintf(&b, "estimate[%d]: %d rows matching WHERE\n", i+1, n)
		}
	}
	return b.String()
}

func (s *Service) estimate(ctx context.Context, dbID, q string) (int64, error) {
	tx, err := s.Pools.ReadOnly(ctx, dbID)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var n int64
	if err := tx.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// Exec runs a prepared script on the admin pool (post-gate only).
func (s *Service) Exec(ctx context.Context, dbID string, p Prepared, prog func(float64, string)) (string, error) {
	if s == nil || s.Pools == nil {
		return "", fmt.Errorf("executor: no admin pool")
	}
	pool, err := s.Pools.AdminPool(ctx, dbID)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, totalTimeout)
	defer cancel()

	stmts := make([]string, 0, len(p.Results))
	for _, r := range p.Results {
		raw := strings.TrimSpace(r.Statement.Raw)
		if raw != "" {
			stmts = append(stmts, raw)
		}
	}
	if len(stmts) == 0 {
		// fall back to fbparse.Split if Raw was empty
		for _, sp := range fbparse.Split(p.SQL) {
			raw := strings.TrimSpace(p.SQL[sp.Start:sp.End])
			if raw != "" {
				stmts = append(stmts, raw)
			}
		}
	}

	var report strings.Builder
	if !p.HasDDL {
		return s.execAtomic(ctx, pool, stmts, prog, &report)
	}
	return s.execPerStatement(ctx, pool, stmts, prog, &report)
}

func (s *Service) execAtomic(ctx context.Context, pool *sql.DB, stmts []string, prog func(float64, string), report *strings.Builder) (string, error) {
	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	for i, stmt := range stmts {
		if prog != nil {
			prog(float64(i)/float64(len(stmts)), fmt.Sprintf("statement %d/%d", i+1, len(stmts)))
		}
		sctx, cancel := context.WithTimeout(ctx, perStmtTimeout)
		res, err := tx.ExecContext(sctx, stmt)
		cancel()
		if err != nil {
			return "", fmt.Errorf("statement %d failed (%s) — transaction rolled back: %w", i+1, shortOf(stmt), err)
		}
		n, _ := res.RowsAffected()
		fmt.Fprintf(report, "%d. OK %s (rows affected: %d)\n", i+1, shortOf(stmt), n)
	}
	killpoint.Hit("exec.pre-commit") // chaos harness: kill with statements executed but uncommitted
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit failed — rolled back: %w", err)
	}
	killpoint.Hit("exec.post-commit") // chaos harness: kill after commit, before the job result is recorded
	return "committed atomically (single transaction):\n" + report.String(), nil
}

func (s *Service) execPerStatement(ctx context.Context, pool *sql.DB, stmts []string, prog func(float64, string), report *strings.Builder) (string, error) {
	applied := 0
	for i, stmt := range stmts {
		if prog != nil {
			prog(float64(i)/float64(len(stmts)), fmt.Sprintf("statement %d/%d", i+1, len(stmts)))
		}
		sctx, cancel := context.WithTimeout(ctx, perStmtTimeout)
		tx, err := pool.BeginTx(sctx, nil)
		if err != nil {
			cancel()
			return "", err
		}
		res, err := tx.ExecContext(sctx, stmt)
		if err != nil {
			tx.Rollback()
			cancel()
			return "", fmt.Errorf("statement %d failed (%s) — PARTIALLY APPLIED: %d of %d statements committed before the failure: %w",
				i+1, shortOf(stmt), applied, len(stmts), err)
		}
		if err := tx.Commit(); err != nil {
			cancel()
			return "", fmt.Errorf("statement %d commit failed: %w", i+1, err)
		}
		cancel()
		n, _ := res.RowsAffected()
		applied++
		fmt.Fprintf(report, "%d. OK %s (rows affected: %d)\n", i+1, shortOf(stmt), n)
	}
	return fmt.Sprintf("committed per-statement (%d statements, script contained DDL):\n%s", applied, report.String()), nil
}

func shortOf(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 50 {
		return s[:50] + "…"
	}
	return s
}
