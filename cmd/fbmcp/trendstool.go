// fb_trends (C.4, Tier 0) + the real sampler plumbing: per-DB metrics
// (file size, attachments, cumulative page I/O, oldest-active transaction
// gap) collected on a ticker into <state.dir>/trends/<db>.jsonl, projected
// with least squares. History starts when the kernel first runs with the
// sampler enabled — the tool says so explicitly.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/dbpool"
	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/trends"
)

// trendsDBRefs lists every configured database for the sampler.
func trendsDBRefs(handle *config.Handle) []trends.DBRef {
	var out []trends.DBRef
	for _, d := range handle.Current().Databases {
		out = append(out, trends.DBRef{ID: d.ID, Path: d.Path})
	}
	return out
}

// newTrendsSampleFn collects one sample via the RO pool (MON$DATABASE +
// MON$ATTACHMENTS) and the database file's on-disk size. Any MON$ failure
// skips the tick for that DB (server unreachable / shutting down).
func newTrendsSampleFn(pools *dbpool.Manager) trends.SampleFn {
	return func(ctx context.Context, db trends.DBRef) (trends.Sample, error) {
		tx, err := pools.ReadOnly(ctx, db.ID)
		if err != nil {
			return trends.Sample{}, err
		}
		defer tx.Rollback()
		var s trends.Sample
		// IO counters: SUM over MON$IO_STATS (all engines; the MON$DATABASE
		// IO columns are FB4+ — verified on FB3). Cumulative since attach.
		if err := tx.QueryRowContext(ctx, `SELECT
			COALESCE(SUM(MON$PAGE_READS), 0), COALESCE(SUM(MON$PAGE_WRITES), 0),
			COALESCE(SUM(MON$PAGE_FETCHES), 0)
			FROM MON$IO_STATS`).Scan(&s.Reads, &s.Writes, &s.Fetches); err != nil {
			return trends.Sample{}, fmt.Errorf("MON$IO_STATS: %w", err)
		}
		var next, oat int64
		if err := tx.QueryRowContext(ctx, `SELECT
			COALESCE(MON$NEXT_TRANSACTION, 0), COALESCE(MON$OLDEST_ACTIVE, 0)
			FROM MON$DATABASE`).Scan(&next, &oat); err != nil {
			return trends.Sample{}, fmt.Errorf("MON$DATABASE: %w", err)
		}
		s.OATGap = next - oat
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM MON$ATTACHMENTS`).Scan(&s.Attachments); err != nil {
			return trends.Sample{}, fmt.Errorf("MON$ATTACHMENTS: %w", err)
		}
		if fi, err := os.Stat(db.Path); err == nil {
			s.SizeBytes = fi.Size()
		}
		s.Time = time.Now().Unix()
		return s, nil
	}
}

// startTrendsSampler launches the C.4 collection loop (no-op when disabled).
func startTrendsSampler(ctx context.Context, handle *config.Handle, pools *dbpool.Manager) {
	if handle.Current().Trends.Disabled {
		return
	}
	tr := handle.Current().Trends.OrDefault()
	sampler := &trends.Sampler{
		Dir:       handle.Current().State.Dir,
		Interval:  time.Duration(tr.IntervalSeconds) * time.Second,
		Retention: time.Duration(tr.RetentionDays) * 24 * time.Hour,
		List:      func() []trends.DBRef { return trendsDBRefs(handle) },
		Sample:    newTrendsSampleFn(pools),
	}
	go sampler.Run(ctx)
}

func registerTrendsTool(server *mcp.Server, handle *config.Handle, pools *dbpool.Manager, aud *audit.Logger) {
	type trendsArg struct {
		Db          string  `json:"db" jsonschema:"registry id of the database"`
		Hours       float64 `json:"hours,omitempty" jsonschema:"analysis window in hours (default 24)"`
		ThresholdGb float64 `json:"threshold_gb,omitempty" jsonschema:"project days until the on-disk size reaches this many GB"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "fb_trends", Description: "Tier 0: capacity trends from the sampler history (default window 24h) — size least-squares projection (days to threshold_gb), attachment-spike anomaly flag, IO deltas. History starts when the kernel first ran with the sampler enabled"}, func(ctx context.Context, req *mcp.CallToolRequest, a trendsArg) (*mcp.CallToolResult, any, error) {
		if _, err := handle.DB(a.Db); err != nil {
			return errText("error: " + err.Error())
		}
		hours := a.Hours
		if hours <= 0 {
			hours = 24
		}
		since := time.Now().Add(-time.Duration(hours * float64(time.Hour))).Unix()
		samples, err := trends.Read(handle.Current().State.Dir, a.Db, since)
		if err != nil {
			return errText("error: " + err.Error())
		}
		structured := map[string]any{"window_hours": hours, "sample_count": len(samples)}
		aud.Log(audit.Entry{Identity: identity.Caller(ctx).Name, Database: a.Db, Tool: "fb_trends", Tier: 0, Decision: "allow",
			Detail: map[string]interface{}{"samples": len(samples)}})
		return text(renderTrends(a.Db, samples, hours, a.ThresholdGb, handle.Current().Trends.OrDefault().IntervalSeconds)), structured, nil
	})
}

func humanBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// renderTrends is the human-facing analysis (unit-tested with synthetic series).
func renderTrends(db string, samples []trends.Sample, hours, thresholdGb float64, intervalSeconds int) string {
	var b strings.Builder
	if len(samples) == 0 {
		fmt.Fprintf(&b, "no trend samples for %s in the last %.0f h — the sampler appends one sample per database every %ds; history starts when the kernel first ran with it enabled\n",
			db, hours, intervalSeconds)
		return b.String()
	}
	first, last := samples[0], samples[len(samples)-1]
	fmt.Fprintf(&b, "samples: %d over %.0f h (first %s, last %s)\n",
		len(samples), hours, time.Unix(first.Time, 0).Format("01-02 15:04"), time.Unix(last.Time, 0).Format("01-02 15:04"))

	b.WriteString(fmt.Sprintf("size now: %s", humanBytes(last.SizeBytes)))
	if last.SizeBytes > 0 && first.SizeBytes > 0 && len(samples) > 1 {
		fmt.Fprintf(&b, " (window delta %s%s)", signByte(last.SizeBytes-first.SizeBytes), humanBytes(abs64(last.SizeBytes-first.SizeBytes)))
	}
	b.WriteString("\n")

	if thr := int64(thresholdGb * (1 << 30)); thresholdGb > 0 {
		p := trends.ProjectSize(samples, thr)
		if p.N < 3 {
			b.WriteString("projection: collecting history (needs >= 3 samples)\n")
		} else if p.DaysToThreshold == nil {
			b.WriteString(fmt.Sprintf("projection: growth %.2f MiB/day — no threshold crossing projected (shrinking or flat)\n", p.SlopeDay/(1<<20)))
		} else if *p.DaysToThreshold == 0 {
			b.WriteString("projection: threshold already reached\n")
		} else {
			b.WriteString(fmt.Sprintf("projection: growth %.2f MiB/day → %s threshold in ~%.0f days (estimate from least squares over %d samples)\n",
				p.SlopeDay/(1<<20), humanBytes(thr), *p.DaysToThreshold, p.N))
		}
	}

	att := trends.AnalyzeAttachments(samples)
	fmt.Fprintf(&b, "attachments: current %d | window mean %.1f, max %d", att.Current, att.Mean, att.Max)
	if att.Anomaly {
		b.WriteString(" — ANOMALY: current exceeds mean+3σ (possible leak or new client farm)")
	}
	b.WriteString("\n")

	if d := trends.IODeltas(samples); len(samples) > 1 {
		fmt.Fprintf(&b, "page IO over window: reads %d, writes %d, fetches %d", d.Reads, d.Writes, d.Fetches)
		if d.Resets > 0 {
			fmt.Fprintf(&b, " (%d engine restart(s) detected — counters reset)", d.Resets)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func signByte(n int64) string {
	if n < 0 {
		return "-"
	}
	return "+"
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
