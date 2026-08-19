package classify

import "testing"

func TestScriptTiers(t *testing.T) {
	cases := []struct {
		sql  string
		tier int
		ok   bool
	}{
		{"SELECT * FROM T", 0, true},
		{"INSERT INTO T (A) VALUES (1)", 1, true},
		{"UPDATE T SET A = 1 WHERE B = 2", 1, true},
		{"UPDATE T SET A = 1", 2, true}, // no WHERE
		{"DELETE FROM T", 2, true},      // no WHERE
		{"DROP TABLE T", 2, true},
		{"CREATE TABLE T (A INT)", 1, true},
		{"GRANT SELECT ON T TO ROLE R", 1, true},
		{"DROP DATABASE", 3, true},
		{"EXECUTE BLOCK AS BEGIN INSERT INTO T VALUES (1); END", 2, true}, // low parse confidence escalates one tier
	}
	for _, c := range cases {
		_, tier, why, ok := Script(c.sql)
		if ok != c.ok || tier != c.tier {
			t.Errorf("%q: tier=%d ok=%v (%s), want %d/%v", c.sql, tier, ok, why, c.tier, c.ok)
		}
	}
}

func TestUnknownDenies(t *testing.T) {
	if _, _, msg, ok := Script("INSERT INTO T VALUES (1); ~~~total garbage~~~"); ok {
		t.Fatal("garbage accepted")
	} else if msg == "" {
		t.Fatal("no reason")
	}
}

func TestInjectionStyleBypasses(t *testing.T) {
	if _, tier, _, ok := Script("EXECUTE BLOCK AS BEGIN DELETE FROM T; END"); !ok || tier < 1 {
		t.Fatalf("EXECUTE BLOCK DML misclassified: tier=%d ok=%v", tier, ok)
	}
	if _, tier, _, ok := Script("/* harmless select */ DROP TABLE T"); !ok || tier < 2 {
		t.Fatalf("comment-hidden DROP misclassified: tier=%d ok=%v", tier, ok)
	}
}

func TestMixedTiersDenied(t *testing.T) {
	if _, _, _, ok := Script("INSERT INTO T VALUES (1); DROP TABLE T"); ok {
		t.Fatal("mixed tiers accepted")
	}
}
