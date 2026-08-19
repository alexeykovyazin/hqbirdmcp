package fbparse

import (
	"strings"
	"testing"
)

// propertyCorpus feeds the property and fuzz seed corpora.
var propertyCorpus = []string{
	"SELECT * FROM T",
	"SELECT 'a;b' FROM T WHERE X = '--not-a-comment'",
	"INSERT INTO T (A) VALUES ('x; y')",
	"UPDATE T SET A = 1 WHERE B = 'semi; colon'",
	"DELETE FROM T",
	"MERGE INTO T USING S ON T.ID = S.ID WHEN MATCHED THEN UPDATE SET A = 1",
	"CREATE TABLE T (A INT, B VARCHAR(10), C \"WEIRD\" DEFAULT 'q;q')",
	"DROP TABLE T",
	"ALTER TABLE T ADD C INT",
	"ALTER TABLE T ALTER COLUMN C TYPE VARCHAR(50)",
	"GRANT SELECT, UPDATE (A) ON T TO U1, U2 WITH GRANT OPTION",
	"REVOKE GRANT OPTION FOR SELECT ON T FROM U",
	"COMMENT ON COLUMN T.C IS 'some; comment'",
	"SET TERM !! ;",
	"SET GENERATOR G TO 42",
	"SET TRANSACTION SNAPSHOT",
	"EXECUTE BLOCK AS BEGIN INSERT INTO X VALUES (1); END",
	"CREATE PROCEDURE P (A INT) AS DECLARE V INT; BEGIN V = A; IF (V > 0) THEN BEGIN UPDATE T SET B = V; END END",
	"CREATE TRIGGER TR FOR EMP BEFORE INSERT AS BEGIN NEW.A = 1; END",
	"CREATE PACKAGE PKG AS BEGIN FUNCTION F(A INT) RETURNS INT; END",
	"CREATE INDEX I ON T (UPPER(NAME))",
	"WITH C AS (SELECT 1 AS X FROM R) SELECT * FROM C",
	"  spaced   out   ;   statements   ",
	"",
	";",
	"-- only a comment",
	"/* block ; comment */ SELECT 1",
	"SELECT X'0A0B' FROM T",
	"SELECT _UTF8 'unicode' FROM T",
	"СREATE TABLE T (A INT)",
	"SELECT 'unterminated",
	`SELECT "quoted;ident--name" FROM T`,
	strings.Repeat("SELECT 1; ", 200),
	"UPDATE T SET A = 'x' WHERE ID = (SELECT MAX(I) FROM S)",
}

// TestPropertySpansExact verifies: input[span.Start:span.End] == Raw,
// spans ascending and disjoint, and the bytes outside spans lex only to
// terminator tokens (whitespace and comments produce none) — the
// lossless-split property of §7.
func TestPropertySpansExact(t *testing.T) {
	cfg := config{term: ";"}
	checkGap := func(in, gap string) {
		toks, _ := lexAll(gap, &cfg)
		for _, tk := range toks {
			if !(tk.kind == tkSymbol && tk.text(gap) == ";") {
				t.Fatalf("%q: significant bytes lost in %q (token %q)", in, gap, tk.text(gap))
			}
		}
	}
	for _, in := range propertyCorpus {
		stmts := Parse(in)
		prevEnd := 0
		for _, s := range stmts {
			if s.Span.Start < prevEnd || s.Span.End <= s.Span.Start {
				t.Fatalf("%q: bad span %+v after %d", in, s.Span, prevEnd)
			}
			if in[s.Span.Start:s.Span.End] != s.Raw {
				t.Fatalf("%q: span slice != Raw (%q)", in, s.Raw)
			}
			checkGap(in, in[prevEnd:s.Span.Start])
			prevEnd = s.Span.End
		}
		checkGap(in, in[prevEnd:])
	}
}

// TestPropertySplitMatchesParse: Split spans are exactly Parse spans.
func TestPropertySplitMatchesParse(t *testing.T) {
	for _, in := range propertyCorpus {
		spans := Split(in)
		stmts := Parse(in)
		if len(spans) != len(stmts) {
			t.Fatalf("%q: Split=%d Parse=%d", in, len(spans), len(stmts))
		}
		for k := range spans {
			if spans[k] != stmts[k].Span {
				t.Fatalf("%q: span %d %+v != %+v", in, k, spans[k], stmts[k].Span)
			}
		}
	}
}

// TestPropertyTemplateEquivalence: literals are semantically inert for
// classification (§5 semantic contracts).
func TestPropertyTemplateEquivalence(t *testing.T) {
	for _, in := range propertyCorpus {
		base := Parse(in)
		for _, s := range base {
			tmpl := s.Template()
			re := Parse(tmpl)
			if len(re) != 1 {
				t.Fatalf("template of %q split into %d: %q", s.Raw, len(re), tmpl)
			}
			r := re[0]
			if r.Verb != s.Verb || r.ObjectType != s.ObjectType || r.variant != s.variant {
				t.Fatalf("template drift for %q → %q: (%s,%s,%s) vs (%s,%s,%s)",
					s.Raw, tmpl, r.Verb, r.ObjectType, r.variant, s.Verb, s.ObjectType, s.variant)
			}
			if r.Mutating != s.Mutating {
				t.Fatalf("template mutating drift for %q", s.Raw)
			}
			if r.Object.Name != s.Object.Name {
				t.Fatalf("template name drift for %q: %q vs %q", s.Raw, r.Object.Name, s.Object.Name)
			}
			if r.Column != nil && (s.Column == nil || r.Column.Name != s.Column.Name) {
				t.Fatalf("template column drift for %q", s.Raw)
			}
		}
	}
}

// TestPropertyNoSilentUnknownAsRead: no Unknown statement is ever
// non-mutating, and no read verb carries issues at High confidence.
func TestPropertyNoSilentUnknownAsRead(t *testing.T) {
	for _, in := range propertyCorpus {
		for _, s := range Parse(in) {
			if s.Verb == VerbUnknown && !s.Mutating {
				t.Fatalf("%q: Unknown statement must be conservatively mutating", s.Raw)
			}
			if s.Verb == VerbSelect && s.Mutating {
				t.Fatalf("%q: SELECT classified mutating", s.Raw)
			}
			if s.Confidence == ConfidenceHigh && len(s.Issues) > 0 {
				t.Fatalf("%q: High confidence with issues %v", s.Raw, s.Issues)
			}
		}
	}
}

// TestPropertyKnownOpKeysComplete: every OpKey emittable over the corpus
// is a known key (one direction of the §7 drift gate).
func TestPropertyKnownOpKeysComplete(t *testing.T) {
	for _, in := range propertyCorpus {
		for _, s := range Parse(in) {
			if s.Verb == VerbUnknown {
				continue
			}
			if !knownOpKey(s.OpKey()) {
				t.Fatalf("%q: OpKey %+v not in KnownOpKeys", s.Raw, s.OpKey())
			}
		}
	}
}
