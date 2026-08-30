// Package idxadvice implements the C.2 index-advice analysis (closes the
// phase5-gap-notes "fb_index_advice does not exist" row): it parses the
// legacy isql access plan (ADR-013 route), finds natural (full-table) scans
// and sorts over them, extracts the query's sargable predicates, and proposes
// CREATE INDEX DDL with a clearly-labeled benefit estimate.
//
// Firebird has no hypothetical indexes, so a proposed index cannot be
// simulated — benefit is estimated from table row counts and an assumption
// about column distinctness, never presented as measured. The analysis is
// deliberately fail-quiet: anything it cannot prove sargable produces "no
// advice" with a reason, never a guess (subqueries, OR, ambiguous columns,
// function-wrapped predicates, existing covering indexes all yield no advice
// rather than a possibly-wrong index).
package idxadvice

import (
	"fmt"
	"regexp"
	"strings"
)

// Node kinds in the parsed plan tree.
const (
	KindScan  = "scan"
	KindSort  = "sort"
	KindJoin  = "join"
	KindGroup = "group" // parenthesized list with >1 child (defensive)
)

// Node is one element of the plan tree.
type Node struct {
	Kind     string
	Table    string   // scan
	Indexes  []string // scan: indexes used (empty = natural)
	Ordered  bool     // scan: ORDER <idx> — index provides row order
	Children []*Node  // sort/join/group
}

// IndexDef is an existing index on a table (from RDB$INDICES/SEGMENTS).
type IndexDef struct {
	Table   string
	Name    string
	Columns []string
}

// Advice is one proposed index.
type Advice struct {
	AdvisoryID string   `json:"advisory_id,omitempty"` // set by the caller after persistence
	Table      string   `json:"table"`
	Columns    []string `json:"columns"`
	Kind       string   `json:"kind"` // equality | range | join
	Reason     string   `json:"reason"`
	Estimate   string   `json:"estimate"`
	DDL        string   `json:"ddl"`
	SortNote   string   `json:"sort_note,omitempty"`
}

// Result is the full analysis outcome for one query.
type Result struct {
	Advice []Advice
	Notes  []string // why no (further) advice — surfaced to the caller
	Scans  []string // natural-scan tables found in the plan
	Sorts  []string // sort nodes sitting over natural scans
}

// ---------------------------------------------------------------------------
// Plan parsing (legacy PLAN format; whitespace/newlines normalized)
// ---------------------------------------------------------------------------

type token struct {
	kind string // "id", "(", ")", ","
	val  string
}

var tokenRe = regexp.MustCompile(`[A-Za-z0-9_$."]+|[(),]`)

func tokenize(s string) []token {
	var out []token
	for _, m := range tokenRe.FindAllString(s, -1) {
		switch m {
		case "(", ")", ",":
			out = append(out, token{kind: m})
		default:
			out = append(out, token{kind: "id", val: strings.Trim(m, `"`)})
		}
	}
	return out
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() (token, bool) {
	if p.pos >= len(p.toks) {
		return token{}, false
	}
	return p.toks[p.pos], true
}

func (p *parser) next() (token, bool) {
	t, ok := p.peek()
	if ok {
		p.pos++
	}
	return t, ok
}

// ParsePlan parses the legacy plan text ("PLAN (…)…" as printed by isql
// SET PLANONLY; newlines already collapsed by the caller are fine either way).
func ParsePlan(plan string) (*Node, error) {
	oneLine := regexp.MustCompile(`\s+`).ReplaceAllString(strings.TrimSpace(plan), " ")
	p := &parser{toks: tokenize(oneLine)}
	t, ok := p.next()
	if !ok {
		return nil, fmt.Errorf("empty plan")
	}
	if strings.EqualFold(t.val, "PLAN") {
		t, ok = p.next()
		if !ok {
			return nil, fmt.Errorf("plan ends after PLAN")
		}
		p.pos--
	}
	n, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	return n, nil
}

func (p *parser) parseExpr() (*Node, error) {
	t, ok := p.peek()
	if !ok {
		return nil, fmt.Errorf("unexpected end of plan")
	}
	switch {
	case t.kind == "(":
		return p.parseGroup()
	case t.kind == "id" && strings.EqualFold(t.val, "SORT"):
		p.next()
		child, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		return &Node{Kind: KindSort, Children: []*Node{child}}, nil
	case t.kind == "id" && strings.EqualFold(t.val, "JOIN"):
		p.next()
		kids, err := p.parseOperandList()
		if err != nil {
			return nil, err
		}
		return &Node{Kind: KindJoin, Children: kids}, nil
	case t.kind == "id":
		return p.parseScan()
	}
	return nil, fmt.Errorf("unexpected token %q", t.val)
}

// parseGroup consumes "(" expr ("," expr)* ")"; a single-child group is
// transparent (Firebird prints SORT ((T NATURAL)) etc.).
func (p *parser) parseGroup() (*Node, error) {
	if t, _ := p.next(); t.kind != "(" {
		return nil, fmt.Errorf("expected ( got %q", t.val)
	}
	var kids []*Node
	for {
		n, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		kids = append(kids, n)
		t, ok := p.next()
		if !ok {
			return nil, fmt.Errorf("unterminated group")
		}
		if t.kind == ")" {
			break
		}
		if t.kind != "," {
			return nil, fmt.Errorf("expected , or ) got %q", t.val)
		}
	}
	if len(kids) == 1 {
		return kids[0], nil
	}
	return &Node{Kind: KindGroup, Children: kids}, nil
}

// parseOperand is parseExpr for SORT/JOIN children, which may themselves be
// parenthesized groups (JOIN (SORT (…), T INDEX (…))).
func (p *parser) parseOperand() (*Node, error) {
	t, _ := p.peek()
	if t.kind == "(" {
		return p.parseGroup()
	}
	return p.parseExpr()
}

func (p *parser) parseOperandList() ([]*Node, error) {
	if t, _ := p.next(); t.kind != "(" {
		return nil, fmt.Errorf("expected ( after JOIN, got %q", t.val)
	}
	var kids []*Node
	for {
		n, err := p.parseOperand()
		if err != nil {
			return nil, err
		}
		kids = append(kids, n)
		t, ok := p.next()
		if !ok {
			return nil, fmt.Errorf("unterminated JOIN list")
		}
		if t.kind == ")" {
			return kids, nil
		}
		if t.kind != "," {
			return nil, fmt.Errorf("expected , or ) in JOIN list, got %q", t.val)
		}
	}
}

func (p *parser) parseScan() (*Node, error) {
	t, ok := p.next()
	if !ok || t.kind != "id" {
		return nil, fmt.Errorf("expected table name, got %q", t.val)
	}
	n := &Node{Kind: KindScan, Table: t.val}
	t, ok = p.next()
	if !ok {
		return n, nil // bare table (defensive; isql always prints an accessor)
	}
	switch {
	case t.kind == "id" && strings.EqualFold(t.val, "NATURAL"):
	case t.kind == "id" && strings.EqualFold(t.val, "INDEX"):
		idxs, err := p.parseIdentList()
		if err != nil {
			return nil, err
		}
		n.Indexes = idxs
	case t.kind == "id" && strings.EqualFold(t.val, "ORDER"):
		ot, ok := p.next()
		if !ok || ot.kind != "id" {
			return nil, fmt.Errorf("expected index after ORDER")
		}
		n.Ordered = true
		n.Indexes = []string{ot.val}
		if nt, _ := p.peek(); nt.kind == "id" && strings.EqualFold(nt.val, "INDEX") {
			p.next()
			idxs, err := p.parseIdentList()
			if err != nil {
				return nil, err
			}
			n.Indexes = append(n.Indexes, idxs...)
		}
	default:
		p.pos-- // not an accessor we know — leave it for the caller's grammar
	}
	return n, nil
}

func (p *parser) parseIdentList() ([]string, error) {
	if t, _ := p.next(); t.kind != "(" {
		return nil, fmt.Errorf("expected ( got %q", t.val)
	}
	var ids []string
	for {
		t, ok := p.next()
		if !ok {
			return nil, fmt.Errorf("unterminated index list")
		}
		if t.kind == ")" {
			return ids, nil
		}
		if t.kind != "," && t.kind != "id" {
			return nil, fmt.Errorf("unexpected token in index list: %q", t.val)
		}
		if t.kind == "id" {
			ids = append(ids, t.val)
		}
	}
}

// ---------------------------------------------------------------------------
// Plan findings
// ---------------------------------------------------------------------------

func collectScans(n *Node, natural *[]string) {
	if n == nil {
		return
	}
	if n.Kind == KindScan && len(n.Indexes) == 0 {
		*natural = append(*natural, n.Table)
	}
	for _, c := range n.Children {
		collectScans(c, natural)
	}
}

func collectSorts(n *Node, out *[]string) {
	if n == nil {
		return
	}
	if n.Kind == KindSort {
		var nat []string
		collectScans(n, &nat)
		if len(nat) > 0 {
			*out = append(*out, strings.Join(nat, ","))
		}
	}
	for _, c := range n.Children {
		collectSorts(c, out)
	}
}

// ---------------------------------------------------------------------------
// Predicate extraction (conservative)
// ---------------------------------------------------------------------------

var reservedAlias = map[string]bool{
	"ON": true, "WHERE": true, "USING": true, "INNER": true, "LEFT": true,
	"RIGHT": true, "FULL": true, "CROSS": true, "JOIN": true, "GROUP": true,
	"ORDER": true, "SET": true, "VALUES": true, "PLAN": true, "ROWS": true,
}

type fromTable struct {
	table string // real table name (plan references this)
	alias string // qualifier used in the SQL ("" = only qualifier is table)
}

var identChar = func(r byte) bool {
	return r == '_' || r == '$' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
}

// extractTables lists FROM/JOIN tables with their aliases. Manual scanning:
// a regex with an optional alias group would swallow the next keyword
// ("FROM A JOIN B" consumes "JOIN" as A's alias and loses B).
func extractTables(q string) []fromTable {
	upper := strings.ToUpper(q)
	var out []fromTable
	for _, kw := range []string{"FROM ", "JOIN "} {
		from := 0
		for {
			i := strings.Index(upper[from:], kw)
			if i < 0 {
				break
			}
			pos := from + i + len(kw)
			for pos < len(q) && (q[pos] == ' ' || q[pos] == '\t' || q[pos] == '\n' || q[pos] == '\r') {
				pos++
			}
			start := pos
			for pos < len(q) && identChar(q[pos]) {
				pos++
			}
			if pos > start {
				t := q[start:pos]
				// alias: the next word, when it is a plain identifier and not
				// a reserved continuation keyword
				p2 := pos
				for p2 < len(q) && (q[p2] == ' ' || q[p2] == '\t' || q[p2] == '\n' || q[p2] == '\r') {
					p2++
				}
				aStart := p2
				for p2 < len(q) && identChar(q[p2]) {
					p2++
				}
				alias := ""
				if p2 > aStart {
					w := strings.ToUpper(q[aStart:p2])
					if !reservedAlias[w] {
						alias = q[aStart:p2]
					}
				}
				out = append(out, fromTable{table: t, alias: alias})
			}
			from = pos
		}
	}
	return out
}

func extractWhere(q string) string {
	up := strings.ToUpper(q)
	i := strings.Index(up, " WHERE ")
	if i < 0 {
		i = strings.Index(up, "WHERE ")
		if i == 0 {
			i = -1 // query starting with WHERE: treat whole as predicate-less
		}
	}
	if i < 0 {
		return ""
	}
	rest := q[i+len(" WHERE "):]
	up = strings.ToUpper(rest)
	for _, kw := range []string{" GROUP ", " ORDER ", " PLAN ", " ROWS ", " WINDOW "} {
		if j := strings.Index(up, kw); j >= 0 {
			rest = rest[:j]
		}
	}
	return rest
}

// onSegments extracts explicit JOIN … ON <cond> clause bodies (joins put
// their sargable predicates in ON, not WHERE). RE2 has no lookahead, so the
// segment is cut manually at the next boundary keyword.
var onBoundaries = []string{" WHERE ", " JOIN ", " GROUP ", " ORDER ", " PLAN ", " ROWS ", " ON ", " INNER ", " LEFT ", " RIGHT ", " FULL ", " CROSS "}

func onSegments(q string) []string {
	up := strings.ToUpper(q)
	var out []string
	from := 0
	for {
		i := strings.Index(up[from:], " ON ")
		if i < 0 {
			break
		}
		start := from + i + len(" ON ")
		end := len(q)
		for _, kw := range onBoundaries {
			if j := strings.Index(up[start:], kw); j >= 0 && start+j < end {
				end = start + j
			}
		}
		if s := strings.TrimSpace(q[start:end]); s != "" {
			out = append(out, s)
		}
		from = start
	}
	return out
}

// splitAnd splits on top-level AND, treating BETWEEN x AND y as one conjunct.
func splitAnd(w string) []string {
	var out []string
	depth, inStr := 0, false
	start := 0
	upper := strings.ToUpper(w)
	for i := 0; i < len(w); i++ {
		switch w[i] {
		case '\'':
			inStr = !inStr
		case '(':
			if !inStr {
				depth++
			}
		case ')':
			if !inStr {
				depth--
			}
		}
		if inStr || depth != 0 {
			continue
		}
		if strings.HasPrefix(upper[i:], " AND ") {
			before := strings.TrimSpace(upper[:i])
			// an AND that belongs to BETWEEN <lo> AND <hi>: the token before it
			// ends the low bound and a BETWEEN appears earlier in this conjunct
			if j := strings.LastIndex(before, " BETWEEN "); j >= 0 {
				bound := before[j+len(" BETWEEN "):]
				if bound != "" && !strings.ContainsAny(bound, " ()=<>") {
					continue // part of a BETWEEN pair
				}
			}
			out = append(out, strings.TrimSpace(w[start:i]))
			start = i + len(" AND ")
		}
	}
	out = append(out, strings.TrimSpace(w[start:]))
	return out
}

// column reference + predicate patterns (applied per conjunct, uppercased)
var (
	reJoin      = regexp.MustCompile(`^\(?([A-Za-z0-9_$]+)\.([A-Za-z0-9_$]+)\s*=\s*([A-Za-z0-9_$]+)\.([A-Za-z0-9_$]+)\)?$`)
	reEq        = regexp.MustCompile(`^\(?([A-Za-z0-9_$]+)\.([A-Za-z0-9_$]+)\s*=\s*(?:'[^']*'|\d+(?:\.\d+)?|[?:][A-Za-z0-9_$]*|NULL)\)?$`)
	reUnqualEq  = regexp.MustCompile(`^\(?([A-Za-z0-9_$]+)\s*=\s*(?:'[^']*'|\d+(?:\.\d+)?|[?:][A-Za-z0-9_$]*)\)?$`)
	reRange     = regexp.MustCompile(`^\(?([A-Za-z0-9_$]+)\.([A-Za-z0-9_$]+)\s*(?:>=|<=|>|<|BETWEEN\b)`)
	reUnqualRng = regexp.MustCompile(`^\(?([A-Za-z0-9_$]+)\s+(?:>=|<=|>|<|BETWEEN\b)`)
	reInList    = regexp.MustCompile(`^\(?([A-Za-z0-9_$]+)\.([A-Za-z0-9_$]+)\s+IN\s+\([^()]*\)\)?$`)
	reLikePre   = regexp.MustCompile(`^\(?([A-Za-z0-9_$]+)\.([A-Za-z0-9_$]+)\s+LIKE\s+'[^'%]*%'`)
)

type colRef struct {
	table string // qualifier as written (alias or table name)
	col   string
}

// extractPredicates returns sargable equality, range and join column refs.
// Function-wrapped / <> / NOT / mid-LIKE predicates are ignored (nonsargable).
func extractPredicates(q string) (eq, rng, join []colRef) {
	parts := onSegments(q)
	if w := extractWhere(q); strings.TrimSpace(w) != "" {
		parts = append(parts, w)
	}
	if len(parts) == 0 {
		return nil, nil, nil
	}
	var conjuncts []string
	for _, p := range parts {
		conjuncts = append(conjuncts, splitAnd(p)...)
	}
	for _, c := range conjuncts {
		upper := strings.ToUpper(c)
		if strings.Contains(upper, " NOT ") || strings.Contains(upper, "<>") || strings.Contains(upper, "!=") {
			continue
		}
		// strip a function call wrapper means the column is inside FUNC(…) —
		// detect by the conjunct not matching any column-leading pattern
		if m := reJoin.FindStringSubmatch(upper); m != nil {
			join = append(join, colRef{m[1], m[2]}, colRef{m[3], m[4]})
			continue
		}
		if m := reEq.FindStringSubmatch(upper); m != nil {
			eq = append(eq, colRef{m[1], m[2]})
			continue
		}
		if m := reInList.FindStringSubmatch(upper); m != nil {
			eq = append(eq, colRef{m[1], m[2]})
			continue
		}
		if m := reLikePre.FindStringSubmatch(upper); m != nil {
			eq = append(eq, colRef{m[1], m[2]})
			continue
		}
		if m := reRange.FindStringSubmatch(upper); m != nil {
			rng = append(rng, colRef{m[1], m[2]})
			continue
		}
		// unqualified forms: usable only when the query has exactly one table
		if m := reUnqualEq.FindStringSubmatch(upper); m != nil {
			eq = append(eq, colRef{"", m[1]})
			continue
		}
		if m := reUnqualRng.FindStringSubmatch(upper); m != nil {
			rng = append(rng, colRef{"", m[1]})
		}
	}
	return eq, rng, join
}

// ---------------------------------------------------------------------------
// Advice construction
// ---------------------------------------------------------------------------

// RowsFn returns a table's approximate row count (ok=false: unknown).
type RowsFn func(table string) (int64, bool)

// Analyze produces index advice for one query + its parsed plan.
// existing lists the database's current indexes; rows estimates row counts.
func Analyze(query string, plan *Node, existing []IndexDef, rows RowsFn) Result {
	var res Result
	collectScans(plan, &res.Scans)
	collectSorts(plan, &res.Sorts)
	if len(res.Scans) == 0 {
		res.Notes = append(res.Notes, "no natural (full) table scans in the plan — nothing to advise")
		return res
	}

	if len(regexp.MustCompile(`(?i)\bSELECT\b`).FindAllStringIndex(query, -1)) > 1 {
		res.Notes = append(res.Notes, "query contains a subquery — analysis is skipped (conservative: no advice rather than a possibly-wrong index)")
		return res
	}
	if regexp.MustCompile(`(?i)\bOR\b`).MatchString(extractWhere(query)) {
		res.Notes = append(res.Notes, "WHERE contains OR — analysis is skipped (OR predicates are not reliably indexable)")
		return res
	}

	tables := extractTables(query)
	qualifierToTable := map[string]string{} // alias-or-name → table name
	for _, t := range tables {
		qual := t.alias
		if qual == "" {
			qual = t.table
		}
		qualifierToTable[strings.ToUpper(qual)] = t.table
	}
	single := len(tables) == 1

	eq, rng, join := extractPredicates(query)
	// columns per real table name, bucketed
	perTable := map[string]map[string]map[string]bool{} // table → kind → col set
	add := func(table, kind, col string) {
		if perTable[table] == nil {
			perTable[table] = map[string]map[string]bool{}
		}
		if perTable[table][kind] == nil {
			perTable[table][kind] = map[string]bool{}
		}
		perTable[table][kind][strings.ToUpper(col)] = true
	}
	resolve := func(ref colRef) (string, bool) {
		if ref.table != "" {
			if t, ok := qualifierToTable[ref.table]; ok {
				return t, true
			}
			return "", false // qualifier not among FROM tables — ignore
		}
		if single {
			return tables[0].table, true
		}
		return "", false // ambiguous unqualified column
	}
	for _, r := range eq {
		if t, ok := resolve(r); ok {
			add(t, "equality", r.col)
		}
	}
	for _, r := range rng {
		if t, ok := resolve(r); ok {
			add(t, "range", r.col)
		}
	}
	for _, r := range join {
		if t, ok := resolve(r); ok {
			add(t, "join", r.col)
		}
	}

	for _, scan := range res.Scans {
		cols := perTable[strings.ToUpper(scan)]
		if len(cols) == 0 {
			res.Notes = append(res.Notes, fmt.Sprintf("%s: natural scan but no sargable predicate found (function-wrapped, ambiguous or absent) — no advice", scan))
			continue
		}
		var ordered []string // equality columns first, then join, then range
		for _, kind := range []string{"equality", "join", "range"} {
			ordered = appendSet(ordered, cols[kind])
		}
		if covered, by := coveringIndex(existing, strings.ToUpper(scan), ordered); covered {
			res.Notes = append(res.Notes, fmt.Sprintf("%s: natural scan but an existing index (%s) already covers %v — the engine chose not to use it; verify with fb_analyze_query after SET STATISTICS (fb_index_rebuild)", scan, by, ordered))
			continue
		}
		kind := adviceKind(cols)
		rowCount, known := rows(strings.ToUpper(scan))
		est := estimate(kind, rowCount, known)
		name := indexName(scan, ordered, existing)
		a := Advice{
			Table:    strings.ToUpper(scan),
			Columns:  ordered,
			Kind:     kind,
			Reason:   fmt.Sprintf("natural (full) scan on %s in the query plan", strings.ToUpper(scan)),
			Estimate: est,
			DDL:      fmt.Sprintf("CREATE INDEX %s ON %s (%s);", name, strings.ToUpper(scan), strings.Join(ordered, ", ")),
		}
		for _, s := range res.Sorts {
			if strings.Contains(strings.ToUpper(s), strings.ToUpper(scan)) {
				a.SortNote = "a SORT sits over this scan; an index on these columns may let the engine read in order and drop the sort — not guaranteed"
			}
		}
		res.Advice = append(res.Advice, a)
	}
	return res
}

func appendSet(dst []string, set map[string]bool) []string {
	for c := range set { // sets are tiny; order within a kind is immaterial
		dst = append(dst, c)
	}
	return dst
}

func adviceKind(cols map[string]map[string]bool) string {
	switch {
	case len(cols["equality"]) > 0:
		return "equality"
	case len(cols["join"]) > 0:
		return "join"
	default:
		return "range"
	}
}

// coveringIndex reports whether an existing index's leading columns are
// exactly the candidate's leading columns (a prefix that the optimizer can
// use); conservative: only exact-prefix matches suppress advice.
func coveringIndex(existing []IndexDef, table string, cols []string) (bool, string) {
	for _, ix := range existing {
		if ix.Table != table || len(ix.Columns) == 0 || len(ix.Columns) < len(cols) {
			continue
		}
		match := true
		for i, c := range cols {
			if ix.Columns[i] != c {
				match = false
				break
			}
		}
		if match {
			return true, ix.Name
		}
	}
	return false, ""
}

// indexName builds IDX_ADVICE_<TABLE>_<COLS>, kept within Firebird's
// 31-byte classic identifier limit, de-duplicated against existing names.
func indexName(table string, cols []string, existing []IndexDef) string {
	base := "IDX_ADVICE_" + table + "_" + strings.Join(cols, "_")
	if len(base) > 31 {
		base = base[:31]
	}
	names := map[string]bool{}
	for _, ix := range existing {
		names[ix.Name] = true
	}
	name := base
	for i := 2; names[name]; i++ {
		suffix := fmt.Sprintf("_%d", i)
		stem := base
		if len(stem) > 31-len(suffix) {
			stem = stem[:31-len(suffix)]
		}
		name = stem + suffix
	}
	return name
}

func estimate(kind string, rowCount int64, known bool) string {
	if !known || rowCount <= 0 {
		return "row count unknown — benefit unquantified (estimate only)"
	}
	var est int64
	switch kind {
	case "equality":
		est = rowCount / 100 // assumed distinctness 1/100 — stated assumption, not measured
	case "join":
		est = rowCount / 10
	default:
		est = rowCount / 3
	}
	if est < 1 {
		est = 1
	}
	return fmt.Sprintf("full scan reads ~%d rows; an index lookup would touch ~%d (assumed distinctness — estimate only, Firebird cannot simulate an uncreated index)", rowCount, est)
}
