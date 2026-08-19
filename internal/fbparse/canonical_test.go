package fbparse

import (
	"strings"
	"testing"
)

// canonCase is one canonical classification-matrix fixture (§7): every
// v3-relevant verb×object/variant combination must classify exactly.
type canonCase struct {
	in       string
	verb     Verb
	ot       ObjectType
	name     string
	variant  string
	mutating bool
	rev      Reversibility
	conf     Confidence
	// extra assertions
	column    string
	secondary []string
	grantee   string
	privs     []string
	flags     func(*testing.T, Statement)
	minVer    string
	where     string
}

func canonRun(t *testing.T, cases []canonCase) {
	t.Helper()
	for _, c := range cases {
		stmts := Parse(c.in + ";")
		if len(stmts) != 1 {
			t.Errorf("%q: got %d statements", c.in, len(stmts))
			continue
		}
		s := stmts[0]
		if s.Verb != c.verb {
			t.Errorf("%q: verb=%s want %s (issues %v)", c.in, s.Verb, c.verb, s.Issues)
		}
		if s.ObjectType != c.ot {
			t.Errorf("%q: objtype=%q want %q", c.in, s.ObjectType, c.ot)
		}
		if s.Object.Name != c.name {
			t.Errorf("%q: name=%q want %q", c.in, s.Object.Name, c.name)
		}
		if s.variant != c.variant {
			t.Errorf("%q: variant=%q want %q", c.in, s.variant, c.variant)
		}
		if s.Mutating != c.mutating {
			t.Errorf("%q: mutating=%v want %v", c.in, s.Mutating, c.mutating)
		}
		if s.Reversibility != c.rev {
			t.Errorf("%q: rev=%d want %d", c.in, s.Reversibility, c.rev)
		}
		if s.Confidence != c.conf {
			t.Errorf("%q: conf=%d want %d (issues %v)", c.in, s.Confidence, c.conf, s.Issues)
		}
		if c.column != "" && (s.Column == nil || s.Column.Name != c.column) {
			t.Errorf("%q: column=%v want %q", c.in, s.Column, c.column)
		}
		if c.secondary != nil {
			var got []string
			for _, r := range s.Secondary {
				got = append(got, r.Name)
			}
			if strings.Join(got, ",") != strings.Join(c.secondary, ",") {
				t.Errorf("%q: secondary=%v want %v", c.in, got, c.secondary)
			}
		}
		if c.grantee != "" && s.Grantee.Name != c.grantee {
			t.Errorf("%q: grantee=%q want %q", c.in, s.Grantee.Name, c.grantee)
		}
		if c.privs != nil && strings.Join(s.Privileges, ",") != strings.Join(c.privs, ",") {
			t.Errorf("%q: privs=%v want %v", c.in, s.Privileges, c.privs)
		}
		if c.minVer != "" && s.MinVersion != c.minVer {
			t.Errorf("%q: minVer=%q want %q", c.in, s.MinVersion, c.minVer)
		}
		if c.where != "" && s.Where != c.where {
			t.Errorf("%q: where=%q want %q", c.in, s.Where, c.where)
		}
		if c.flags != nil {
			c.flags(t, s)
		}
		// The canonical corpus is the CI drift-gate corpus: every OpKey
		// emitted must be a known key.
		if !knownOpKey(s.OpKey()) {
			t.Errorf("%q: OpKey %+v not in KnownOpKeys", c.in, s.OpKey())
		}
	}
}

func knownOpKey(k OpKey) bool {
	for _, kk := range KnownOpKeys() {
		if kk == k {
			return true
		}
	}
	return false
}

func TestCanonicalReads(t *testing.T) {
	canonRun(t, []canonCase{
		{in: "SELECT * FROM T", verb: VerbSelect, conf: ConfidenceHigh},
		{in: "SELECT FIRST 10 SKIP 5 A, B FROM T WHERE A > 1 ORDER BY B", verb: VerbSelect, conf: ConfidenceHigh},
		{in: "WITH C AS (SELECT 1 AS X FROM R) SELECT * FROM C", verb: VerbSelect, conf: ConfidenceHigh},
		{in: "WITH RECURSIVE R(N) AS (SELECT 1 FROM RDB$DATABASE UNION ALL SELECT N+1 FROM R WHERE N < 5) SELECT * FROM R", verb: VerbSelect, conf: ConfidenceHigh},
		{
			in: "SELECT * FROM T WHERE ID = 1 WITH LOCK", verb: VerbSelect, variant: "WITH_LOCK", conf: ConfidenceHigh,
			flags: func(t *testing.T, s Statement) {
				if !s.Flags.WithLock {
					t.Errorf("WithLock not set")
				}
			},
		},
	})
}

func TestCanonicalDML(t *testing.T) {
	canonRun(t, []canonCase{
		{in: "INSERT INTO T (A, B) VALUES (1, 'x')", verb: VerbInsert, ot: ObjTable, name: "T", mutating: true, rev: ReversibilityRestorePoint, conf: ConfidenceHigh},
		{
			in: "INSERT INTO T SELECT * FROM S", verb: VerbInsert, ot: ObjTable, name: "T", mutating: true, rev: ReversibilityRestorePoint,
			conf: ConfidenceHigh, secondary: []string{"S"},
		},
		{
			in: "UPDATE T SET A = 1, B = B + 1 WHERE ID = 5", verb: VerbUpdate, ot: ObjTable, name: "T", mutating: true,
			rev: ReversibilityRestorePoint, conf: ConfidenceHigh,
			where: "WHERE ID = 5",
		},
		{
			in: "DELETE FROM T WHERE ID = 5 AND NAME LIKE 'a%'", verb: VerbDelete, ot: ObjTable, name: "T", mutating: true,
			rev: ReversibilityRestorePoint, conf: ConfidenceHigh, where: "WHERE ID = 5 AND NAME LIKE 'a%'",
		},
		{in: "DELETE FROM T", verb: VerbDelete, ot: ObjTable, name: "T", mutating: true, rev: ReversibilityRestorePoint, conf: ConfidenceHigh},
		{
			in: "UPDATE OR INSERT INTO T (A) VALUES (1) MATCHING (A)", verb: VerbUpdate, ot: ObjTable, name: "T",
			variant: "OR_INSERT", mutating: true, rev: ReversibilityRestorePoint, conf: ConfidenceHigh,
			flags: func(t *testing.T, s Statement) {
				if !s.Flags.Upsert {
					t.Errorf("Upsert not set")
				}
			},
		},
		{
			in: "MERGE INTO T USING S ON T.ID = S.ID WHEN MATCHED THEN UPDATE SET T.A = S.A", verb: VerbMerge,
			ot: ObjTable, name: "T", mutating: true, rev: ReversibilityRestorePoint, conf: ConfidenceHigh,
			secondary: []string{"S"},
		},
		{
			in:   "MERGE INTO T USING S ON T.ID = S.ID WHEN NOT MATCHED THEN INSERT (ID) VALUES (S.ID)",
			verb: VerbMerge, ot: ObjTable, name: "T", mutating: true, rev: ReversibilityRestorePoint, conf: ConfidenceHigh,
			secondary: []string{"S"},
		},
	})
}

func TestCanonicalCreate(t *testing.T) {
	canonRun(t, []canonCase{
		{in: "CREATE TABLE T (A INT, B VARCHAR(10))", verb: VerbCreate, ot: ObjTable, name: "T", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "CREATE GLOBAL TEMPORARY TABLE T (A INT) ON COMMIT PRESERVE ROWS", verb: VerbCreate, ot: ObjGlobalTempTable, name: "T", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "CREATE EXTERNAL TABLE T FILE 't.dat' (A CHAR(10))", verb: VerbCreate, ot: ObjExternalTable, name: "T", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "CREATE VIEW V (A, B) AS SELECT A, B FROM T WITH CHECK OPTION", verb: VerbCreate, ot: ObjView, name: "V", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "CREATE INDEX I ON T (A)", verb: VerbCreate, ot: ObjIndex, name: "I", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh, secondary: []string{"T"}},
		{in: "CREATE UNIQUE INDEX I ON T (A)", verb: VerbCreate, ot: ObjIndex, name: "I", variant: "INDEX_UNIQUE", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh, secondary: []string{"T"}},
		{in: "CREATE DESCENDING INDEX I ON T (A)", verb: VerbCreate, ot: ObjIndex, name: "I", variant: "INDEX_DESC", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh, secondary: []string{"T"}},
		{in: "CREATE UNIQUE DESC INDEX I ON T (A DESC)", verb: VerbCreate, ot: ObjIndex, name: "I", variant: "INDEX_UNIQUE_DESC", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh, secondary: []string{"T"}},
		{
			in: "CREATE INDEX I ON T (UPPER(NAME))", verb: VerbCreate, ot: ObjIndex, name: "I", variant: "INDEX_EXPRESSION",
			mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh, secondary: []string{"T"},
			flags: func(t *testing.T, s Statement) {
				if !s.Flags.IndexExpression {
					t.Errorf("IndexExpression not set")
				}
			},
		},
		{in: "CREATE INDEX I ON T COMPUTED BY (A + B)", verb: VerbCreate, ot: ObjIndex, name: "I", variant: "INDEX_EXPRESSION", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh, secondary: []string{"T"}},
		{in: "CREATE SEQUENCE S START WITH 10 INCREMENT BY 2", verb: VerbCreate, ot: ObjSequence, name: "S", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "CREATE GENERATOR G", verb: VerbCreate, ot: ObjSequence, name: "G", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "CREATE DOMAIN D AS VARCHAR(20) NOT NULL", verb: VerbCreate, ot: ObjDomain, name: "D", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "CREATE EXCEPTION E 'failed'", verb: VerbCreate, ot: ObjException, name: "E", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "CREATE TABLE T (B BOOLEAN)", verb: VerbCreate, ot: ObjTable, name: "T", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceMedium, minVer: "3.0"},
		{in: "CREATE USER U PASSWORD 'pw'", verb: VerbCreate, ot: ObjUser, name: "U", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "CREATE ROLE R", verb: VerbCreate, ot: ObjRole, name: "R", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "CREATE SHADOW 1 's1.dat'", verb: VerbCreate, ot: ObjShadow, name: "1", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "CREATE DATABASE 'srv:x.fdb' USER 'u' PASSWORD 'p' PAGE_SIZE 8192", verb: VerbCreate, ot: ObjDatabase, name: "srv:x.fdb", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "CREATE MAPPING M USING PLUGIN P FROM ANY TO USER U", verb: VerbCreate, ot: ObjMapping, name: "M", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{
			in:   "CREATE TRIGGER TR FOR EMP BEFORE INSERT POSITION 0 AS BEGIN NEW.A = 1; END",
			verb: VerbCreate, ot: ObjTrigger, name: "TR", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh,
			secondary: []string{"EMP"},
			flags: func(t *testing.T, s Statement) {
				if s.Flags.TriggerKind != "DML" {
					t.Errorf("TriggerKind=%q", s.Flags.TriggerKind)
				}
			},
		},
		{
			in:   "CREATE TRIGGER TR ACTIVE ON CONNECT AS BEGIN END",
			verb: VerbCreate, ot: ObjTrigger, name: "TR", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh,
			flags: func(t *testing.T, s Statement) {
				if s.Flags.TriggerKind != "DATABASE_EVENT" {
					t.Errorf("TriggerKind=%q", s.Flags.TriggerKind)
				}
			},
		},
		{
			in:   "CREATE TRIGGER TR BEFORE ANY DDL STATEMENT AS BEGIN END",
			verb: VerbCreate, ot: ObjTrigger, name: "TR", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh,
			flags: func(t *testing.T, s Statement) {
				if s.Flags.TriggerKind != "DDL" {
					t.Errorf("TriggerKind=%q", s.Flags.TriggerKind)
				}
			},
		},
		{
			in:   "CREATE TRIGGER TR AFTER INSERT OR UPDATE ON EMP AS BEGIN END",
			verb: VerbCreate, ot: ObjTrigger, name: "TR", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh,
			secondary: []string{"EMP"},
		},
		{
			in:   "CREATE PROCEDURE P (A INT) RETURNS (B INT) AS BEGIN B = A; SUSPEND; END",
			verb: VerbCreate, ot: ObjProcedure, name: "P", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh,
		},
		{
			in:   "CREATE PROCEDURE P AS BEGIN INSERT INTO LOG VALUES (1); END",
			verb: VerbCreate, ot: ObjProcedure, name: "P", mutating: true, rev: ReversibilityReverseDDL,
			// FR-9: body DML targets are best-effort and flagged → Low.
			conf: ConfidenceLow,
			flags: func(t *testing.T, s Statement) {
				if s.Body == nil || !s.Body.HasDML {
					t.Fatalf("body DML not detected")
				}
				if s.Body.Bytes <= 0 {
					t.Errorf("body bytes=%d", s.Body.Bytes)
				}
			},
		},
		{in: "CREATE FUNCTION F (A INT) RETURNS INT AS BEGIN RETURN A; END", verb: VerbCreate, ot: ObjFunction, name: "F", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "CREATE FUNCTION F (A INT) RETURNS INT EXTERNAL NAME 'x' ENGINE Y", verb: VerbCreate, ot: ObjFunction, name: "F", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "CREATE OR ALTER PROCEDURE P AS BEGIN END", verb: VerbCreateOrAlter, ot: ObjProcedure, name: "P", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "CREATE OR ALTER TRIGGER TR ACTIVE AS BEGIN END", verb: VerbCreateOrAlter, ot: ObjTrigger, name: "TR", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "CREATE OR ALTER VIEW V AS SELECT * FROM T", verb: VerbCreateOrAlter, ot: ObjView, name: "V", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "RECREATE TABLE T (A INT)", verb: VerbRecreate, ot: ObjTable, name: "T", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "RECREATE PROCEDURE P AS BEGIN END", verb: VerbRecreate, ot: ObjProcedure, name: "P", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{
			in: "CREATE INDEX I ON T (A) WHERE A > 0", verb: VerbCreate, ot: ObjIndex, name: "I", variant: "INDEX_PARTIAL",
			mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceMedium, minVer: "5.0", secondary: []string{"T"},
		},
	})
}

func TestCanonicalPackage(t *testing.T) {
	canonRun(t, []canonCase{
		{
			in:   "CREATE PACKAGE PKG AS BEGIN FUNCTION F(A INT) RETURNS INT; PROCEDURE P(A INT); END",
			verb: VerbCreate, ot: ObjPackage, name: "PKG", mutating: true, rev: ReversibilityReverseDDL,
			conf: ConfidenceMedium, minVer: "3.0",
		},
		{
			in:   "CREATE PACKAGE BODY PKG AS BEGIN FUNCTION F(A INT) RETURNS INT AS BEGIN RETURN A; END PROCEDURE P(A INT) AS BEGIN END END",
			verb: VerbCreate, ot: ObjPackage, name: "PKG", variant: "BODY", mutating: true, rev: ReversibilityReverseDDL,
			conf: ConfidenceMedium, minVer: "3.0",
		},
		{in: "DROP PACKAGE PKG", verb: VerbDrop, ot: ObjPackage, name: "PKG", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceMedium, minVer: "3.0"},
		{in: "DROP PACKAGE BODY PKG", verb: VerbDrop, ot: ObjPackage, name: "PKG", variant: "BODY", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceMedium, minVer: "3.0"},
	})
}

func TestCanonicalDrop(t *testing.T) {
	canonRun(t, []canonCase{
		{in: "DROP TABLE T", verb: VerbDrop, ot: ObjTable, name: "T", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "DROP VIEW V", verb: VerbDrop, ot: ObjView, name: "V", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "DROP INDEX I", verb: VerbDrop, ot: ObjIndex, name: "I", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "DROP PROCEDURE P", verb: VerbDrop, ot: ObjProcedure, name: "P", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "DROP FUNCTION F", verb: VerbDrop, ot: ObjFunction, name: "F", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "DROP EXTERNAL FUNCTION F", verb: VerbDrop, ot: ObjFunction, name: "F", variant: "EXTERNAL", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "DROP TRIGGER TR", verb: VerbDrop, ot: ObjTrigger, name: "TR", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "DROP DOMAIN D", verb: VerbDrop, ot: ObjDomain, name: "D", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "DROP EXCEPTION E", verb: VerbDrop, ot: ObjException, name: "E", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "DROP SEQUENCE S", verb: VerbDrop, ot: ObjSequence, name: "S", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "DROP GENERATOR G", verb: VerbDrop, ot: ObjSequence, name: "G", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "DROP ROLE R", verb: VerbDrop, ot: ObjRole, name: "R", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "DROP USER U", verb: VerbDrop, ot: ObjUser, name: "U", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "DROP FILTER F", verb: VerbDrop, ot: ObjFilter, name: "F", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "DROP SHADOW 3", verb: VerbDrop, ot: ObjShadow, name: "3", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "DROP MAPPING M", verb: VerbDrop, ot: ObjMapping, name: "M", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "DROP TABLE IF EXISTS T", verb: VerbDrop, ot: ObjTable, name: "T", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "DROP DATABASE", verb: VerbDrop, ot: ObjDatabase, mutating: true, rev: ReversibilityRestorePoint, conf: ConfidenceHigh},
	})
}

func TestCanonicalAlter(t *testing.T) {
	canonRun(t, []canonCase{
		{in: "ALTER TABLE T ADD C INT", verb: VerbAlter, ot: ObjTable, name: "T", variant: "COLUMN_ADD", column: "C", mutating: true, rev: ReversibilityRestorePoint, conf: ConfidenceHigh},
		{in: "ALTER TABLE T ADD COLUMN C INT DEFAULT 0", verb: VerbAlter, ot: ObjTable, name: "T", variant: "COLUMN_ADD", column: "C", mutating: true, rev: ReversibilityRestorePoint, conf: ConfidenceHigh},
		{in: "ALTER TABLE T DROP C", verb: VerbAlter, ot: ObjTable, name: "T", variant: "COLUMN_DROP", column: "C", mutating: true, rev: ReversibilityRestorePoint, conf: ConfidenceHigh},
		{in: "ALTER TABLE T DROP COLUMN IF EXISTS C CASCADE", verb: VerbAlter, ot: ObjTable, name: "T", variant: "COLUMN_DROP", column: "C", mutating: true, rev: ReversibilityRestorePoint, conf: ConfidenceHigh},
		{in: "ALTER TABLE T ALTER COLUMN C TYPE VARCHAR(50)", verb: VerbAlter, ot: ObjTable, name: "T", variant: "COLUMN_TYPE", column: "C", mutating: true, rev: ReversibilityRestorePoint, conf: ConfidenceHigh},
		{in: "ALTER TABLE T ALTER C TYPE BOOLEAN", verb: VerbAlter, ot: ObjTable, name: "T", variant: "COLUMN_TYPE", column: "C", mutating: true, rev: ReversibilityRestorePoint, conf: ConfidenceMedium, minVer: "3.0"},
		{in: "ALTER TABLE T ALTER C SET NOT NULL", verb: VerbAlter, ot: ObjTable, name: "T", variant: "COLUMN_NOT_NULL", column: "C", mutating: true, rev: ReversibilityRestorePoint, conf: ConfidenceHigh},
		{in: "ALTER TABLE T ALTER C DROP NOT NULL", verb: VerbAlter, ot: ObjTable, name: "T", variant: "COLUMN_NOT_NULL", column: "C", mutating: true, rev: ReversibilityRestorePoint, conf: ConfidenceHigh},
		{in: "ALTER TABLE T ALTER C SET DEFAULT 0", verb: VerbAlter, ot: ObjTable, name: "T", variant: "COLUMN_DEFAULT", column: "C", mutating: true, rev: ReversibilityRestorePoint, conf: ConfidenceHigh},
		{in: "ALTER TABLE T ALTER C DROP DEFAULT", verb: VerbAlter, ot: ObjTable, name: "T", variant: "COLUMN_DEFAULT", column: "C", mutating: true, rev: ReversibilityRestorePoint, conf: ConfidenceHigh},
		{in: "ALTER TABLE T ALTER C TO D", verb: VerbAlter, ot: ObjTable, name: "T", variant: "COLUMN_RENAME", column: "C", mutating: true, rev: ReversibilityRestorePoint, conf: ConfidenceHigh},
		{in: "ALTER TABLE T ADD CONSTRAINT PK_T PRIMARY KEY (ID)", verb: VerbAlter, ot: ObjTable, name: "T", variant: "CONSTRAINT_PK", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "ALTER TABLE T ADD CONSTRAINT UQ_T UNIQUE (A, B)", verb: VerbAlter, ot: ObjTable, name: "T", variant: "CONSTRAINT_UNIQUE", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{
			in: "ALTER TABLE T ADD CONSTRAINT FK_T FOREIGN KEY (X) REFERENCES O (Y)", verb: VerbAlter, ot: ObjTable,
			name: "T", variant: "CONSTRAINT_FK", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh,
			secondary: []string{"O"},
		},
		{in: "ALTER TABLE T ADD CHECK (A > 0)", verb: VerbAlter, ot: ObjTable, name: "T", variant: "CONSTRAINT_CHECK", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "ALTER TABLE T DROP CONSTRAINT PK_T", verb: VerbAlter, ot: ObjTable, name: "T", variant: "CONSTRAINT_DROP", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "ALTER VIEW V AS SELECT A FROM T", verb: VerbAlter, ot: ObjView, name: "V", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{
			in: "ALTER INDEX I INACTIVE", verb: VerbAlter, ot: ObjIndex, name: "I", variant: "INDEX_INACTIVE", mutating: true,
			rev: ReversibilityReverseDDL, conf: ConfidenceHigh,
			flags: func(t *testing.T, s Statement) {
				if s.Flags.IndexActivation != "INACTIVE" {
					t.Errorf("activation=%q", s.Flags.IndexActivation)
				}
			},
		},
		{
			in: "ALTER INDEX I ACTIVE", verb: VerbAlter, ot: ObjIndex, name: "I", variant: "INDEX_ACTIVE", mutating: true,
			rev: ReversibilityReverseDDL, conf: ConfidenceHigh,
			flags: func(t *testing.T, s Statement) {
				if s.Flags.IndexActivation != "ACTIVE" {
					t.Errorf("activation=%q", s.Flags.IndexActivation)
				}
			},
		},
		{in: "ALTER DATABASE SET LINGER TO 5", verb: VerbAlter, ot: ObjDatabase, mutating: true, rev: ReversibilityRestorePoint, conf: ConfidenceMedium, minVer: "4.0"},
		{in: "ALTER SEQUENCE S RESTART WITH 10", verb: VerbAlter, ot: ObjSequence, name: "S", mutating: true, rev: ReversibilityRestorePoint, conf: ConfidenceHigh},
		{in: "ALTER TRIGGER TR INACTIVE", verb: VerbAlter, ot: ObjTrigger, name: "TR", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "ALTER PROCEDURE P (A INT) AS BEGIN END", verb: VerbAlter, ot: ObjProcedure, name: "P", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "ALTER FUNCTION F (A INT) RETURNS INT AS BEGIN RETURN A; END", verb: VerbAlter, ot: ObjFunction, name: "F", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "ALTER USER U SET PASSWORD 'x'", verb: VerbAlter, ot: ObjUser, name: "U", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "ALTER DOMAIN D SET DEFAULT 0", verb: VerbAlter, ot: ObjDomain, name: "D", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "ALTER DOMAIN D TYPE VARCHAR(30)", verb: VerbAlter, ot: ObjDomain, name: "D", mutating: true, rev: ReversibilityRestorePoint, conf: ConfidenceHigh},
		{in: "ALTER EXCEPTION E 'new msg'", verb: VerbAlter, ot: ObjException, name: "E", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "ALTER ROLE R DROP SYSTEM PRIVILEGES", verb: VerbAlter, ot: ObjRole, name: "R", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
	})
}

func TestCanonicalDCL(t *testing.T) {
	canonRun(t, []canonCase{
		{in: "GRANT SELECT ON T TO U", verb: VerbGrant, ot: ObjTable, name: "T", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh, grantee: "U", privs: []string{"SELECT"}},
		{in: "GRANT SELECT, INSERT ON TABLE T TO U", verb: VerbGrant, ot: ObjTable, name: "T", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh, grantee: "U", privs: []string{"SELECT", "INSERT"}},
		{
			in: "GRANT ALL ON T TO PUBLIC", verb: VerbGrant, ot: ObjTable, name: "T", mutating: true, rev: ReversibilityReverseDDL,
			conf: ConfidenceHigh, grantee: "PUBLIC", privs: []string{"ALL"},
		},
		{
			in: "GRANT UPDATE (A, B), REFERENCES (A) ON T TO U WITH GRANT OPTION", verb: VerbGrant, ot: ObjTable, name: "T",
			variant: "GRANT_WITH_OPTION", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh,
			grantee: "U", privs: []string{"UPDATE", "REFERENCES"},
			flags: func(t *testing.T, s Statement) {
				if !s.Flags.GrantOption {
					t.Errorf("GrantOption not set")
				}
			},
		},
		{in: "GRANT EXECUTE ON PROCEDURE P TO U", verb: VerbGrant, ot: ObjProcedure, name: "P", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh, grantee: "U", privs: []string{"EXECUTE"}},
		{in: "GRANT EXECUTE ON FUNCTION F TO U", verb: VerbGrant, ot: ObjFunction, name: "F", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh, grantee: "U", privs: []string{"EXECUTE"}},
		{
			in: "GRANT USAGE ON GENERATOR G TO U", verb: VerbGrant, ot: ObjSequence, name: "G", mutating: true,
			rev: ReversibilityReverseDDL, conf: ConfidenceMedium, minVer: "3.0", grantee: "U", privs: []string{"USAGE"},
		},
		{
			in: "GRANT USAGE ON EXCEPTION E TO U", verb: VerbGrant, ot: ObjException, name: "E", mutating: true,
			rev: ReversibilityReverseDDL, conf: ConfidenceMedium, minVer: "3.0", grantee: "U", privs: []string{"USAGE"},
		},
		{
			in: "GRANT R TO U WITH ADMIN OPTION", verb: VerbGrant, ot: ObjRole, name: "R", variant: "GRANT_ADMIN_OPTION",
			mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh, grantee: "U",
			flags: func(t *testing.T, s Statement) {
				if !s.Flags.AdminOption {
					t.Errorf("AdminOption not set")
				}
			},
		},
		{
			in: "GRANT CREATE, ALTER ANY ON TABLE TO U", verb: VerbGrant, ot: ObjTable, variant: "DDL",
			mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceMedium, minVer: "3.0", grantee: "U",
			privs: []string{"CREATE", "ALTER ANY"},
		},
		{
			in: "GRANT CREATE ON DATABASE TO U", verb: VerbGrant, ot: ObjDatabase, variant: "DDL",
			mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceMedium, minVer: "3.0", grantee: "U",
			privs: []string{"CREATE"},
		},
		{
			in: "GRANT SELECT ON T TO U1, U2 GRANTED BY V", verb: VerbGrant, ot: ObjTable, name: "T", mutating: true,
			rev: ReversibilityReverseDDL, conf: ConfidenceHigh, grantee: "U1", privs: []string{"SELECT"},
			flags: func(t *testing.T, s Statement) {
				if s.Flags.Extras["grantor"] != "V" || s.Flags.Extras["grantees"] != "U2" {
					t.Errorf("extras=%v", s.Flags.Extras)
				}
			},
		},
		{in: "REVOKE SELECT ON T FROM U", verb: VerbRevoke, ot: ObjTable, name: "T", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh, grantee: "U", privs: []string{"SELECT"}},
		{
			in: "REVOKE GRANT OPTION FOR SELECT ON T FROM U", verb: VerbRevoke, ot: ObjTable, name: "T",
			variant: "REVOKE_GRANT_OPTION", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh,
			grantee: "U", privs: []string{"SELECT"},
			flags: func(t *testing.T, s Statement) {
				if !s.Flags.GrantOption {
					t.Errorf("GrantOption not set")
				}
			},
		},
		{
			in: "REVOKE ADMIN OPTION FOR R FROM U", verb: VerbRevoke, ot: ObjRole, name: "R",
			variant: "REVOKE_ADMIN_OPTION", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh,
			grantee: "U",
			flags: func(t *testing.T, s Statement) {
				if !s.Flags.AdminOption {
					t.Errorf("AdminOption not set")
				}
			},
		},
		{in: "REVOKE ALL ON T FROM U", verb: VerbRevoke, ot: ObjTable, name: "T", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh, grantee: "U", privs: []string{"ALL"}},
	})
}

func TestCanonicalCommentDeclareSet(t *testing.T) {
	canonRun(t, []canonCase{
		{in: "COMMENT ON TABLE T IS 'note'", verb: VerbComment, ot: ObjTable, name: "T", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "COMMENT ON DATABASE IS 'note'", verb: VerbComment, ot: ObjDatabase, mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "COMMENT ON COLUMN T.C IS 'note'", verb: VerbComment, ot: ObjTable, name: "T", variant: "COLUMN", column: "C", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "COMMENT ON PROCEDURE P IS 'note'", verb: VerbComment, ot: ObjProcedure, name: "P", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "COMMENT ON PROCEDURE PARAMETER P.X IS 'note'", verb: VerbComment, ot: ObjProcedure, name: "P", variant: "PARAMETER", column: "X", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "COMMENT ON FUNCTION PARAMETER F.X IS 'note'", verb: VerbComment, ot: ObjFunction, name: "F", variant: "PARAMETER", column: "X", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "COMMENT ON USER U IS 'note'", verb: VerbComment, ot: ObjUser, name: "U", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "COMMENT ON INDEX I IS 'note'", verb: VerbComment, ot: ObjIndex, name: "I", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "DECLARE FILTER F INPUT_TYPE 1 OUTPUT_TYPE 2 ENTRY_POINT 'e' MODULE_NAME 'm'", verb: VerbDeclare, ot: ObjFilter, name: "F", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "DECLARE EXTERNAL FUNCTION F CSTRING(10) RETURNS CSTRING(10) ENTRY_POINT 'e' MODULE_NAME 'm'", verb: VerbDeclare, ot: ObjFunction, name: "F", variant: "EXTERNAL", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "SET GENERATOR G TO 100", verb: VerbSet, ot: ObjSequence, name: "G", variant: "GENERATOR", mutating: true, rev: ReversibilityRestorePoint, conf: ConfidenceHigh},
		{in: "SET STATISTICS INDEX I", verb: VerbSet, ot: ObjIndex, name: "I", variant: "STATISTICS", mutating: true, rev: ReversibilityReverseDDL, conf: ConfidenceHigh},
		{in: "SET TRANSACTION READ WRITE WAIT ISOLATION LEVEL SNAPSHOT", verb: VerbSet, variant: "TRANSACTION", mutating: false, rev: ReversibilityNone, conf: ConfidenceHigh},
		{in: "SET ROLE R", verb: VerbSet, variant: "SESSION", mutating: false, rev: ReversibilityNone, conf: ConfidenceHigh},
		{in: "SET SESSION IDLE TIMEOUT 5 MINUTES", verb: VerbSet, variant: "SESSION", mutating: false, rev: ReversibilityNone, conf: ConfidenceHigh},
	})
}

func TestCanonicalExecute(t *testing.T) {
	canonRun(t, []canonCase{
		{in: "EXECUTE PROCEDURE P (1, 'x')", verb: VerbExecuteProc, ot: ObjProcedure, name: "P", mutating: true, rev: ReversibilityRestorePoint, conf: ConfidenceHigh},
		{
			in:   "EXECUTE BLOCK (X INT = ?) RETURNS (Y INT) AS BEGIN Y = X * 2; SUSPEND; END",
			verb: VerbExecuteBlock, mutating: true, rev: ReversibilityRestorePoint, conf: ConfidenceHigh,
			flags: func(t *testing.T, s Statement) {
				if s.Body == nil || s.Body.HasDML {
					t.Errorf("body=%+v", s.Body)
				}
			},
		},
	})
	stmts := Parse("EXECUTE BLOCK AS BEGIN DELETE FROM T WHERE ID = 1; END;")
	if len(stmts) != 1 {
		t.Fatalf("got %d statements", len(stmts))
	}
	s := stmts[0]
	if s.Verb != VerbExecuteBlock || !s.Mutating {
		t.Fatalf("verb=%s mutating=%v", s.Verb, s.Mutating)
	}
	if s.Body == nil || !s.Body.HasDML {
		t.Fatalf("body DML not detected: %+v", s.Body)
	}
	if len(s.Secondary) != 1 || s.Secondary[0].Name != "T" {
		t.Fatalf("secondary=%v", s.Secondary)
	}
	if s.Confidence != ConfidenceLow {
		t.Fatalf("expected Low for best-effort body extraction, got %d (issues %v)", s.Confidence, s.Issues)
	}
}
