package main

import (
	"context"
	"strings"
	"testing"

	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/qlog"
)

func TestClassifyRead(t *testing.T) {
	cases := []struct {
		sql  string
		want readKind
	}{
		{"SELECT * FROM EMPLOYEE", readSelect},
		{"select full_name from employee where salary > 100 order by 1", readSelect},
		{"WITH C AS (SELECT 1 AS X FROM RDB$DATABASE) SELECT X FROM C", readSelect},
		{"SELECT * FROM ORG_CHART('Marketing')", readSelect}, // selectable procedure via SELECT
		{"EXECUTE PROCEDURE DEPT_BUDGET(100)", readProc},
		{"execute procedure DEPT_BUDGET(100)", readProc},
		// refusals — all fail-closed
		{"", readNone},
		{"   ", readNone},
		{"INSERT INTO T VALUES (1)", readNone},
		{"UPDATE T SET A = 1", readNone},
		{"DELETE FROM T", readNone},
		{"CREATE TABLE T (A INT)", readNone},
		{"DROP TABLE T", readNone},
		{"SET TRANSACTION NO WAIT", readNone},
		{"EXECUTE BLOCK RETURNS (N INT) AS BEGIN N = 1; END", readNone}, // may read, but can do anything — fb_write only
		{"SELECT * FROM T FOR UPDATE WITH LOCK", readNone},
		{"SELECT 1 FROM RDB$DATABASE; SELECT 2 FROM RDB$DATABASE", readNone}, // script, not one statement
		{"EXECUTE PROCEDURE P(1); EXECUTE PROCEDURE P(2)", readNone},
		{"SELECT 1 FROM RDB$DATABASE; DELETE FROM T", readNone},
		{"~~~ nonsense ~~~", readNone},
	}
	for _, c := range cases {
		got, why := classifyRead(c.sql)
		if got != c.want {
			t.Errorf("classifyRead(%q) = %d, want %d (why=%s)", c.sql, got, c.want, why)
		}
	}
}

func TestClassifyReadRefusalNamesFbWrite(t *testing.T) {
	_, why := classifyRead("INSERT INTO T VALUES (1)")
	if !strings.Contains(why, "fb_write") {
		t.Fatalf("refusal must point at fb_write, got %q", why)
	}
}

func TestClampQueryRows(t *testing.T) {
	for _, c := range []struct{ in, want int }{
		{0, queryDefaultRows}, {-5, queryDefaultRows}, {1, 1}, {100, 100},
		{1000, 1000}, {5000, queryHardRowCap},
	} {
		if got := clampQueryRows(c.in); got != c.want {
			t.Errorf("clampQueryRows(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestStringifyRow(t *testing.T) {
	row := stringifyRow([]any{nil, []byte("abc"), int64(42), 3.5, "txt"})
	want := []string{"<null>", "abc", "42", "3.5", "txt"}
	for i := range want {
		if row[i] != want[i] {
			t.Errorf("col %d: got %q want %q", i, row[i], want[i])
		}
	}
	long := strings.Repeat("x", queryValueMax+100)
	if got := truncateVal(long); len(got) != queryValueMax+len("…[truncated]") {
		t.Fatalf("value not truncated: %d", len(got))
	}
}

// requestWrite is the fb_query fallback: a mutation must produce a pending
// action text (Tier 1 in-band token), never execute; a read is denied and
// redirected to fb_query.
func TestRequestWriteMutationPending(t *testing.T) {
	gt := newTestGT(t)
	id := identity.Local(2, nil)
	msg, denied := gt.requestWrite(context.Background(), id, "spike5", "INSERT INTO T (A) VALUES (1)", false)
	if denied {
		t.Fatalf("mutation should go pending, got denied: %s", msg)
	}
	if !strings.Contains(msg, "In-band token") || !strings.Contains(msg, "fb_write") {
		t.Fatalf("pending text missing token/impact: %s", msg)
	}
}

func TestRequestWriteReadDenied(t *testing.T) {
	gt := newTestGT(t)
	id := identity.Local(2, nil)
	msg, denied := gt.requestWrite(context.Background(), id, "spike5", "SELECT * FROM T", false)
	if !denied {
		t.Fatalf("read must be denied by fb_write, got: %s", msg)
	}
	if !strings.Contains(msg, "fb_query") {
		t.Fatalf("denial must redirect to fb_query: %s", msg)
	}
}

func TestRequestWritePreview(t *testing.T) {
	gt := newTestGT(t)
	id := identity.Local(2, nil)
	msg, denied := gt.requestWrite(context.Background(), id, "spike5", "INSERT INTO T (A) VALUES (1)", true)
	if denied {
		t.Fatalf("preview must not be denied: %s", msg)
	}
	if !strings.Contains(msg, "mode=preview") {
		t.Fatalf("preview marker missing: %s", msg)
	}
}

func TestFormatQueryResult(t *testing.T) {
	res := queryResult{
		cols:      []string{"A", "B"},
		rows:      [][]string{{"1", "x"}, {"2", "y"}},
		truncated: true,
		elapsedMS: 1.5,
		plan:      "PLAN (T NATURAL)",
		perTable:  []qlog.PerTable{{Table: "T", SeqReads: 5, IdxReads: 2}},
	}
	out := formatQueryResult(res)
	for _, want := range []string{"A | B", "1 | x", "rows: 2 (truncated", "plan:", "PLAN (T NATURAL)", "per-table:", "elapsed:"} {
		if !strings.Contains(out, want) {
			t.Errorf("formatted output missing %q:\n%s", want, out)
		}
	}
}
