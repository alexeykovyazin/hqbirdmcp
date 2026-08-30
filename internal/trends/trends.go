// Package trends implements the C.4 capacity-trends surface: a periodic
// per-database sampler appending NDJSON under <state.dir>/trends/, least-
// squares size projections ("threshold in N days"), attachment-spike
// flags, and day-based retention pruning. History starts when the kernel
// first runs with the sampler enabled — the tool says so.
//
// The sampler is stateless across restarts by design: state IS the jsonl
// file (restart durability is just "append again").
package trends

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"time"
)

// Sample is one observation of one database. Read/Write/Fetch are engine
// cumulative counters (MON$DATABASE, reset on engine restart — analysis
// treats a decrease as a reset); OATGap is Next - OldestActive.
type Sample struct {
	Time        int64 `json:"t"` // unix seconds
	SizeBytes   int64 `json:"size_bytes"`
	Attachments int64 `json:"attachments"`
	Reads       int64 `json:"reads"`
	Writes      int64 `json:"writes"`
	Fetches     int64 `json:"fetches"`
	OATGap      int64 `json:"oat_gap"`
}

// dir returns <stateDir>/trends; file is <dir>/<db>.jsonl.
func fileFor(stateDir, db string) string {
	return filepath.Join(stateDir, "trends", db+".jsonl")
}

// Append writes one sample to the database's jsonl (creating dirs as needed).
func Append(stateDir, db string, s Sample) error {
	p := fileFor(stateDir, db)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// Read loads a database's samples with Time >= since (unix seconds; 0 = all).
func Read(stateDir, db string, since int64) ([]Sample, error) {
	f, err := os.Open(fileFor(stateDir, db))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Sample
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var s Sample
		if err := json.Unmarshal(line, &s); err != nil {
			continue // tolerate a torn trailing line (crash mid-append)
		}
		if s.Time >= since {
			out = append(out, s)
		}
	}
	return out, sc.Err()
}

// Prune drops samples older than olderThan from the database's jsonl and
// reports how many were removed. Rewrites the file (samples are small;
// files stay in the low MiB range for month-scale retention).
func Prune(stateDir, db string, olderThan time.Time) (int, error) {
	p := fileFor(stateDir, db)
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var kept [][]byte
	removed := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var s Sample
		if json.Unmarshal(line, &s) != nil || s.Time < olderThan.Unix() {
			removed++
			continue
		}
		kept = append(kept, append([]byte(nil), line...))
	}
	f.Close()
	if err := sc.Err(); err != nil {
		return removed, err
	}
	if removed == 0 {
		return 0, nil
	}
	tmp := p + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return removed, err
	}
	for _, line := range kept {
		if _, err := out.Write(append(line, '\n')); err != nil {
			out.Close()
			return removed, err
		}
	}
	if err := out.Close(); err != nil {
		return removed, err
	}
	return removed, os.Rename(tmp, p)
}

// ---------------------------------------------------------------------------
// Analysis
// ---------------------------------------------------------------------------

// Projection is the least-squares size trend over a sample window.
type Projection struct {
	N        int     `json:"n"` // samples used
	SlopeDay float64 `json:"slope_bytes_per_day"`
	// DaysToThreshold is populated only when a threshold was given, the
	// series is long enough (>=3), and the trend is growing.
	DaysToThreshold *float64 `json:"days_to_threshold,omitempty"`
}

// ProjectSize fits size over time (unix seconds → bytes/day). Windows with
// fewer than 3 samples are reported, not projected (two points always fit a
// line perfectly and would read as false precision).
func ProjectSize(samples []Sample, thresholdBytes int64) Projection {
	var p Projection
	p.N = len(samples)
	slope := leastSquares(samples)
	p.SlopeDay = slope
	if p.N < 3 || thresholdBytes <= 0 || slope <= 0 {
		return p
	}
	last := samples[len(samples)-1]
	if float64(thresholdBytes) <= float64(last.SizeBytes) {
		d := 0.0
		p.DaysToThreshold = &d // already at/over threshold
		return p
	}
	days := (float64(thresholdBytes) - float64(last.SizeBytes)) / slope
	p.DaysToThreshold = &days
	return p
}

// leastSquares: slope of size over time, bytes per day. Time tied to the
// first sample to keep the fit numerically well-conditioned.
func leastSquares(samples []Sample) float64 {
	n := len(samples)
	if n < 2 {
		return 0
	}
	t0 := samples[0].Time
	var sx, sy, sxx, sxy float64
	for _, s := range samples {
		x := float64(s.Time-t0) / 86400.0 // days
		y := float64(s.SizeBytes)
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	den := float64(n)*sxx - sx*sx
	if den == 0 {
		return 0
	}
	return (float64(n)*sxy - sx*sy) / den
}

// SpikeStat summarizes the attachment series of a window.
type SpikeStat struct {
	Current  int64   `json:"current"`
	Mean     float64 `json:"mean"`
	Max      int64   `json:"max"`
	StdDev   float64 `json:"stddev"`
	Anomaly  bool    `json:"anomaly"` // current > mean + 3σ (needs >= 20 samples)
	SamplesN int     `json:"samples"`
}

// AnalyzeAttachments flags an attachment spike: current count above mean+3σ
// over the window (with enough samples for the statistic to mean anything).
func AnalyzeAttachments(samples []Sample) SpikeStat {
	var st SpikeStat
	st.SamplesN = len(samples)
	if len(samples) == 0 {
		return st
	}
	st.Current = samples[len(samples)-1].Attachments
	var sum float64
	st.Max = samples[0].Attachments
	for _, s := range samples {
		sum += float64(s.Attachments)
		if s.Attachments > st.Max {
			st.Max = s.Attachments
		}
	}
	st.Mean = sum / float64(len(samples))
	var varSum float64
	for _, s := range samples {
		d := float64(s.Attachments) - st.Mean
		varSum += d * d
	}
	st.StdDev = math.Sqrt(varSum / float64(len(samples)))
	if len(samples) >= 20 && float64(st.Current) > st.Mean+3*st.StdDev {
		st.Anomaly = true
	}
	return st
}

// IO cumulative-counter deltas over the window, reset-aware: a decrease
// means the engine restarted, so that boundary contributes no delta.
type IODelta struct {
	Reads   int64 `json:"reads"`
	Writes  int64 `json:"writes"`
	Fetches int64 `json:"fetches"`
	Resets  int   `json:"resets"`
}

func IODeltas(samples []Sample) IODelta {
	var d IODelta
	for i := 1; i < len(samples); i++ {
		prev, cur := samples[i-1], samples[i]
		if cur.Reads >= prev.Reads {
			d.Reads += cur.Reads - prev.Reads
		} else {
			d.Resets++
		}
		if cur.Writes >= prev.Writes {
			d.Writes += cur.Writes - prev.Writes
		}
		if cur.Fetches >= prev.Fetches {
			d.Fetches += cur.Fetches - prev.Fetches
		}
	}
	return d
}
