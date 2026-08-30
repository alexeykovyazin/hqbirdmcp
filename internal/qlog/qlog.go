// Package qlog implements the fb_query telemetry log: one JSON object per
// line (NDJSON) at <state.dir>/query-log.jsonl. Unlike the audit log
// (hash-chained decisions), qlog is high-volume operational telemetry —
// query text, parameters, access plan, and execution statistics per call —
// so it carries no integrity chain and is readable with any JSONL tooling.
package qlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// MaxQueryText bounds the query text stored per entry (defense against
// pathological inputs bloating the log); longer texts are truncated in full
// (statistics are unaffected).
const MaxQueryText = 64 << 10

// PerTable is one table's record counters for the executed statement
// (Firebird 5+ MON$TABLE_STATS). SeqReads are full-scan (natural) reads,
// IdxReads indexed reads — the same numbers isql's SET PER_TAB shows.
type PerTable struct {
	Table    string `json:"table"`
	SeqReads int64  `json:"seq_reads"`
	IdxReads int64  `json:"idx_reads"`
	Inserts  int64  `json:"inserts"`
	Updates  int64  `json:"updates"`
	Deletes  int64  `json:"deletes"`
	Backouts int64  `json:"backouts"`
	Purges   int64  `json:"purges"`
	Expunges int64  `json:"expunges"`
}

// Stats aggregates the statement's MON$IO_STATS page counters and record
// counters (all supported engines; per-table detail only on FB 5+).
type Stats struct {
	PageReads   int64 `json:"page_reads"`
	PageWrites  int64 `json:"page_writes"`
	PageFetches int64 `json:"page_fetches"`
	PageMarks   int64 `json:"page_marks"`
	SeqReads    int64 `json:"seq_reads"`
	IdxReads    int64 `json:"idx_reads"`
	Inserts     int64 `json:"inserts"`
	Updates     int64 `json:"updates"`
	Deletes     int64 `json:"deletes"`
	Backouts    int64 `json:"backouts"`
	Purges      int64 `json:"purges"`
	Expunges    int64 `json:"expunges"`
}

// Entry is one query-log record. Outcome: ok | error | fallback | denied.
// Plan/Stats/PerTable are nil/empty where unavailable (engine below 5.0,
// denied before execution, failed execution).
type Entry struct {
	Time      time.Time      `json:"time"`
	Tool      string         `json:"tool"`
	Identity  string         `json:"identity"`
	Database  string         `json:"database"`
	Query     string         `json:"query"`
	Params    map[string]any `json:"params,omitempty"`
	Outcome   string         `json:"outcome"`
	Error     string         `json:"error,omitempty"`
	Rows      int            `json:"rows,omitempty"`
	Truncated bool           `json:"truncated,omitempty"`
	ElapsedMS float64        `json:"elapsed_ms,omitempty"`
	Engine    string         `json:"engine,omitempty"`
	Stats     *Stats         `json:"stats,omitempty"`
	PerTable  []PerTable     `json:"per_table_stats,omitempty"`
	Plan      string         `json:"plan,omitempty"`
	PlanError string         `json:"plan_error,omitempty"`
}

// Logger appends entries to <dir>/query-log.jsonl.
type Logger struct {
	mu   sync.Mutex
	path string
	f    *os.File
}

func Open(dir string) (*Logger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "query-log.jsonl")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, err
	}
	return &Logger{path: p, f: f}, nil
}

// Log writes one NDJSON line. Safe on a nil logger (tools keep working when
// telemetry is not wired; the call is then a no-op).
func (l *Logger) Log(e Entry) error {
	if l == nil {
		return nil
	}
	if len(e.Query) > MaxQueryText {
		e.Query = e.Query[:MaxQueryText] + fmt.Sprintf("…[truncated %d bytes]", len(e.Query))
	}
	e.Time = time.Now().UTC()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.f.Write(append(b, '\n')); err != nil {
		return err
	}
	return l.f.Sync()
}

func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
