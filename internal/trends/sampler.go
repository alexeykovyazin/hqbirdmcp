// Sampler: the periodic collection loop. The engine-specific query
// (MON$DATABASE / MON$ATTACHMENTS / file size) lives behind SampleFn so the
// loop and the analysis are unit-testable without a server.
package trends

import (
	"context"
	"log"
	"time"
)

// DBRef is one managed database to sample.
type DBRef struct {
	ID   string
	Path string // local file path for on-disk size ("" = size unknown)
}

// SampleFn collects one sample for one database. A returned error means
// "skip this tick" (server down, unreachable) — sampling never gives up.
type SampleFn func(ctx context.Context, db DBRef) (Sample, error)

// Sampler periodically appends samples and prunes old ones.
type Sampler struct {
	Dir       string        // state dir; samples land in <dir>/trends
	Interval  time.Duration // tick period (default 5m, floored to 10s)
	Retention time.Duration // prune samples older than this (0 = keep all)
	List      func() []DBRef
	Sample    SampleFn
	// Logger receives one line per tick outcome anomaly; nil = std log.
	Logger *log.Logger
}

func (s *Sampler) interval() time.Duration {
	if s.Interval < 10*time.Second {
		return 10 * time.Second
	}
	return s.Interval
}

// Run blocks until ctx is done: tick immediately, then every interval;
// prune once at start and then once per day.
func (s *Sampler) Run(ctx context.Context) {
	if s.List == nil || s.Sample == nil {
		return
	}
	s.pruneAll()
	lastPrune := time.Now()
	s.tick(ctx)
	t := time.NewTicker(s.interval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick(ctx)
			if s.Retention > 0 && time.Since(lastPrune) >= 24*time.Hour {
				s.pruneAll()
				lastPrune = time.Now()
			}
		}
	}
}

// TickOnce runs one collection pass over every database — used by tests and
// by live verification without waiting for the ticker.
func (s *Sampler) TickOnce(ctx context.Context) (ok, failed int) {
	return s.tick(ctx)
}

func (s *Sampler) tick(ctx context.Context) (ok, failed int) {
	for _, db := range s.List() {
		sample, err := s.Sample(ctx, db)
		if err != nil {
			failed++
			s.logf("trends: %s: skip: %v", db.ID, err)
			continue
		}
		if err := Append(s.Dir, db.ID, sample); err != nil {
			failed++
			s.logf("trends: %s: append: %v", db.ID, err)
			continue
		}
		ok++
	}
	return ok, failed
}

func (s *Sampler) pruneAll() {
	for _, db := range s.List() {
		if n, err := Prune(s.Dir, db.ID, time.Now().Add(-s.Retention)); err != nil {
			s.logf("trends: %s: prune: %v", db.ID, err)
		} else if n > 0 {
			s.logf("trends: %s: pruned %d sample(s)", db.ID, n)
		}
	}
}

func (s *Sampler) logf(format string, args ...any) {
	if s.Logger != nil {
		s.Logger.Printf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}
