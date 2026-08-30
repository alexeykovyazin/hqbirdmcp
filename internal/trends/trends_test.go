package trends

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const GB = 1 << 30

func linearSeries(days int, start, growthPerDay float64) []Sample {
	out := make([]Sample, 0, days+1)
	for i := 0; i <= days; i++ {
		out = append(out, Sample{
			Time:      int64(1700000000 + i*86400),
			SizeBytes: int64(start + growthPerDay*float64(i)),
		})
	}
	return out
}

func TestProjectSizeLinear(t *testing.T) {
	// 1 GB growing 0.1 GB/day, sampled daily for 10 days (perfect line).
	s := linearSeries(10, GB, GB/10)
	p := ProjectSize(s, 3*GB)
	if p.N != 11 {
		t.Fatalf("n = %d", p.N)
	}
	if math.Abs(p.SlopeDay-GB/10) > GB/1000 {
		t.Fatalf("slope = %f, want ~%d", p.SlopeDay, GB/10)
	}
	if p.DaysToThreshold == nil {
		t.Fatal("no projection")
	}
	// last sample = 2 GB; reaching 3 GB at 0.1 GB/day takes 10 days.
	if math.Abs(*p.DaysToThreshold-10) > 0.01 {
		t.Fatalf("days = %f, want ~10", *p.DaysToThreshold)
	}
}

func TestProjectSizeNoisy(t *testing.T) {
	// 10% noise around the same line: slope must stay within 5%.
	s := linearSeries(30, GB, GB/10)
	for i := range s {
		noise := float64((i*7919)%97-48) / 48.0 // deterministic, ±1
		s[i].SizeBytes += int64(noise * GB / 100)
	}
	p := ProjectSize(s, 5*GB) // last sample is 4 GB; 5 GB is ~10 days out
	if math.Abs(p.SlopeDay-GB/10) > GB/20 {
		t.Fatalf("slope = %f, want within 5%% of %d", p.SlopeDay, GB/10)
	}
	if p.DaysToThreshold == nil || math.Abs(*p.DaysToThreshold-10) > 2 {
		t.Fatalf("days = %f, want ~10±2", safeDays(p.DaysToThreshold))
	}
}

func TestProjectSizeGuards(t *testing.T) {
	// fewer than 3 samples: reported, not projected
	p := ProjectSize(linearSeries(1, GB, GB), 3*GB)
	if p.N != 2 || p.DaysToThreshold != nil {
		t.Fatalf("2 samples must not project: %+v", p)
	}
	// shrinking series: no threshold projection
	shrink := linearSeries(10, 3*GB, -GB/10)
	p = ProjectSize(shrink, 4*GB)
	if p.DaysToThreshold != nil {
		t.Fatalf("shrinking series must not project: %+v", p)
	}
	// already over threshold: zero days
	over := linearSeries(10, GB, GB/10) // last = 2 GB
	p = ProjectSize(over, GB)
	if p.DaysToThreshold == nil || *p.DaysToThreshold != 0 {
		t.Fatalf("over-threshold must be 0 days: %+v", p)
	}
}

func TestAnalyzeAttachmentsSpike(t *testing.T) {
	base := make([]Sample, 30)
	for i := range base {
		base[i] = Sample{Attachments: 4, Time: int64(i)}
	}
	st := AnalyzeAttachments(base)
	if st.Anomaly {
		t.Fatalf("flat series flagged: %+v", st)
	}
	// inject a spike at the head (last sample = current)
	spike := append([]Sample{}, base...)
	spike[len(spike)-1].Attachments = 40
	st = AnalyzeAttachments(spike)
	if !st.Anomaly || st.Current != 40 {
		t.Fatalf("spike not flagged: %+v", st)
	}
	// short window: statistics disabled
	st = AnalyzeAttachments(spike[:10])
	if st.Anomaly {
		t.Fatalf("short window must not flag: %+v", st)
	}
}

func TestIODeltasReset(t *testing.T) {
	s := []Sample{
		{Reads: 100, Writes: 10, Fetches: 1000},
		{Reads: 150, Writes: 12, Fetches: 1500}, // +50/+2/+500
		{Reads: 5, Writes: 2, Fetches: 100},     // engine restart (all decreased)
		{Reads: 25, Writes: 4, Fetches: 300},    // +20/+2/+200
	}
	d := IODeltas(s)
	if d.Reads != 70 || d.Writes != 4 || d.Fetches != 700 {
		t.Fatalf("deltas = %+v", d)
	}
	if d.Resets != 1 {
		t.Fatalf("resets = %d, want 1", d.Resets)
	}
}

func TestAppendReadPruneRestartDurability(t *testing.T) {
	dir := t.TempDir()
	db := "spiketest"

	mk := func(sec int64, size int64) Sample {
		return Sample{Time: sec, SizeBytes: size}
	}
	// "first run": two samples, close (no explicit close needed — Append opens per call)
	if err := Append(dir, db, mk(1700000000, GB)); err != nil {
		t.Fatal(err)
	}
	if err := Append(dir, db, mk(1700003600, GB)); err != nil {
		t.Fatal(err)
	}
	// "restart": new process appends again; history must survive
	if err := Append(dir, db, mk(1700007200, 2*GB)); err != nil {
		t.Fatal(err)
	}
	all, err := Read(dir, db, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[2].SizeBytes != 2*GB {
		t.Fatalf("restart lost history: %+v", all)
	}

	// windowed read
	w, err := Read(dir, db, 1700003600)
	if err != nil || len(w) != 2 {
		t.Fatalf("window = %v err %v", w, err)
	}

	// prune drops the two old ones, keeps the newest
	n, err := Prune(dir, db, time.Unix(1700007200, 0)) // keeps Time >= cutoff
	if err != nil || n != 2 {
		t.Fatalf("prune removed %d err %v, want 2", n, err)
	}
	after, err := Read(dir, db, 0)
	if err != nil || len(after) != 1 || after[0].Time != 1700007200 {
		t.Fatalf("after prune: %+v err %v", after, err)
	}

	// torn trailing line tolerated
	p := filepath.Join(dir, "trends", db+".jsonl")
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"t":1700010800,"size`)
	f.Close()
	torn, err := Read(dir, db, 0)
	if err != nil || len(torn) != 1 {
		t.Fatalf("torn line must be skipped: %v err %v", torn, err)
	}
}

func TestSamplerRunAndTick(t *testing.T) {
	dir := t.TempDir()
	dbs := []DBRef{{ID: "a", Path: "x"}, {ID: "b", Path: ""}}
	calls := map[string]int{}
	fn := func(ctx context.Context, db DBRef) (Sample, error) {
		calls[db.ID]++
		if db.ID == "b" {
			return Sample{}, errors.New("unreachable")
		}
		return Sample{Time: time.Now().Unix(), SizeBytes: 123}, nil
	}
	s := &Sampler{Dir: dir, List: func() []DBRef { return dbs }, Sample: fn}
	ok, failed := s.TickOnce(context.Background())
	if ok != 1 || failed != 1 {
		t.Fatalf("tick ok=%d failed=%d", ok, failed)
	}
	a, err := Read(dir, "a", 0)
	if err != nil || len(a) != 1 || a[0].SizeBytes != 123 {
		t.Fatalf("sample a = %+v err %v", a, err)
	}
	if b, _ := Read(dir, "b", 0); len(b) != 0 {
		t.Fatalf("failed db must not append: %+v", b)
	}
	// interval floor: a tiny interval must still be floored to >= 10s
	s.Interval = time.Millisecond
	if s.interval() < 10*time.Second {
		t.Fatalf("interval floor: %v", s.interval())
	}
}

func safeDays(d *float64) float64 {
	if d == nil {
		return -1
	}
	return *d
}
