package executor

import "testing"

// WS3.1: exclusive-reservation detection for materialized-view refreshes.
func TestPrepareNeedsExclusive(t *testing.T) {
	cases := []struct {
		sql  string
		want bool
	}{
		{"REFRESH MATERIALIZED VIEW MV", true},
		{"REFRESH MATERIALIZED VIEW MV DROP DATA", true},
		{"REFRESH MATERIALIZED VIEW MV CASCADE", true},
		{"REFRESH MATERIALIZED VIEW MV CONCURRENTLY", false},
		{"REFRESH MATERIALIZED VIEW MV CONCURRENTLY CASCADE", false},
		{"CREATE MATERIALIZED VIEW MV AS SELECT 1 FROM RDB$DATABASE", false},
		{"UPDATE T SET A = 1 WHERE B = 2", false},
	}
	for _, c := range cases {
		p, err := Prepare(c.sql)
		if err != nil {
			t.Fatalf("%q: %v", c.sql, err)
		}
		if p.NeedsExclusive != c.want {
			t.Errorf("%q: NeedsExclusive=%v want %v", c.sql, p.NeedsExclusive, c.want)
		}
	}
}
