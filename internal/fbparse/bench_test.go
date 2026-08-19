package fbparse

import (
	"strings"
	"testing"
)

// benchScript is a representative mixed workload (reads, writes, DDL,
// DCL, PSQL) — the "average statement" of NFR-5.
var benchScript = strings.Join([]string{
	"SELECT A, B FROM T WHERE A > 10 ORDER BY B;",
	"INSERT INTO T (A, B) VALUES (1, 'x');",
	"UPDATE T SET B = 'y' WHERE A = 1;",
	"DELETE FROM T WHERE A = 2;",
	"MERGE INTO T USING S ON T.A = S.A WHEN MATCHED THEN UPDATE SET T.B = S.B;",
	"CREATE TABLE U (A INT, B VARCHAR(10));",
	"ALTER TABLE U ADD C INT;",
	"DROP TABLE U;",
	"GRANT SELECT ON T TO APP;",
	"SET GENERATOR G TO 5;",
	strings.ReplaceAll("CREATE PROCEDURE P AS BEGIN INSERT INTO L VALUES (1); UPDATE L SET A = 2; END", "\n", " "),
}, "\n")

// BenchmarkParseAverage measures statements/sec on one core; NFR-5
// budget: ≥ 10,000 average statements/sec.
func BenchmarkParseAverage(b *testing.B) {
	b.SetBytes(int64(len(benchScript)))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			Parse(benchScript)
		}
	})
}

func BenchmarkParseSingleStatement(b *testing.B) {
	in := "UPDATE T SET A = 1 WHERE ID = 5 AND NAME LIKE 'x%'"
	for i := 0; i < b.N; i++ {
		ParseOne(in)
	}
}

// Pathological inputs (§7): deep PSQL nesting, long literals, dense
// comments — the lexer must stay linear.
func BenchmarkPathologicalDeepNesting(b *testing.B) {
	in := "CREATE PROCEDURE P AS BEGIN " +
		strings.Repeat("IF (X = 1) THEN BEGIN ", 400) + "X = 2;" +
		strings.Repeat("END ", 400) + " END"
	b.SetBytes(int64(len(in)))
	for i := 0; i < b.N; i++ {
		Parse(in)
	}
}

func BenchmarkPathologicalLongLiteral(b *testing.B) {
	in := "INSERT INTO T VALUES ('" + strings.Repeat("a", 1<<20) + "')"
	b.SetBytes(int64(len(in)))
	for i := 0; i < b.N; i++ {
		Parse(in)
	}
}

func BenchmarkPathologicalComments(b *testing.B) {
	in := strings.Repeat("/* block */ -- line\n", 100000) + "SELECT 1"
	b.SetBytes(int64(len(in)))
	for i := 0; i < b.N; i++ {
		Parse(in)
	}
}

func BenchmarkManyStatements(b *testing.B) {
	in := strings.Repeat("UPDATE T SET A = 1 WHERE ID = 5;", 10000)
	b.SetBytes(int64(len(in)))
	for i := 0; i < b.N; i++ {
		Parse(in)
	}
}

func BenchmarkTemplate(b *testing.B) {
	in := "UPDATE T SET A = 'x', B = 'y' WHERE C = 'z' AND D IN ('1','2','3')"
	stmts := Parse(in)
	for i := 0; i < b.N; i++ {
		_ = stmts[0].Template()
	}
}
