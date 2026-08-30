package qlog

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogWritesOneJSONPerLine(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := l.Log(Entry{Tool: "fb_query", Database: "spike5", Query: "SELECT 1 FROM RDB$DATABASE",
			Outcome: "ok", Rows: 1, Stats: &Stats{SeqReads: 1, IdxReads: 2},
			PerTable: []PerTable{{Table: "RDB$DATABASE", SeqReads: 1}}, Plan: "PLAN (RDB$DATABASE NATURAL)"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "query-log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d", len(lines))
	}
	var e Entry
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatalf("line 1 not valid JSON: %v", err)
	}
	if e.Tool != "fb_query" || e.Outcome != "ok" || e.Stats.SeqReads != 1 || e.PerTable[0].Table != "RDB$DATABASE" || e.Plan == "" {
		t.Fatalf("round-trip lost fields: %+v", e)
	}
	if e.Time.IsZero() {
		t.Fatal("time not stamped")
	}
	if bytes.Count(b, []byte("\n")) != 3 {
		t.Fatal("trailing newline discipline broken")
	}
}

func TestReopenAppends(t *testing.T) {
	dir := t.TempDir()
	l1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := l1.Log(Entry{Tool: "fb_query", Query: "SELECT 1", Outcome: "ok"}); err != nil {
		t.Fatal(err)
	}
	l1.Close()
	l2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if err := l2.Log(Entry{Tool: "fb_query", Query: "SELECT 2", Outcome: "ok"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "query-log.jsonl"))
	if got := strings.Count(string(b), "\n"); got != 2 {
		t.Fatalf("want 2 appended lines, got %d", got)
	}
}

func TestQueryTextTruncated(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	huge := strings.Repeat("A", MaxQueryText+1000)
	if err := l.Log(Entry{Query: huge, Outcome: "ok"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "query-log.jsonl"))
	if len(b) > MaxQueryText+512 {
		t.Fatalf("oversized query not truncated: %d bytes", len(b))
	}
	var e Entry
	if err := json.Unmarshal(b[:bytes.IndexByte(b, '\n')], &e); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(e.Query, "[truncated") && !strings.Contains(e.Query, "truncated") {
		t.Fatal("truncation marker missing")
	}
}

func TestNilLoggerNoop(t *testing.T) {
	var l *Logger
	if err := l.Log(Entry{Query: "SELECT 1"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}
