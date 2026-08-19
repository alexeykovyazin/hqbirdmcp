package classify

import (
	"fmt"
	"testing"

	"github.com/aleks/fbmcp/internal/fbparse"
	"github.com/aleks/fbmcp/internal/policy"
)

// Canonical statements for every v3 write-row that P4.1 classifies.
// Drift-tested: the mapped v3 op's TierForRisk plus ADR-019 escalations
// must match the expected tier. CI-blocking (phase4_plan.md P4.1 T7).
var canonical = []struct {
	sql  string
	v3   int
	tier int
	note string
}{
	{"CREATE TABLE T (A INT)", 11, 1, "create table"},
	{"DROP TABLE T", 11, 2, "drop escalated"},
	{"ALTER TABLE T ADD B INT", 12, 1, "add column"},
	{"ALTER TABLE T DROP B", 12, 1, "drop column"},
	{"ALTER TABLE T ALTER COLUMN A TYPE VARCHAR(20)", 13, 2, "type change restore-point"},
	{"ALTER TABLE T ALTER COLUMN A TO B", 14, 1, "rename column"},
	{"ALTER TABLE T ADD CONSTRAINT PK_T PRIMARY KEY (A)", 16, 1, "pk"},
	{"ALTER TABLE T ADD CONSTRAINT FK_T FOREIGN KEY (A) REFERENCES U (A)", 17, 1, "fk"},
	{"ALTER TABLE T ADD CHECK (A > 0)", 18, 1, "check"},
	{"CREATE INDEX I ON T (A)", 19, 1, "create index"},
	{"DROP INDEX I", 19, 2, "drop index escalated"},
	{"ALTER INDEX I INACTIVE", 20, 1, "deactivate"},
	{"SET STATISTICS INDEX I", 21, 1, "set statistics"},
	{"CREATE VIEW V AS SELECT A FROM T", 22, 1, "view"},
	{"CREATE SEQUENCE S", 24, 1, "sequence"},
	{"CREATE PROCEDURE P AS BEGIN END", 25, 1, "procedure"},
	{"CREATE TRIGGER TR FOR T BEFORE INSERT AS BEGIN END", 27, 1, "trigger"},
	{"CREATE DOMAIN D AS INT", 29, 1, "domain"},
	{"CREATE USER U PASSWORD 'x'", 31, 1, "user"},
	{"DROP USER U", 31, 2, "drop user escalated"},
	{"CREATE ROLE R", 34, 1, "role"},
	{"GRANT SELECT ON T TO U", 37, 1, "grant"},
	{"REVOKE SELECT ON T FROM U", 37, 1, "revoke"},
	{"INSERT INTO T (A) VALUES (1)", 102, 1, "insert"},
	{"UPDATE T SET A = 1 WHERE B = 2", 102, 1, "update where"},
	{"DELETE FROM T WHERE A = 1", 102, 1, "delete where"},
	{"DELETE FROM T", 102, 2, "delete no where"},
	{"COMMENT ON TABLE T IS 'n'", 109, 1, "comment"},
	{"SET TRANSACTION LOCK TIMEOUT 5", 43, 1, "lock timeout"},
	{"DROP DATABASE", 8, 3, "drop database"},
	// P7.4 (phase7_plan.md): materialized views.
	{"CREATE MATERIALIZED VIEW MV AS SELECT A FROM T", 23, 1, "create materialized view"},
	{"ALTER MATERIALIZED VIEW MV AS SELECT A FROM T TO NOT MATERIALIZED", 22, 1, "MV -> view conversion, same class as ALTER VIEW"},
	{"ALTER VIEW MV AS SELECT A FROM T TO MATERIALIZED", 22, 1, "view -> MV conversion, same class as ALTER VIEW"},
	{"REFRESH MATERIALIZED VIEW MV", 23, 1, "refresh, exclusive mode"},
	{"REFRESH MATERIALIZED VIEW MV CONCURRENTLY", 23, 1, "refresh, concurrent mode"},
	{"REFRESH MATERIALIZED VIEW MV CASCADE", 23, 2, "refresh CASCADE escalates to tier 2"},
}

func TestCanonicalV3Matrix(t *testing.T) {
	for _, c := range canonical {
		results, tier, why, ok := Script(c.sql)
		if !ok {
			t.Errorf("%s: denied (%s)", c.note, why)
			continue
		}
		if tier != c.tier {
			t.Errorf("%s: tier=%d want %d (%s) sql=%q", c.note, tier, c.tier, why, c.sql)
		}
		if len(results) != 1 {
			t.Errorf("%s: %d statements", c.note, len(results))
			continue
		}
		if c.v3 != 0 && results[0].V3Op != 0 && results[0].V3Op != c.v3 {
			t.Errorf("%s: v3 op %d want %d", c.note, results[0].V3Op, c.v3)
		}
		if c.v3 > 0 {
			if op, ok := v3Op(c.v3); ok {
				_ = policy.TierForRisk(op) // mapping exists
			} else {
				t.Errorf("%s: v3 row %d missing from generated table", c.note, c.v3)
			}
		}
		// parser must recognize the verb (injection battery seed)
		if results[0].Statement.Verb == fbparse.VerbUnknown {
			t.Errorf("%s: unknown verb", c.note)
		}
	}
}

func TestUnclassifiableDenies(t *testing.T) {
	for _, sql := range []string{
		"",
		"~~~",
		"SELECT * FROM T; DROP TABLE T; ~~~",
	} {
		if _, _, _, ok := Script(sql); ok {
			t.Errorf("accepted %q", sql)
		}
	}
}

func TestOpKeyCoverageSample(t *testing.T) {
	// Every KnownOpKey that we *do* map must produce a known tier or a
	// documented deny. This is a smoke, not a full cartesian product.
	seen := 0
	for _, k := range fbparse.KnownOpKeys() {
		n := v3OpFor(k)
		if n == 0 {
			continue
		}
		seen++
		if _, ok := v3Op(n); !ok && n != 0 {
			t.Errorf("OpKey %v maps to missing v3 row %d", k, n)
		}
	}
	if seen < 20 {
		t.Fatalf("too few mapped keys: %d", seen)
	}
}

func TestCompensationNeverSaysSafe(t *testing.T) {
	for _, c := range canonical {
		if c.tier == 0 {
			continue
		}
		st, err := fbparse.ParseOne(c.sql)
		if err != nil {
			continue
		}
		txt := Compensation(st)
		if txt == "" {
			t.Errorf("%s: empty compensation", c.note)
		}
		for _, w := range []string{"safe", "Safe", "SAFE"} {
			if contains(txt, w) {
				t.Errorf("%s: compensation contains %q", c.note, w)
			}
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(fmt.Sprint(s)) > 0 && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
