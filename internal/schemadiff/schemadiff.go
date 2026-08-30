// Package schemadiff implements the C.3 diff surface: schema capture from
// the RDB$ catalogs, a grouped schema diff ("would need ALTER …"), and a
// bounded, key-based data diff that streams both databases ordered by their
// primary key. All capture is read-only; the data diff refuses tables above
// the configured row cap instead of silently truncating.
package schemadiff

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Column is one table column (canonical, trimmed names).
type Column struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	Default  string `json:"default,omitempty"`
}

// Table is one user table with its columns (declaration order) and PK.
type Table struct {
	Name    string   `json:"name"`
	Columns []Column `json:"columns"`
	PK      []string `json:"pk,omitempty"`
}

// Schema is a captured catalog snapshot of one database.
type Schema struct {
	Tables map[string]*Table `json:"tables"`
}

// TableNames sorted for stable output.
func (s *Schema) TableNames() []string {
	out := make([]string, 0, len(s.Tables))
	for n := range s.Tables {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Capture reads the user-table catalog (RDB$RELATIONS with
// RDB$SYSTEM_FLAG=0) with columns (RDB$RELATION_FIELDS → RDB$FIELDS for the
// canonical type text) and primary keys (RDB$RELATION_CONSTRAINTS →
// RDB$INDEX_SEGMENTS). Views are excluded (RDB$VIEW_BLR is null only for
// real tables); a view's CREATE/ALTER is not comparable to a table's.
func Capture(ctx context.Context, db *sql.DB) (*Schema, error) {
	s := &Schema{Tables: map[string]*Table{}}
	rows, err := db.QueryContext(ctx, `
		SELECT CAST(r.RDB$RELATION_NAME AS VARCHAR(66)) AS REL,
		       CAST(f.RDB$FIELD_NAME AS VARCHAR(66)) AS FLD, f.RDB$NULL_FLAG,
		       f.RDB$DEFAULT_SOURCE, y.RDB$FIELD_TYPE, y.RDB$FIELD_SUB_TYPE,
		       y.RDB$FIELD_LENGTH, y.RDB$FIELD_PRECISION, y.RDB$FIELD_SCALE,
		       f.RDB$FIELD_POSITION
		FROM RDB$RELATIONS r
		JOIN RDB$RELATION_FIELDS f ON f.RDB$RELATION_NAME = r.RDB$RELATION_NAME
		JOIN RDB$FIELDS y ON y.RDB$FIELD_NAME = f.RDB$FIELD_SOURCE
		WHERE COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0 AND r.RDB$VIEW_BLR IS NULL
		ORDER BY r.RDB$RELATION_NAME, f.RDB$FIELD_POSITION`)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var rel, fld string
		var nullFlag sql.NullInt64
		var defSrc, subType, prec sql.NullString
		var typ, length int64
		var scale sql.NullInt64
		if err := rows.Scan(&rel, &fld, &nullFlag, &defSrc, &typ, &subType, &length, &prec, &scale, new(sql.NullInt64)); err != nil {
			return nil, err
		}
		t := s.Tables[strings.TrimSpace(rel)]
		if t == nil {
			t = &Table{Name: strings.TrimSpace(rel)}
			s.Tables[t.Name] = t
		}
		t.Columns = append(t.Columns, Column{
			Name:     strings.TrimSpace(fld),
			Type:     canonicalType(typ, firstLine(subType), length, firstLine(prec), scale),
			Nullable: nullFlag.Int64 == 0,
			Default:  strings.TrimSpace(defSrc.String),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := capturePKs(ctx, db, s); err != nil {
		return nil, err
	}
	return s, nil
}

func firstLine(ns sql.NullString) string {
	if !ns.Valid {
		return ""
	}
	s := strings.TrimSpace(ns.String)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return s
}

// canonicalType renders a stable, comparable type text from the blr code +
// subtype/length/precision/scale (int128/decfloat render as their class;
// enough fidelity for drift detection, not for re-execution).
func canonicalType(typ int64, subType string, length int64, prec string, scale sql.NullInt64) string {
	base := "TYPE_" + fmt.Sprint(typ)
	switch typ {
	case 7:
		base = "SMALLINT"
	case 8:
		base = "INTEGER"
	case 10:
		base = "FLOAT"
	case 27:
		base = "DOUBLE PRECISION"
	case 12, 13, 35:
		return map[int64]string{12: "DATE", 13: "TIME", 35: "TIMESTAMP"}[typ]
	case 14:
		return fmt.Sprintf("CHAR(%d)", length) // length is bytes; charset folded
	case 37:
		return fmt.Sprintf("VARCHAR(%d)", length)
	case 40:
		return "CSTRING"
	case 9:
		base = "QUAD"
	case 26:
		base = "BOOLEAN"
	case 16: // BIGINT / INT128 by precision
		if prec != "" {
			return "INT128"
		}
		return "BIGINT"
	case 24: // DECFLOAT(16|34)
		return "DECFLOAT(" + prec + ")"
	case 261: // BLOB
		switch subType {
		case "1":
			return "BLOB SUB_TYPE TEXT"
		case "":
			return "BLOB"
		default:
			return "BLOB SUB_TYPE " + subType
		}
	}
	if scale.Valid && scale.Int64 != 0 { // scaled numerics
		return base + fmt.Sprintf(" (%d,%d)", len(fmt.Sprint(length)), -scale.Int64)
	}
	return base
}

func capturePKs(ctx context.Context, db *sql.DB, s *Schema) error {
	rows, err := db.QueryContext(ctx, `
		SELECT CAST(rc.RDB$RELATION_NAME AS VARCHAR(66)) AS REL,
		       CAST(sg.RDB$FIELD_NAME AS VARCHAR(66)) AS FLD, sg.RDB$POS
		FROM RDB$RELATION_CONSTRAINTS rc
		JOIN RDB$INDEX_SEGMENTS sg ON sg.RDB$INDEX_NAME = rc.RDB$INDEX_NAME
		WHERE rc.RDB$CONSTRAINT_TYPE = 'PRIMARY KEY'
		ORDER BY rc.RDB$RELATION_NAME, sg.RDB$POS`)
	if err != nil {
		return nil // PK detail is optional for the diff; degrade quietly
	}
	defer rows.Close()
	for rows.Next() {
		var rel, fld string
		var pos int64
		if err := rows.Scan(&rel, &fld, &pos); err != nil {
			return nil
		}
		if t := s.Tables[strings.TrimSpace(rel)]; t != nil {
			t.PK = append(t.PK, strings.TrimSpace(fld))
		}
	}
	return rows.Err()
}

// ---------------------------------------------------------------------------
// Schema diff
// ---------------------------------------------------------------------------

// TableDiff is one common table's column-level differences.
type TableDiff struct {
	Table          string   `json:"table"`
	ColumnsAdded   []string `json:"columns_added,omitempty"`   // in A, missing in B (B would need ADD)
	ColumnsDropped []string `json:"columns_dropped,omitempty"` // in B, missing in A
	ColumnsChanged []string `json:"columns_changed,omitempty"` // human one-liners
	PKChanged      bool     `json:"pk_changed,omitempty"`
}

// Result is the full schema diff A → B.
type Result struct {
	OnlyInA   []string    `json:"only_in_a,omitempty"` // tables missing in B (B would need CREATE)
	OnlyInB   []string    `json:"only_in_b,omitempty"` // tables missing in A
	Tables    []TableDiff `json:"tables,omitempty"`
	Identical bool        `json:"identical"`
}

// Empty reports no differences.
func (r Result) Empty() bool {
	return len(r.OnlyInA) == 0 && len(r.OnlyInB) == 0 && len(r.Tables) == 0
}

// Diff compares schema A (source) against B (candidate). Column diffs are
// grouped as "B would need …" actions.
func Diff(a, b *Schema) Result {
	var res Result
	for _, name := range a.TableNames() {
		tb := b.Tables[name]
		if tb == nil {
			res.OnlyInA = append(res.OnlyInA, name)
			continue
		}
		td := tableDiff(a.Tables[name], tb)
		if td != nil {
			res.Tables = append(res.Tables, *td)
		}
	}
	for _, name := range b.TableNames() {
		if a.Tables[name] == nil {
			res.OnlyInB = append(res.OnlyInB, name)
		}
	}
	res.Identical = res.Empty()
	return res
}

func tableDiff(a, b *Table) *TableDiff {
	bCols := map[string]*Column{}
	for i := range b.Columns {
		bCols[b.Columns[i].Name] = &b.Columns[i]
	}
	aCols := map[string]bool{}
	var td TableDiff
	td.Table = a.Name
	for _, ca := range a.Columns {
		aCols[ca.Name] = true
		cb := bCols[ca.Name]
		if cb == nil {
			td.ColumnsAdded = append(td.ColumnsAdded, ca.Name)
			continue
		}
		if ca.Type != cb.Type || ca.Nullable != cb.Nullable || ca.Default != cb.Default {
			what := fmt.Sprintf("%s: %s%s → %s%s", ca.Name,
				ca.Type, nullTag(ca.Nullable), cb.Type, nullTag(cb.Nullable))
			if ca.Default != cb.Default {
				what += fmt.Sprintf(" (default %q → %q)", ca.Default, cb.Default)
			}
			td.ColumnsChanged = append(td.ColumnsChanged, what)
		}
	}
	for _, cb := range b.Columns {
		if !aCols[cb.Name] {
			td.ColumnsDropped = append(td.ColumnsDropped, cb.Name)
		}
	}
	if strings.Join(a.PK, ",") != strings.Join(b.PK, ",") {
		td.PKChanged = true
	}
	if len(td.ColumnsAdded) == 0 && len(td.ColumnsDropped) == 0 && len(td.ColumnsChanged) == 0 && !td.PKChanged {
		return nil
	}
	return &td
}

func nullTag(nullable bool) string {
	if nullable {
		return " (null)"
	}
	return " not null"
}

// Render is the grouped human text ("B would need …").
func Render(r Result, nameA, nameB string) string {
	var out []string
	if r.Identical {
		return fmt.Sprintf("schemas identical: %s == %s", nameA, nameB)
	}
	if len(r.OnlyInA) > 0 {
		out = append(out, fmt.Sprintf("tables missing in %s (would need CREATE):", nameB))
		for _, t := range r.OnlyInA {
			out = append(out, "  - "+t)
		}
	}
	if len(r.OnlyInB) > 0 {
		out = append(out, fmt.Sprintf("tables only in %s (absent in %s):", nameB, nameA))
		for _, t := range r.OnlyInB {
			out = append(out, "  - "+t)
		}
	}
	for _, td := range r.Tables {
		out = append(out, fmt.Sprintf("table %s (would need ALTER on %s):", td.Table, nameB))
		for _, c := range td.ColumnsAdded {
			out = append(out, "  - ADD column "+c)
		}
		for _, c := range td.ColumnsDropped {
			out = append(out, "  - DROP column "+c)
		}
		for _, c := range td.ColumnsChanged {
			out = append(out, "  - ALTER "+c)
		}
		if td.PKChanged {
			out = append(out, "  - PRIMARY KEY differs")
		}
	}
	return strings.Join(out, "\n")
}

// ---------------------------------------------------------------------------
// Data diff (bounded, key-based)
// ---------------------------------------------------------------------------

// DataDiff is the bounded summary of one table's row differences.
type DataDiff struct {
	Table        string   `json:"table"`
	RowsA        int64    `json:"rows_a"`
	RowsB        int64    `json:"rows_b"`
	OnlyInA      int64    `json:"only_in_a"`
	OnlyInB      int64    `json:"only_in_b"`
	Different    int64    `json:"different"`
	SamplesOnlyA []string `json:"samples_only_a,omitempty"`
	SamplesOnlyB []string `json:"samples_only_b,omitempty"`
	SamplesDiff  []string `json:"samples_diff,omitempty"`
	Truncated    bool     `json:"truncated"`
}

// DataDiffOptions bounds the comparison.
type DataDiffOptions struct {
	RowCap  int // refuse (error) when either side exceeds this
	Samples int // sample rows kept per category
}

// DiffData compares one table's rows between two open databases by primary
// key, streaming both sides ordered by the key. All columns are compared as
// strings (canonical text — drift detection, not byte-level fidelity).
func DiffData(ctx context.Context, a, b *sql.DB, table string, opts DataDiffOptions) (*DataDiff, error) {
	if opts.RowCap <= 0 {
		opts.RowCap = 100_000
	}
	if opts.Samples <= 0 {
		opts.Samples = 10
	}
	key, cols, err := tableShape(ctx, a, table)
	if err != nil {
		return nil, err
	}
	if key == nil {
		return nil, fmt.Errorf("%s: no primary key — data diff is key-based; pick a table with a PK", table)
	}
	out := &DataDiff{Table: table}
	for label, dbc := range map[string]*sql.DB{"a": a, "b": b} {
		var n int64
		if err := dbc.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM "%s"`, table)).Scan(&n); err != nil {
			return nil, fmt.Errorf("%s: count: %w", label, err)
		}
		if label == "a" {
			out.RowsA = n
		} else {
			out.RowsB = n
		}
		if n > int64(opts.RowCap) {
			return nil, fmt.Errorf("%s has %d rows (cap %d) — raise row_cap or narrow the table set; refusing to diff unbounded data", table, n, opts.RowCap)
		}
	}

	order := append([]string{}, key...)
	for _, c := range cols {
		if !contains(order, c) {
			order = append(order, c)
		}
	}
	sel := quotedList(order)
	qA, err := a.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM "%s" ORDER BY %s`, sel, table, quotedList(key)))
	if err != nil {
		return nil, fmt.Errorf("read A: %w", err)
	}
	defer qA.Close()
	qB, err := b.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM "%s" ORDER BY %s`, sel, table, quotedList(key)))
	if err != nil {
		return nil, fmt.Errorf("read B: %w", err)
	}
	defer qB.Close()

	rowA, okA, err := nextRow(qA, len(order))
	if err != nil {
		return nil, err
	}
	rowB, okB, err := nextRow(qB, len(order))
	if err != nil {
		return nil, err
	}
	for okA || okB {
		cmp := 0
		switch {
		case !okA:
			cmp = 1
		case !okB:
			cmp = -1
		default:
			for i := range key {
				if c := strings.Compare(rowA[i], rowB[i]); c != 0 {
					cmp = c
					break
				}
			}
		}
		switch {
		case cmp < 0:
			out.OnlyInA++
			out.addSample(&out.SamplesOnlyA, &out.Truncated, opts.Samples, keyRow(key, rowA))
			rowA, okA, err = nextRow(qA, len(order))
		case cmp > 0:
			out.OnlyInB++
			out.addSample(&out.SamplesOnlyB, &out.Truncated, opts.Samples, keyRow(key, rowB))
			rowB, okB, err = nextRow(qB, len(order))
		default:
			for i := range order {
				if rowA[i] != rowB[i] {
					out.Different++
					out.addSample(&out.SamplesDiff, &out.Truncated, opts.Samples,
						keyRow(key, rowA)+" → "+keyRow(key, rowB))
					break
				}
			}
			rowA, okA, err = nextRow(qA, len(order))
			if err != nil {
				return nil, err
			}
			rowB, okB, err = nextRow(qB, len(order))
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (d *DataDiff) addSample(dst *[]string, truncated *bool, max int, s string) {
	if len(*dst) < max {
		*dst = append(*dst, s)
	} else {
		*truncated = true
	}
}

func keyRow(key []string, row []string) string {
	var parts []string
	for i, k := range key {
		parts = append(parts, k+"="+row[i])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func nextRow(rows *sql.Rows, n int) ([]string, bool, error) {
	if !rows.Next() {
		return nil, false, rows.Err()
	}
	vals := make([]any, n)
	ptrs := make([]any, n)
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, false, err
	}
	out := make([]string, n)
	for i, v := range vals {
		if v == nil {
			out[i] = "<null>"
			continue
		}
		if b, ok := v.([]byte); ok {
			out[i] = string(b)
		} else {
			out[i] = fmt.Sprintf("%v", v)
		}
	}
	return out, true, nil
}

// tableShape returns the PK columns and all column names of one table.
func tableShape(ctx context.Context, db *sql.DB, table string) (key, cols []string, err error) {
	rows, err := db.QueryContext(ctx, `
		SELECT CAST(f.RDB$FIELD_NAME AS VARCHAR(66)) AS FLD FROM RDB$RELATION_FIELDS f
		JOIN RDB$RELATIONS r ON r.RDB$RELATION_NAME = f.RDB$RELATION_NAME
		WHERE f.RDB$RELATION_NAME = ? ORDER BY f.RDB$FIELD_POSITION`, table)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, nil, err
		}
		cols = append(cols, strings.TrimSpace(n))
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(cols) == 0 {
		return nil, nil, fmt.Errorf("%s: table not found", table)
	}
	pkRows, err := db.QueryContext(ctx, `
		SELECT CAST(sg.RDB$FIELD_NAME AS VARCHAR(66)) AS FLD FROM RDB$RELATION_CONSTRAINTS rc
		JOIN RDB$INDEX_SEGMENTS sg ON sg.RDB$INDEX_NAME = rc.RDB$INDEX_NAME
		WHERE rc.RDB$CONSTRAINT_TYPE = 'PRIMARY KEY' AND rc.RDB$RELATION_NAME = ?
		ORDER BY sg.RDB$FIELD_POSITION`, table)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: pk lookup: %w", table, err)
	}
	defer pkRows.Close()
	for pkRows.Next() {
		var n string
		if err := pkRows.Scan(&n); err != nil {
			return nil, nil, fmt.Errorf("%s: pk scan: %w", table, err)
		}
		key = append(key, strings.TrimSpace(n))
	}
	return key, cols, pkRows.Err()
}

func quotedList(names []string) string {
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = `"` + n + `"`
	}
	return strings.Join(parts, ", ")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
