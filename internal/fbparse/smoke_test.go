package fbparse

import "testing"

func TestSmokeBasics(t *testing.T) {
	cases := []struct {
		in   string
		verb Verb
		ot   ObjectType
		name string
	}{
		{"SELECT * FROM T", VerbSelect, "", ""},
		{"select 1", VerbSelect, "", ""},
		{"INSERT INTO T (A) VALUES (1)", VerbInsert, ObjTable, "T"},
		{"UPDATE T SET A = 1 WHERE ID = 2", VerbUpdate, ObjTable, "T"},
		{"DELETE FROM T WHERE ID = 2", VerbDelete, ObjTable, "T"},
		{"DROP TABLE T", VerbDrop, ObjTable, "T"},
		{"CREATE TABLE T (A INT)", VerbCreate, ObjTable, "T"},
		{"GRANT SELECT ON T TO U", VerbGrant, ObjTable, "T"},
		{"REVOKE SELECT ON T FROM U", VerbRevoke, ObjTable, "T"},
		{"COMMENT ON TABLE T IS 'x'", VerbComment, ObjTable, "T"},
		{"SET GENERATOR G TO 1", VerbSet, ObjSequence, "G"},
	}
	for _, c := range cases {
		stmts := Parse(c.in + ";")
		if len(stmts) != 1 {
			t.Fatalf("%q: got %d statements", c.in, len(stmts))
		}
		s := stmts[0]
		if s.Verb != c.verb || s.ObjectType != c.ot || s.Object.Name != c.name {
			t.Errorf("%q: got (%s,%s,%q) want (%s,%s,%q) issues=%v", c.in, s.Verb, s.ObjectType, s.Object.Name, c.verb, c.ot, c.name, s.Issues)
		}
		if s.Raw != c.in {
			t.Errorf("%q: Raw=%q", c.in, s.Raw)
		}
	}
}

func TestSmokePSQL(t *testing.T) {
	in := `CREATE PROCEDURE P AS
BEGIN
  INSERT INTO T VALUES (1);
  UPDATE T SET A = 2;
END;`
	stmts := Parse(in)
	if len(stmts) != 1 {
		t.Fatalf("got %d statements: %+v", len(stmts), stmts)
	}
	s := stmts[0]
	if s.Verb != VerbCreate || s.ObjectType != ObjProcedure {
		t.Errorf("got %s %s", s.Verb, s.ObjectType)
	}
	if s.Body == nil || !s.Body.HasDML {
		t.Errorf("body not detected: %+v", s.Body)
	}
}

func TestSmokeExecuteBlock(t *testing.T) {
	in := "EXECUTE BLOCK AS BEGIN DELETE FROM T; END"
	stmts := Parse(in)
	if len(stmts) != 1 {
		t.Fatalf("got %d statements", len(stmts))
	}
	s := stmts[0]
	if s.Verb != VerbExecuteBlock || !s.Mutating {
		t.Errorf("got %s mutating=%v", s.Verb, s.Mutating)
	}
}

func TestSmokeSetTerm(t *testing.T) {
	in := "SET TERM !! ; CREATE PROCEDURE P AS BEGIN INSERT INTO T VALUES (1); END !! SET TERM ; !!"
	stmts := Parse(in)
	if len(stmts) != 3 {
		t.Fatalf("got %d statements: %+v", len(stmts), stmts)
	}
	if stmts[0].Verb != VerbSet || stmts[0].variant != varSetTerm {
		t.Errorf("stmt0: %s %s", stmts[0].Verb, stmts[0].variant)
	}
	if stmts[1].Verb != VerbCreate {
		t.Errorf("stmt1: %s", stmts[1].Verb)
	}
}

func TestSmokeWhere(t *testing.T) {
	s := ParseOne2(t, "UPDATE T SET A = 1 WHERE ID = (SELECT MAX(X) FROM S) AND B = 'x' PLAN (T NATURAL)")
	if s.Where != "WHERE ID = (SELECT MAX(X) FROM S) AND B = 'x'" {
		t.Errorf("Where=%q", s.Where)
	}
	q, ok := s.RowEstimateQuery()
	if !ok || q != "SELECT COUNT(*) FROM T WHERE ID = (SELECT MAX(X) FROM S) AND B = 'x'" {
		t.Errorf("RowEstimateQuery=%q ok=%v", q, ok)
	}
}

func ParseOne2(t *testing.T, in string) Statement {
	t.Helper()
	stmts := Parse(in)
	if len(stmts) != 1 {
		t.Fatalf("%q: %d statements %+v", in, len(stmts), stmts)
	}
	return stmts[0]
}
