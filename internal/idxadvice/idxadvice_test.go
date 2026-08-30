package idxadvice

import (
	"strings"
	"testing"
)

func mustPlan(t *testing.T, s string) *Node {
	t.Helper()
	n, err := ParsePlan(s)
	if err != nil {
		t.Fatalf("ParsePlan(%q): %v", s, err)
	}
	return n
}

// Golden plan shapes (isql SET PLANONLY output, whitespace normalized).
func TestParsePlanShapes(t *testing.T) {
	cases := []struct {
		plan  string
		scans []string // natural-scan tables
		sorts int
		idxOf string // table → first index used ("" none)
	}{
		{"PLAN (CUSTOMER NATURAL)", []string{"CUSTOMER"}, 0, ""},
		{"PLAN (CUSTOMER INDEX (IDX_CUST_COUNTRY))", nil, 0, "CUSTOMER"},
		{"PLAN SORT ((CUSTOMER NATURAL))", []string{"CUSTOMER"}, 1, ""},
		{"PLAN JOIN (CUSTOMER NATURAL, ORDERS INDEX (FK_ORD_CUST))", []string{"CUSTOMER"}, 0, "ORDERS"},
		{"PLAN JOIN (SORT (CUSTOMER NATURAL), ORDERS NATURAL)", []string{"CUSTOMER", "ORDERS"}, 1, ""},
		{"PLAN (CUSTOMER ORDER IDX_CUST_NAME)", nil, 0, "CUSTOMER"},
		{"PLAN (CUSTOMER ORDER IDX_CUST_NAME INDEX (IDX_CUST_NAME2))", nil, 0, "CUSTOMER"},
		// multi-line isql output
		{"PLAN JOIN (CUSTOMER NATURAL,\n  ORDERS INDEX (FK_ORD_CUST))", []string{"CUSTOMER"}, 0, "ORDERS"},
	}
	for _, c := range cases {
		n := mustPlan(t, c.plan)
		var scans []string
		collectScans(n, &scans)
		if len(scans) != len(c.scans) {
			t.Errorf("%q: scans = %v, want %v", c.plan, scans, c.scans)
			continue
		}
		for i := range scans {
			if scans[i] != c.scans[i] {
				t.Errorf("%q: scans = %v, want %v", c.plan, scans, c.scans)
			}
		}
		var sorts []string
		collectSorts(n, &sorts)
		if len(sorts) != c.sorts {
			t.Errorf("%q: sorts = %v, want %d", c.plan, sorts, c.sorts)
		}
		if c.idxOf != "" {
			found := false
			var visit func(*Node)
			visit = func(n *Node) {
				if n == nil {
					return
				}
				if n.Kind == KindScan && n.Table == c.idxOf && len(n.Indexes) > 0 {
					found = true
				}
				for _, k := range n.Children {
					visit(k)
				}
			}
			visit(n)
			if !found {
				t.Errorf("%q: %s should have an index scan", c.plan, c.idxOf)
			}
		}
	}
}

func TestParsePlanRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "NATURAL", "PLAN (", "PLAN (T INDEX )", "PLAN SORT"} {
		if _, err := ParsePlan(bad); err == nil {
			t.Errorf("ParsePlan(%q) should fail", bad)
		}
	}
}

var noRows RowsFn = func(string) (int64, bool) { return 0, false }

// The motivating case: equality predicate on a naturally scanned table.
func TestAdviceEquality(t *testing.T) {
	q := "SELECT * FROM CUSTOMER WHERE COUNTRY = 'DE'"
	plan := mustPlan(t, "PLAN (CUSTOMER NATURAL)")
	res := Analyze(q, plan, nil, func(string) (int64, bool) { return 5000, true })
	if len(res.Advice) != 1 {
		t.Fatalf("advice = %d, want 1 (notes: %v)", len(res.Advice), res.Notes)
	}
	a := res.Advice[0]
	if a.Table != "CUSTOMER" || len(a.Columns) != 1 || a.Columns[0] != "COUNTRY" {
		t.Errorf("advice table/cols = %s %v", a.Table, a.Columns)
	}
	if a.Kind != "equality" {
		t.Errorf("kind = %s, want equality", a.Kind)
	}
	want := "CREATE INDEX IDX_ADVICE_CUSTOMER_COUNTRY ON CUSTOMER (COUNTRY);"
	if a.DDL != want {
		t.Errorf("ddl = %q, want %q", a.DDL, want)
	}
	if !strings.Contains(a.Estimate, "estimate only") {
		t.Errorf("estimate must be labeled as estimate: %q", a.Estimate)
	}
}

// Aliased table + join predicate on the scanned side → composite candidate.
func TestAdviceJoinComposite(t *testing.T) {
	q := "SELECT * FROM ORDERS O JOIN CUSTOMER C ON O.CUST_ID = C.ID WHERE O.STATUS = 'OPEN'"
	plan := mustPlan(t, "PLAN JOIN (ORDERS NATURAL, CUSTOMER INDEX (PK_CUSTOMER))")
	res := Analyze(q, plan, nil, noRows)
	if len(res.Advice) != 1 {
		t.Fatalf("advice = %d, want 1 (notes: %v)", len(res.Advice), res.Notes)
	}
	a := res.Advice[0]
	if a.Table != "ORDERS" {
		t.Fatalf("table = %s, want ORDERS", a.Table)
	}
	cols := map[string]bool{a.Columns[0]: true, a.Columns[1]: true}
	if !cols["CUST_ID"] || !cols["STATUS"] {
		t.Errorf("columns = %v, want CUST_ID+STATUS", a.Columns)
	}
	if !strings.Contains(a.DDL, "ON ORDERS (") {
		t.Errorf("ddl = %q", a.DDL)
	}
}

// BETWEEN range predicate survives the AND split.
func TestAdviceBetween(t *testing.T) {
	q := "SELECT * FROM SALES WHERE AMOUNT BETWEEN 10 AND 20"
	plan := mustPlan(t, "PLAN (SALES NATURAL)")
	res := Analyze(q, plan, nil, noRows)
	if len(res.Advice) != 1 || res.Advice[0].Kind != "range" {
		t.Fatalf("advice = %+v (notes %v)", res.Advice, res.Notes)
	}
	if res.Advice[0].Columns[0] != "AMOUNT" {
		t.Errorf("columns = %v", res.Advice[0].Columns)
	}
}

// Sort over the scan gets the cautious sort note.
func TestSortNote(t *testing.T) {
	q := "SELECT * FROM CUSTOMER WHERE COUNTRY = 'DE' ORDER BY NAME"
	plan := mustPlan(t, "PLAN SORT ((CUSTOMER NATURAL))")
	res := Analyze(q, plan, nil, noRows)
	if len(res.Advice) != 1 || res.Advice[0].SortNote == "" {
		t.Fatalf("sort note missing: %+v", res.Advice)
	}
}

// Existing index with the same leading columns suppresses advice.
func TestCoveringIndexSuppresses(t *testing.T) {
	q := "SELECT * FROM CUSTOMER WHERE COUNTRY = 'DE'"
	plan := mustPlan(t, "PLAN (CUSTOMER NATURAL)")
	existing := []IndexDef{{Table: "CUSTOMER", Name: "IDX_C", Columns: []string{"COUNTRY", "NAME"}}}
	res := Analyze(q, plan, existing, noRows)
	if len(res.Advice) != 0 {
		t.Fatalf("advice = %d, want 0", len(res.Advice))
	}
	if len(res.Notes) == 0 || !strings.Contains(res.Notes[0], "IDX_C") {
		t.Errorf("notes = %v, want covering-index note", res.Notes)
	}
}

// Identifier generation: 31-byte cap and collision suffix.
func TestIndexName(t *testing.T) {
	n := indexName("A_VERY_LONG_TABLE_NAME", []string{"COL_ONE", "COL_TWO"}, nil)
	if len(n) > 31 {
		t.Errorf("name %q exceeds 31 bytes", n)
	}
	n2 := indexName("T", []string{"C"}, []IndexDef{{Table: "T", Name: "IDX_ADVICE_T_C"}})
	if n2 == "IDX_ADVICE_T_C" {
		t.Errorf("collision not de-duplicated: %q", n2)
	}
}

// Adversarial: every one of these must produce NO advice, and a reason.
func TestAdversarialNoAdvice(t *testing.T) {
	cases := []struct {
		name, query, plan string
		wantNote          string
	}{
		{"subquery", "SELECT * FROM A WHERE X = (SELECT MAX(Y) FROM B)", "PLAN (A NATURAL)", "subquery"},
		{"or", "SELECT * FROM A WHERE X = 1 OR Y = 2", "PLAN (A NATURAL)", "OR"},
		{"nonsargable function", "SELECT * FROM A WHERE UPPER(NAME) = 'AB'", "PLAN (A NATURAL)", "no sargable predicate"},
		{"nonsargable not-equal", "SELECT * FROM A WHERE X != 5", "PLAN (A NATURAL)", "no sargable predicate"},
		{"mid-like", "SELECT * FROM A WHERE NAME LIKE '%AB'", "PLAN (A NATURAL)", "no sargable predicate"},
	}
	for _, c := range cases {
		res := Analyze(c.query, mustPlan(t, c.plan), nil, noRows)
		if len(res.Advice) != 0 {
			t.Errorf("%s: got advice %+v, want none", c.name, res.Advice)
		}
		found := false
		for _, n := range res.Notes {
			if strings.Contains(n, c.wantNote) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: notes %v lack %q", c.name, res.Notes, c.wantNote)
		}
	}
	// Ambiguous unqualified column: the qualified join predicate may still be
	// advised (that is correct), but the ambiguous NAME must appear nowhere.
	res := Analyze("SELECT * FROM A JOIN B ON A.ID = B.ID WHERE NAME = 'x'",
		mustPlan(t, "PLAN JOIN (A NATURAL, B NATURAL)"), nil, noRows)
	for _, a := range res.Advice {
		for _, c := range a.Columns {
			if c == "NAME" {
				t.Errorf("ambiguous column NAME leaked into advice %+v", a)
			}
		}
	}
}

// No natural scans at all → nothing to advise, said so.
func TestNoScansNoAdvice(t *testing.T) {
	res := Analyze("SELECT * FROM A WHERE X = 1", mustPlan(t, "PLAN (A INDEX (IDX_A_X))"), nil, noRows)
	if len(res.Advice) != 0 {
		t.Fatalf("advice = %d, want 0", len(res.Advice))
	}
	if len(res.Notes) == 0 {
		t.Error("expected a no-scans note")
	}
}

// IN (literal list) and prefix LIKE are equality-class.
func TestInAndPrefixLike(t *testing.T) {
	q := "SELECT * FROM A WHERE A.ST IN ('x','y') AND A.NM LIKE 'ab%'"
	plan := mustPlan(t, "PLAN (A NATURAL)")
	res := Analyze(q, plan, nil, noRows)
	if len(res.Advice) != 1 {
		t.Fatalf("advice = %d, want 1 (notes %v)", len(res.Advice), res.Notes)
	}
	cols := map[string]bool{}
	for _, c := range res.Advice[0].Columns {
		cols[c] = true
	}
	if !cols["ST"] || !cols["NM"] {
		t.Errorf("columns = %v, want ST+NM", res.Advice[0].Columns)
	}
	if res.Advice[0].Kind != "equality" {
		t.Errorf("kind = %s", res.Advice[0].Kind)
	}
}

func TestSplitAndBetween(t *testing.T) {
	parts := splitAnd("X BETWEEN 1 AND 2 AND Y = 3")
	if len(parts) != 2 {
		t.Fatalf("parts = %v, want 2", parts)
	}
	if strings.ToUpper(parts[0]) != "X BETWEEN 1 2" && !strings.HasPrefix(strings.ToUpper(parts[0]), "X BETWEEN") {
		t.Errorf("first part = %q", parts[0])
	}
}

func TestEstimateUnknownRows(t *testing.T) {
	if got := estimate("equality", 0, false); !strings.Contains(got, "unknown") {
		t.Errorf("estimate = %q", got)
	}
}
