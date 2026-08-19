package fbparse

import (
	"strings"
	"testing"
)

// FuzzParse: never panic on any input; spans stay exact (NFR-2, §7).
// Seed corpus from the adversarial set — these feed P6.1's continuous
// fuzzing lane.
func FuzzParse(f *testing.F) {
	for _, s := range propertyCorpus {
		f.Add(s)
	}
	f.Add("EXECUTE BLOCK (x INT = ?) AS BEGIN IF (x = 1) THEN BEGIN DELETE FROM T; END END")
	f.Add("SET TERM !! ; EXECUTE BLOCK AS BEGIN INSERT INTO T VALUES (1); END !! SET TERM ; !!")
	f.Add("\x00\x01\x02\xff\xfe")
	f.Add("'{{{{")
	f.Add(`"""`)
	f.Add("x'0")
	f.Add("0x")
	f.Add(strings.Repeat("(", 1000) + strings.Repeat(")", 1000))
	f.Add(strings.Repeat("BEGIN ", 500) + strings.Repeat("END ", 500))
	f.Add(strings.Repeat("CASE ", 500) + strings.Repeat("END ", 500))

	f.Fuzz(func(t *testing.T, in string) {
		if len(in) > 1<<20 {
			return // keep iterations fast; size cap covered elsewhere
		}
		stmts := Parse(in)
		prevEnd := 0
		for _, s := range stmts {
			if s.Span.Start < prevEnd || s.Span.End > len(in) || s.Span.End <= s.Span.Start {
				t.Fatalf("bad span %+v (len %d, prev %d)", s.Span, len(in), prevEnd)
			}
			if in[s.Span.Start:s.Span.End] != s.Raw {
				t.Fatalf("span slice mismatch")
			}
			if s.Verb == VerbUnknown && !s.Mutating {
				t.Fatalf("Unknown non-mutating")
			}
			_ = s.Template()
			_, _ = s.RowEstimateQuery()
			_ = s.OpKey()
			prevEnd = s.Span.End
		}
		_ = IsReadOnly(in)
	})
}

// FuzzSplit: Split agrees with Parse's spans on any input.
func FuzzSplit(f *testing.F) {
	for _, s := range propertyCorpus {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		if len(in) > 1<<20 {
			return
		}
		spans := Split(in)
		stmts := Parse(in)
		if len(spans) != len(stmts) {
			t.Fatalf("Split=%d Parse=%d for %q", len(spans), len(stmts), in)
		}
		for k := range spans {
			if spans[k] != stmts[k].Span {
				t.Fatalf("span %d mismatch for %q", k, in)
			}
		}
	})
}
