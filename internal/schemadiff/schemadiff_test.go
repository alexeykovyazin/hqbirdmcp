package schemadiff

import (
	"reflect"
	"strings"
	"testing"
)

func table(name string, cols ...Column) *Table {
	return &Table{Name: name, Columns: cols}
}

func col(name, typ string, nullable bool) Column {
	return Column{Name: name, Type: typ, Nullable: nullable}
}

func TestDiffIdentical(t *testing.T) {
	a := &Schema{Tables: map[string]*Table{
		"T1": table("T1", col("ID", "INTEGER", false), col("NM", "VARCHAR(80)", true)),
	}}
	b := &Schema{Tables: map[string]*Table{
		"T1": table("T1", col("ID", "INTEGER", false), col("NM", "VARCHAR(80)", true)),
	}}
	r := Diff(a, b)
	if !r.Identical || !r.Empty() {
		t.Fatalf("expected identical, got %+v", r)
	}
}

func TestDiffMissingTables(t *testing.T) {
	a := &Schema{Tables: map[string]*Table{
		"KEEP": table("KEEP", col("ID", "INTEGER", false)),
		"GONE": table("GONE", col("ID", "INTEGER", false)),
	}}
	b := &Schema{Tables: map[string]*Table{
		"KEEP": table("KEEP", col("ID", "INTEGER", false)),
		"NEW":  table("NEW", col("ID", "INTEGER", false)),
	}}
	r := Diff(a, b)
	if !reflect.DeepEqual(r.OnlyInA, []string{"GONE"}) || !reflect.DeepEqual(r.OnlyInB, []string{"NEW"}) {
		t.Fatalf("onlyInA/onlyInB = %v / %v", r.OnlyInA, r.OnlyInB)
	}
}

func TestDiffColumnChanges(t *testing.T) {
	a := &Schema{Tables: map[string]*Table{
		"T": table("T",
			col("ID", "INTEGER", false),
			col("ADDME", "INTEGER", true),
			col("CHG", "INTEGER", true),
			col("STIFF", "INTEGER", false),
		),
	}}
	b := &Schema{Tables: map[string]*Table{
		"T": table("T",
			col("ID", "INTEGER", false),
			col("CHG", "VARCHAR(20)", false),
			col("DROPME", "INTEGER", true),
			col("STIFF", "INTEGER", false),
		),
	}}
	b.Tables["T"].PK = []string{"ID"}
	r := Diff(a, b)
	if len(r.Tables) != 1 {
		t.Fatalf("tables = %+v", r.Tables)
	}
	td := r.Tables[0]
	if !reflect.DeepEqual(td.ColumnsAdded, []string{"ADDME"}) {
		t.Errorf("added = %v", td.ColumnsAdded)
	}
	if !reflect.DeepEqual(td.ColumnsDropped, []string{"DROPME"}) {
		t.Errorf("dropped = %v", td.ColumnsDropped)
	}
	if len(td.ColumnsChanged) != 1 || td.ColumnsChanged[0] != "CHG: INTEGER (null) → VARCHAR(20) not null" {
		t.Errorf("changed = %v", td.ColumnsChanged)
	}
	if !td.PKChanged {
		t.Error("PK change not detected")
	}
}

func TestRender(t *testing.T) {
	a := &Schema{Tables: map[string]*Table{"T": table("T", col("C", "INTEGER", true))}}
	b := &Schema{Tables: map[string]*Table{}}
	txt := Render(Diff(a, b), "src", "dst")
	if txt == "schemas identical: src == dst" {
		t.Fatalf("render: %s", txt)
	}
	if !strings.Contains(txt, "would need CREATE") || !strings.Contains(txt, "T") {
		t.Errorf("render: %s", txt)
	}
	same := Render(Diff(a, a), "src", "dst")
	if same != "schemas identical: src == dst" {
		t.Errorf("identical render: %s", same)
	}
}
