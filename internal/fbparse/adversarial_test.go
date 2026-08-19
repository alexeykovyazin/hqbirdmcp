package fbparse

import (
	"strings"
	"testing"
)

// The adversarial corpus (§7): every case must classify safely-as-Unknown
// or correctly — never a silent misparse.

func mustClassify(t *testing.T, in string) Statement {
	t.Helper()
	stmts := Parse(in)
	if len(stmts) != 1 {
		t.Fatalf("%q: got %d statements: %+v", in, len(stmts), stmts)
	}
	return stmts[0]
}

func TestAdversarialExecuteBlockDML(t *testing.T) {
	// The classic naive-filter bypass: EXECUTE BLOCK with INSERT.
	s := mustClassify(t, "EXECUTE BLOCK AS BEGIN INSERT INTO T VALUES (1); END")
	if s.Verb != VerbExecuteBlock || !s.Mutating {
		t.Fatalf("verb=%s mutating=%v", s.Verb, s.Mutating)
	}
	if s.Body == nil || !s.Body.HasDML {
		t.Fatal("HasDML not detected")
	}
	if IsReadOnly("EXECUTE BLOCK AS BEGIN INSERT INTO T VALUES (1); END") {
		t.Fatal("IsReadOnly accepted an EXECUTE BLOCK")
	}
	// Read-only-looking block is still mutating (conservative).
	s = mustClassify(t, "EXECUTE BLOCK RETURNS (X INT) AS BEGIN X = 1; SUSPEND; END")
	if !s.Mutating {
		t.Fatal("EXECUTE BLOCK must stay mutating")
	}
}

func TestAdversarialCommentsHidingVerbs(t *testing.T) {
	s := mustClassify(t, "/* DROP TABLE T */ DELETE FROM T")
	if s.Verb != VerbDelete || s.Object.Name != "T" {
		t.Fatalf("verb=%s name=%s", s.Verb, s.Object.Name)
	}
	s = mustClassify(t, "-- CREATE TABLE x (a int)\nSELECT 1 FROM R")
	if s.Verb != VerbSelect {
		t.Fatalf("verb=%s", s.Verb)
	}
}

func TestAdversarialQuotedIdentifiers(t *testing.T) {
	// Identifier containing the terminator and a line-comment opener.
	s := mustClassify(t, `SELECT * FROM "we;ird--name"`)
	if s.Verb != VerbSelect {
		t.Fatalf("verb=%s", s.Verb)
	}
	// Multiple statements: the quoted ';' must not split.
	stmts := Parse(`SELECT 1; SELECT * FROM "a;b"; SELECT 2`)
	if len(stmts) != 3 {
		t.Fatalf("got %d statements", len(stmts))
	}
	if stmts[1].Object.Name != "" { // reads carry no object
		t.Fatalf("unexpected object %q", stmts[1].Object.Name)
	}
	// Unicode identifier.
	s = mustClassify(t, `UPDATE "Таблица" SET A = 1`)
	if s.Verb != VerbUpdate || s.Object.Name != "Таблица" || !s.Object.Quoted {
		t.Fatalf("verb=%s name=%q quoted=%v", s.Verb, s.Object.Name, s.Object.Quoted)
	}
}

func TestAdversarialUnicodeConfusableVerbs(t *testing.T) {
	// Cyrillic-lookalike CREATE (P6.1 T2 suite): must not match CREATE.
	for _, in := range []string{
		"СREATE TABLE T (A INT)",   // Cyrillic Es
		"SЕLECT * FROM T",          // Cyrillic Ye
		"DROР TABLE T",             // Cyrillic Er
		"ІNSERT INTO T VALUES (1)", // Cyrillic I
	} {
		s := mustClassify(t, in)
		if s.Verb != VerbUnknown || s.Confidence != ConfidenceLow {
			t.Fatalf("%q: verb=%s conf=%d", in, s.Verb, s.Confidence)
		}
		if !s.Mutating {
			t.Fatalf("%q: Unknown must be conservatively mutating", in)
		}
	}
}

func TestAdversarialDialect1(t *testing.T) {
	// Strong signals → Unknown with issue (FR-3).
	for _, in := range []string{
		`INSERT INTO T VALUES ("lit")`,
		`INSERT INTO T (A, B) VALUES (1, "lit", F(2, "x"))`,
		`SELECT "" FROM T`,
	} {
		s := mustClassify(t, in)
		if s.Verb != VerbUnknown {
			t.Fatalf("%q: strong dialect-1 signal must yield Unknown, got %s", in, s.Verb)
		}
		found := false
		for _, iss := range s.Issues {
			if iss.Kind == IssueDialect1Quoting {
				found = true
			}
		}
		if !found {
			t.Fatalf("%q: no IssueDialect1Quoting", in)
		}
	}

	// Weak signals (value positions with a legal dialect-3 reading):
	// classified but flagged and degraded.
	for _, in := range []string{
		`SELECT * FROM T WHERE NAME = "O'Brien"`,
		`SELECT 'a' || "b" FROM T`,
		`SELECT * FROM T WHERE NAME LIKE "pattern"`,
	} {
		s := mustClassify(t, in)
		if s.Verb != VerbSelect {
			t.Fatalf("%q: weak signal changed the verb: %s", in, s.Verb)
		}
		if s.Confidence != ConfidenceLow {
			t.Fatalf("%q: weak signal must degrade confidence: %d", in, s.Confidence)
		}
	}

	// Explicit dialect-1 mode: lenient parse with flag (NFR-7).
	s2 := Parse(`SELECT * FROM T WHERE NAME = 'x'`, WithDialect(Dialect1))[0]
	if s2.Verb != VerbSelect || s2.Confidence != ConfidenceHigh {
		t.Fatalf("d1 mode: verb=%s conf=%d", s2.Verb, s2.Confidence)
	}
	s3 := Parse(`SELECT * FROM T WHERE NAME = "x"`, WithDialect(Dialect1))[0]
	if s3.Verb != VerbSelect {
		t.Fatalf("d1 lenient: verb=%s", s3.Verb)
	}
	hasD1 := false
	for _, iss := range s3.Issues {
		if iss.Kind == IssueDialect1Quoting {
			hasD1 = true
		}
	}
	if !hasD1 {
		t.Fatal("d1 lenient parse not flagged")
	}
	// In dialect-1 mode the double-quoted literal is scrubbed by Template.
	if got := s3.Template(); strings.Count(got, `"x"`) != 0 {
		t.Fatalf("d1 template kept the literal: %q", got)
	}
}

func TestAdversarialUnterminated(t *testing.T) {
	for _, in := range []string{
		"SELECT 'abc",
		`SELECT * FROM "abc`,
		"SELECT 1 /* unterminated",
		"UPDATE T SET A = 'x",
	} {
		s := mustClassify(t, in)
		if s.Verb != VerbUnknown || s.Confidence != ConfidenceLow {
			t.Fatalf("%q: verb=%s conf=%d", in, s.Verb, s.Confidence)
		}
		found := false
		for _, iss := range s.Issues {
			if iss.Kind == IssueUnclosedLiteral {
				found = true
			}
		}
		if !found {
			t.Fatalf("%q: no unclosed-literal issue (%v)", in, s.Issues)
		}
	}
	// Unterminated comment swallowing the rest still leaves prior
	// statements intact; the affected statement is Unknown per FR-3.
	stmts := Parse("SELECT 1; SELECT 2 /* swallow")
	if len(stmts) != 2 {
		t.Fatalf("got %d statements", len(stmts))
	}
	if stmts[0].Verb != VerbSelect || len(stmts[0].Issues) != 0 {
		t.Fatalf("stmt0 verb=%s issues=%v", stmts[0].Verb, stmts[0].Issues)
	}
	if stmts[1].Verb != VerbUnknown {
		t.Fatalf("stmt1 verb=%s issues=%v", stmts[1].Verb, stmts[1].Issues)
	}
	// Whole input is one unterminated comment → single Unknown statement.
	stmts = Parse("/* only")
	if len(stmts) != 1 || stmts[0].Verb != VerbUnknown {
		t.Fatalf("got %+v", stmts)
	}
}

func TestAdversarialEmptyInput(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t\r\n", "-- just a comment", ";"} {
		if got := Parse(in); len(got) != 0 {
			t.Errorf("%q: got %d statements", in, len(got))
		}
		if _, err := ParseOne(in); err != ErrEmptyInput {
			t.Errorf("%q: err=%v want ErrEmptyInput", in, err)
		}
		if IsReadOnly(in) {
			t.Errorf("%q: IsReadOnly accepted", in)
		}
	}
}

func TestAdversarialNestedBeginEnd(t *testing.T) {
	in := `CREATE PROCEDURE P AS
DECLARE V INT;
BEGIN
  V = CASE WHEN 1 = 1 THEN 1 ELSE 2 END;
  IF (V = 1) THEN
  BEGIN
    IF (V = 2) THEN
    BEGIN
      DELETE FROM T;
    END
  END
  WHILE (V < 3) DO
  BEGIN
    V = V + 1;
  END
END`
	s := mustClassify(t, in)
	if s.Verb != VerbCreate || s.ObjectType != ObjProcedure {
		t.Fatalf("verb=%s objtype=%s", s.Verb, s.ObjectType)
	}
	if s.Body == nil || !s.Body.HasDML {
		t.Fatal("nested body DML not detected")
	}
}

func TestAdversarialMergeSubSelect(t *testing.T) {
	s := mustClassify(t, "MERGE INTO T USING (SELECT ID FROM S WHERE A = 1) SRC ON T.ID = SRC.ID WHEN MATCHED THEN DELETE")
	if s.Verb != VerbMerge || s.Object.Name != "T" {
		t.Fatalf("verb=%s name=%s", s.Verb, s.Object.Name)
	}
	if s.variant != "USING_SUBQUERY" {
		t.Fatalf("variant=%s", s.variant)
	}
	if len(s.Secondary) != 1 || s.Secondary[0].Name != "S" {
		t.Fatalf("secondary=%v", s.Secondary)
	}
	if s.Confidence != ConfidenceLow {
		t.Fatalf("conf=%d", s.Confidence)
	}
}

func TestAdversarialLookalikes(t *testing.T) {
	for _, in := range []string{"TRUNCATE TABLE T", "USE MYDB", "COMMIT", "ROLLBACK", "SAVEPOINT S", "CONNECT 'x'", "CALL P(1)"} {
		s := mustClassify(t, in)
		if s.Verb != VerbUnknown {
			t.Errorf("%q: verb=%s want UNKNOWN", in, s.Verb)
		}
		if !s.Mutating {
			t.Errorf("%q: Unknown must be mutating", in)
		}
	}
}

func TestAdversarialIsReadOnlyFalseOnDoubt(t *testing.T) {
	falseCases := []string{
		"SELECT 1; SELECT 2",
		"SELECT * FROM T; DELETE FROM T",
		"DELETE FROM T",
		"WITH X AS (SELECT 1) SELECT * FROM X; SELECT 2",
		"SELECT * FROM T WITH LOCK",
		"UPDATE T SET A = 1",
		"",
		"SELECT 'unterminated",
		`SELECT * FROM T WHERE A = "x"`, // dialect-1 weak signal
		"EXECUTE BLOCK RETURNS (X INT) AS BEGIN X = 1; SUSPEND; END",
		"SET TERM !! ;",
	}
	for _, in := range falseCases {
		if IsReadOnly(in) {
			t.Errorf("IsReadOnly(%q) = true", in)
		}
	}
	trueCases := []string{
		"SELECT * FROM T",
		"select 1",
		"WITH C AS (SELECT 1 AS X FROM R) SELECT * FROM C",
		"SELECT * FROM T WHERE A = 'it''s'",
	}
	for _, in := range trueCases {
		if !IsReadOnly(in) {
			t.Errorf("IsReadOnly(%q) = false", in)
		}
	}
}

func TestAdversarialInvalidUTF8(t *testing.T) {
	s := mustClassify(t, "\xff\xfe SELECT 1")
	if s.Verb != VerbUnknown || s.Confidence != ConfidenceLow {
		t.Fatalf("verb=%s conf=%d", s.Verb, s.Confidence)
	}
	found := false
	for _, iss := range s.Issues {
		if iss.Kind == IssueInvalidUTF8 {
			found = true
		}
	}
	if !found {
		t.Fatal("no invalid-UTF-8 issue")
	}
	if IsReadOnly("\xff SELECT 1") {
		t.Fatal("IsReadOnly accepted invalid UTF-8")
	}
}

func TestAdversarialParseOne(t *testing.T) {
	if _, err := ParseOne("SELECT 1; SELECT 2"); err != ErrMultipleStatements {
		t.Errorf("err=%v", err)
	}
	if _, err := ParseOne(strings.Repeat("x", 100), WithMaxBytes(10)); err != ErrTooLarge {
		t.Errorf("err=%v", err)
	}
	if got := Parse(strings.Repeat("SELECT 1; ", 100), WithMaxBytes(10)); len(got) != 1 || got[0].Verb != VerbUnknown {
		t.Errorf("oversize Parse: %+v", got)
	}
	if got := Split(strings.Repeat("SELECT 1; ", 100), WithMaxBytes(10)); got != nil {
		t.Errorf("oversize Split: %+v", got)
	}
	s, err := ParseOne("  SELECT 1  ")
	if err != nil || s.Verb != VerbSelect {
		t.Errorf("err=%v verb=%s", err, s.Verb)
	}
}

func TestAdversarialLiteralsAndTemplate(t *testing.T) {
	// Adjacent segments concatenate (Parser.cpp StrMark loop): one '?'.
	s := mustClassify(t, "SELECT 'a' 'b' FROM T")
	if got := s.Template(); got != "SELECT ? FROM T" {
		t.Fatalf("template=%q", got)
	}
	// Escapes are preserved verbatim outside literals.
	s = mustClassify(t, `UPDATE "T""X" SET A = 'va''l'`)
	if got := s.Template(); got != `UPDATE "T""X" SET A = ?` {
		t.Fatalf("template=%q", got)
	}
	if s.Object.Name != `T"X` {
		t.Fatalf("name=%q", s.Object.Name)
	}
	// Hex string constants.
	s = mustClassify(t, "INSERT INTO T VALUES (X'0A0B')")
	if got := s.Template(); got != "INSERT INTO T VALUES (?)" {
		t.Fatalf("template=%q", got)
	}
	// Numeric literals survive (name positions, DROP SHADOW n).
	s = mustClassify(t, "DROP SHADOW 7")
	if got := s.Template(); got != "DROP SHADOW 7" {
		t.Fatalf("template=%q", got)
	}
	// Introducer strings: the literal part is scrubbed.
	s = mustClassify(t, "_UTF8 'x'")
	if got := s.Template(); got != "_UTF8 ?" {
		t.Fatalf("template=%q", got)
	}
	// Comments between segments are part of the literal token.
	s = mustClassify(t, "SELECT 'a' /* join */ 'b' FROM T")
	if got := s.Template(); got != "SELECT ? FROM T" {
		t.Fatalf("template=%q", got)
	}
}

func TestAdversarialRowEstimate(t *testing.T) {
	// Positioned update cannot produce a searched COUNT.
	s := mustClassify(t, "UPDATE T SET A = 1 WHERE CURRENT OF C")
	if _, ok := s.RowEstimateQuery(); ok {
		t.Fatal("positioned update produced a count query")
	}
	// No WHERE → full-table count is still safe.
	s = mustClassify(t, "UPDATE T SET A = 1")
	q, ok := s.RowEstimateQuery()
	if !ok || q != "SELECT COUNT(*) FROM T" {
		t.Fatalf("q=%q ok=%v", q, ok)
	}
	// Quoted identifiers re-quoted exactly.
	s = mustClassify(t, `UPDATE "My Table" SET A = 1 WHERE ID = 5`)
	q, ok = s.RowEstimateQuery()
	if !ok || q != `SELECT COUNT(*) FROM "My Table" WHERE ID = 5` {
		t.Fatalf("q=%q ok=%v", q, ok)
	}
	// Reads never produce counts.
	s = mustClassify(t, "SELECT * FROM T WHERE A = 1")
	if _, ok := s.RowEstimateQuery(); ok {
		t.Fatal("SELECT produced a count query")
	}
	// Upsert has no search-WHERE semantics.
	s = mustClassify(t, "UPDATE OR INSERT INTO T (A) VALUES (1)")
	if _, ok := s.RowEstimateQuery(); ok {
		t.Fatal("UPDATE OR INSERT produced a count query")
	}
}
