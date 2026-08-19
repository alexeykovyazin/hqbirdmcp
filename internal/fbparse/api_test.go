package fbparse

import (
	"strings"
	"testing"
)

func TestOptionsWithTerm(t *testing.T) {
	in := "UPDATE T SET A = 1 ## SELECT 'a;b' ## DROP TABLE T"
	stmts := Parse(in, WithTerm("##"))
	if len(stmts) != 3 {
		t.Fatalf("got %d statements", len(stmts))
	}
	if stmts[0].Verb != VerbUpdate || stmts[1].Verb != VerbSelect || stmts[2].Verb != VerbDrop {
		t.Fatalf("verbs: %s %s %s", stmts[0].Verb, stmts[1].Verb, stmts[2].Verb)
	}
	// Alphabetic term matches case-insensitively (isql-style).
	stmts = Parse("SELECT 1 go", WithTerm("GO"))
	if len(stmts) != 1 || stmts[0].Verb != VerbSelect {
		t.Fatalf("got %+v", stmts)
	}
	// Invalid options keep defaults.
	if got := Split("SELECT 1;", WithTerm("")); len(got) != 1 {
		t.Fatalf("empty term: %+v", got)
	}
	if got := Split(strings.Repeat("x", 50), WithMaxBytes(0)); len(got) != 1 {
		t.Fatalf("zero cap: %+v", got)
	}
}

func TestKnownOpKeysShape(t *testing.T) {
	keys := KnownOpKeys()
	if len(keys) < 90 {
		t.Fatalf("only %d keys", len(keys))
	}
	seen := map[OpKey]bool{}
	for _, k := range keys {
		if k.Verb == "" || k.Verb == VerbUnknown {
			t.Fatalf("bad key %+v", k)
		}
		if k.Verb != VerbSelect && k.Verb != VerbExecuteBlock && k.ObjectType == "" && k.Variant == "" {
			t.Fatalf("key without object or variant: %+v", k)
		}
		if seen[k] {
			t.Fatalf("duplicate key %+v", k)
		}
		seen[k] = true
	}
	// Spot-check anchor keys of the v3 vocabulary.
	for _, want := range []OpKey{
		{VerbSelect, "", ""},
		{VerbInsert, ObjTable, ""},
		{VerbUpdate, ObjTable, varOrInsert},
		{VerbCreate, ObjTable, ""},
		{VerbDrop, ObjDatabase, ""},
		{VerbAlter, ObjTable, varColumnType},
		{VerbGrant, ObjTable, varGrantWithOption},
		{VerbRevoke, ObjRole, varRevokeAdminOption},
		{VerbSet, "", varSetTransaction},
		{VerbExecuteBlock, "", ""},
	} {
		if !seen[want] {
			t.Fatalf("missing key %+v", want)
		}
	}
}

func TestStatementStringsAndJSONishFields(t *testing.T) {
	s := mustClassify(t, "GRANT SELECT ON T TO U")
	if string(s.Verb) != "GRANT" || string(s.ObjectType) != "TABLE" {
		t.Fatalf("verb/objtype string forms: %s/%s", s.Verb, s.ObjectType)
	}
	if s.OpKey() != (OpKey{Verb: VerbGrant, ObjectType: ObjTable, Variant: ""}) {
		t.Fatalf("opkey %+v", s.OpKey())
	}
}
