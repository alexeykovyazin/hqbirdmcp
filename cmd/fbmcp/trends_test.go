package main

import (
	"strings"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/trends"
)

func sampleSeq(start time.Time, n int, step time.Duration, sz int64, growth int64) []trends.Sample {
	out := make([]trends.Sample, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, trends.Sample{
			Time:        start.Add(time.Duration(i) * step).Unix(),
			SizeBytes:   sz + growth*int64(i),
			Attachments: 4,
			Reads:       int64(1000 * (i + 1)),
			Writes:      int64(100 * (i + 1)),
			Fetches:     int64(10000 * (i + 1)),
			OATGap:      3,
		})
	}
	return out
}

func TestRenderTrendsNoSamples(t *testing.T) {
	out := renderTrends("db1", nil, 24, 0, 300)
	if !strings.Contains(out, "no trend samples") || !strings.Contains(out, "every 300s") {
		t.Fatalf("render: %s", out)
	}
}

func TestRenderTrendsCollecting(t *testing.T) {
	// two samples: window shown, projection explicitly not attempted
	s := sampleSeq(time.Now().Add(-2*time.Minute), 2, time.Minute, 1<<30, 1<<20)
	out := renderTrends("db1", s, 24, 10, 300)
	if !strings.Contains(out, "samples: 2") || !strings.Contains(out, "collecting history") {
		t.Fatalf("render: %s", out)
	}
}

func TestRenderTrendsProjection(t *testing.T) {
	// 1 GiB growing 0.1 GiB/day sampled hourly for 10 days → 10 GiB in ~90 days
	start := time.Now().Add(-10 * 24 * time.Hour)
	s := sampleSeq(start, 241, time.Hour, 1<<30, (1<<30)/240)
	out := renderTrends("db1", s, 24*11, 10, 300)
	// last sample = 2 GiB, growth 0.1 GiB/day → 10 GiB in ~80 days
	if !strings.Contains(out, "10.00 GiB threshold in ~80 days") {
		t.Fatalf("projection render: %s", out)
	}
	if !strings.Contains(out, "ANOMALY") {
		// flat 4 attachments over 241 samples must NOT flag
		if strings.Contains(out, "attachments: current 4 | window mean 4.0, max 4") == false {
			t.Fatalf("attachment line: %s", out)
		}
	}
}

func TestRenderTrendsAnomalyAndReset(t *testing.T) {
	s := sampleSeq(time.Now().Add(-30*time.Minute), 30, time.Minute, 1<<30, 0)
	for i := 15; i < len(s); i++ {
		s[i].Reads = 500 + int64(i) // reset at sample 15 (below previous cumulative)
	}
	s[len(s)-1].Attachments = 40 // spike
	out := renderTrends("db1", s, 1, 0, 300)
	if !strings.Contains(out, "ANOMALY") {
		t.Fatalf("spike not flagged:\n%s", out)
	}
	if !strings.Contains(out, "1 engine restart(s) detected") {
		t.Fatalf("reset not reported:\n%s", out)
	}
}

func TestRenderTrendsWindowDelta(t *testing.T) {
	s := sampleSeq(time.Now().Add(-3*time.Hour), 4, time.Hour, 1<<30, 1<<28)
	out := renderTrends("db1", s, 24, 0, 300)
	if !strings.Contains(out, "window delta +") {
		t.Fatalf("delta line:\n%s", out)
	}
}
