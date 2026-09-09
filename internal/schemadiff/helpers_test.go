package schemadiff

import (
	"database/sql"
	"testing"
)

// P1.6 (test_plan): table tests for the pure helpers.

func TestFirstLine(t *testing.T) {
	cases := []struct {
		ns   sql.NullString
		want string
	}{
		{sql.NullString{Valid: false}, ""},
		{sql.NullString{String: "hello", Valid: true}, "hello"},
		{sql.NullString{String: "  padded  ", Valid: true}, "padded"},
		{sql.NullString{String: "first\r\nsecond", Valid: true}, "first"},
		{sql.NullString{String: "first\nsecond", Valid: true}, "first"},
	}
	for _, c := range cases {
		if got := firstLine(c.ns); got != c.want {
			t.Fatalf("firstLine(%#v)=%q want %q", c.ns, got, c.want)
		}
	}
}

func TestCanonicalType(t *testing.T) {
	cases := []struct {
		name   string
		typ    int64
		sub    string
		length int64
		prec   string
		scale  sql.NullInt64
		want   string
	}{
		{"smallint", 7, "", 0, "", sql.NullInt64{}, "SMALLINT"},
		{"integer", 8, "", 0, "", sql.NullInt64{}, "INTEGER"},
		{"float", 10, "", 0, "", sql.NullInt64{}, "FLOAT"},
		{"double", 27, "", 0, "", sql.NullInt64{}, "DOUBLE PRECISION"},
		{"date", 12, "", 0, "", sql.NullInt64{}, "DATE"},
		{"time", 13, "", 0, "", sql.NullInt64{}, "TIME"},
		{"timestamp", 35, "", 0, "", sql.NullInt64{}, "TIMESTAMP"},
		{"char", 14, "", 10, "", sql.NullInt64{}, "CHAR(10)"},
		{"varchar", 37, "", 32, "", sql.NullInt64{}, "VARCHAR(32)"},
		{"cstring", 40, "", 0, "", sql.NullInt64{}, "CSTRING"},
		{"boolean", 26, "", 0, "", sql.NullInt64{}, "BOOLEAN"},
		{"bigint", 16, "", 0, "", sql.NullInt64{}, "BIGINT"},
		{"int128", 16, "", 0, "long", sql.NullInt64{}, "INT128"},
		{"decfloat16", 24, "", 0, "16", sql.NullInt64{}, "DECFLOAT(16)"},
		{"decfloat34", 24, "", 0, "34", sql.NullInt64{}, "DECFLOAT(34)"},
		{"blob", 261, "", 0, "", sql.NullInt64{}, "BLOB"},
		{"blob text", 261, "1", 0, "", sql.NullInt64{}, "BLOB SUB_TYPE TEXT"},
		{"blob binary", 261, "-1", 0, "", sql.NullInt64{}, "BLOB SUB_TYPE -1"},
		{"numeric scaled", 8, "", 9, "", sql.NullInt64{Int64: 2, Valid: true}, "INTEGER (1,-2)"},
		{"unknown blr", 99, "", 0, "", sql.NullInt64{}, "TYPE_99"},
		{"scale zero ignored", 8, "", 0, "", sql.NullInt64{Int64: 0, Valid: true}, "INTEGER"},
		{"scale invalid", 8, "", 0, "", sql.NullInt64{Valid: false}, "INTEGER"},
	}
	for _, c := range cases {
		if got := canonicalType(c.typ, c.sub, c.length, c.prec, c.scale); got != c.want {
			t.Fatalf("%s: canonicalType=%q want %q", c.name, got, c.want)
		}
	}
}

func TestQuotedListAndContains(t *testing.T) {
	if got := quotedList([]string{"A", "B"}); got != `"A", "B"` {
		t.Fatalf("quotedList=%q", got)
	}
	if got := quotedList(nil); got != "" {
		t.Fatalf("quotedList(nil)=%q", got)
	}
	if !contains([]string{"x"}, "x") || contains(nil, "x") {
		t.Fatal("contains broken")
	}
}

func TestKeyRowAndAddSample(t *testing.T) {
	if got := keyRow([]string{"ID", "TS"}, []string{"7", "2026"}); got != "{ID=7, TS=2026}" {
		t.Fatalf("keyRow=%q", got)
	}
	var dd DataDiff
	for i := 0; i < 12; i++ {
		dd.addSample(&dd.SamplesDiff, &dd.Truncated, 10, "row")
	}
	if len(dd.SamplesDiff) != 10 || !dd.Truncated {
		t.Fatalf("addSample cap: len=%d truncated=%v", len(dd.SamplesDiff), dd.Truncated)
	}
}
