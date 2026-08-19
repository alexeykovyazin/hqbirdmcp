package privs

import "testing"

func TestDiffGrant(t *testing.T) {
	before := []Grant{{User: "U", Privilege: "S", Relation: "T"}}
	after := ApplyPreview(before, "GRANT", "INSERT", "T", "U")
	added, removed := Diff(before, after)
	if len(removed) != 0 || len(added) != 1 || added[0] != "INSERT ON T" {
		t.Fatalf("added=%v removed=%v", added, removed)
	}
}

func TestDiffRevoke(t *testing.T) {
	before := []Grant{
		{User: "U", Privilege: "S", Relation: "T"},
		{User: "U", Privilege: "I", Relation: "T"},
	}
	after := ApplyPreview(before, "REVOKE", "SELECT", "T", "U")
	added, removed := Diff(before, after)
	if len(added) != 0 || len(removed) != 1 || removed[0] != "SELECT ON T" {
		t.Fatalf("added=%v removed=%v after=%s", added, removed, Format(after))
	}
}

func TestFormatEmpty(t *testing.T) {
	if Format(nil) != "(no privileges)" {
		t.Fatal(Format(nil))
	}
}
