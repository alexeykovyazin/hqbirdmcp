package classify

import (
	"strings"
	"testing"
)

func TestP61AdversarialClassifier(t *testing.T) {
	cases := []struct {
		sql      string
		minTier  int
		mustDeny bool
	}{
		{"EXECUTE BLOCK AS BEGIN DELETE FROM T; END", 1, false},
		{"EXECUTE BLOCK (x INT = 1) AS BEGIN INSERT INTO T VALUES (x); END", 1, false},
		{"/* SELECT 1 */ DROP TABLE T", 2, false},
		{"-- DROP TABLE\nSELECT 1 FROM T", 0, false},
		{`UPDATE T SET A = 1 WHERE B = U&'\0041'`, 1, false},
		{"CREATE TABLE T (A INT); DROP TABLE T", 0, true}, // mixed tiers
	}
	for _, c := range cases {
		_, tier, why, ok := Script(c.sql)
		if c.mustDeny {
			if ok {
				t.Errorf("%q: mixed/unknown accepted tier=%d", c.sql, tier)
			}
			continue
		}
		if !ok {
			t.Errorf("%q: denied (%s)", c.sql, why)
			continue
		}
		if tier < c.minTier {
			t.Errorf("%q: tier %d < %d", c.sql, tier, c.minTier)
		}
	}
	if _, _, _, ok := Script(strings.Repeat("SELECT 1;\n", 60)); ok {
		t.Fatal("statement cap not enforced")
	}
}

func FuzzScript(f *testing.F) {
	f.Add("SELECT 1 FROM T")
	f.Add("EXECUTE BLOCK AS BEGIN DELETE FROM T; END")
	f.Add("/* DROP */ SELECT 1")
	f.Add("DROP DATABASE")
	f.Fuzz(func(t *testing.T, in string) {
		if len(in) > 1<<16 {
			return
		}
		_, _, _, _ = Script(in)
	})
}
