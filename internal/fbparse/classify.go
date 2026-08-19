package fbparse

import "strings"

// classify turns one split result into a Statement. It never fails and
// never panics (NFR-2): anything unrecognized degrades to Unknown with an
// issue, never to a read verb.
func classify(rs rawStmt, input string, cfg *config) Statement {
	s := Statement{
		Raw:        input[rs.span.Start:rs.span.End],
		Span:       rs.span,
		Verb:       VerbUnknown,
		Mutating:   true,
		Confidence: ConfidenceLow,
	}
	s.Issues = append(s.Issues, rs.issues...)

	toks, lexIssues := lexAll(s.Raw, cfg)
	for _, iss := range lexIssues {
		iss.Offset += rs.span.Start // report against the Parse input
		s.Issues = append(s.Issues, iss)
	}

	// Any unclosed literal/comment (FR-3) or malformed lexeme makes the
	// whole statement untrustworthy.
	for _, iss := range s.Issues {
		if iss.Kind == IssueUnclosedLiteral {
			s.Issues = append(s.Issues, Issue{Kind: IssueAmbiguousParse,
				Msg: "fbparse: lexical oddity prevents classification", Offset: rs.span.Start})
			return s
		}
	}

	if len(toks) == 0 {
		// Synthetic statement for garbage-only input (FR-3).
		return s
	}

	// Dialect-1 quoting (FR-3, NFR-7): in dialect-3 mode a strong signal
	// that " is used for strings forces Unknown; weak value-position
	// signals only flag the ambiguity. In dialect-1 mode we parse
	// leniently and flag.
	if cfg.dialect == Dialect3 {
		if off, strong, found := dialect1Signal(toks, s.Raw); found {
			if strong {
				s.Issues = append(s.Issues, Issue{Kind: IssueDialect1Quoting,
					Msg: "fbparse: double-quoted token used in a string position (dialect-1 quoting?)", Offset: rs.span.Start + off})
				return s
			}
			s.Issues = append(s.Issues, Issue{Kind: IssueDialect1Quoting,
				Msg: "fbparse: double-quoted token in a value position is ambiguous (dialect-1 quoting?)", Offset: rs.span.Start + off})
		}
	} else if off, ok := firstDQuoteString(toks, s.Raw); ok {
		s.Issues = append(s.Issues, Issue{Kind: IssueDialect1Quoting,
			Msg: "fbparse: dialect-1 double-quoted string parsed leniently", Offset: rs.span.Start + off})
	}

	c := &classifier{in: s.Raw, toks: toks, cfg: cfg, s: &s}
	c.depth = parenDepths(toks, s.Raw)
	s.dialect1 = cfg.dialect == Dialect1
	c.run()
	s.finalizeConfidence()
	return s
}

func (s *Statement) finalizeConfidence() {
	if s.Verb == VerbUnknown {
		s.Confidence = ConfidenceLow
		s.Mutating = true // conservative: never map unclassifiable to read
		return
	}
	if len(s.Issues) == 0 {
		s.Confidence = ConfidenceHigh
		return
	}
	onlyHeuristic := true
	for _, iss := range s.Issues {
		if iss.Kind != IssueHeuristicVersion {
			onlyHeuristic = false
			break
		}
	}
	if onlyHeuristic {
		s.Confidence = ConfidenceMedium
	} else {
		s.Confidence = ConfidenceLow
	}
}

func (s *Statement) addIssue(k IssueKind, msg string, off int) {
	s.Issues = append(s.Issues, Issue{Kind: k, Msg: msg, Offset: off})
}

// raiseVersion applies the FR-12 monotone floor: detection only ever
// raises; "" stays unknown, never a claimed "2.5-safe".
func (s *Statement) raiseVersion(v string, off int) {
	if v == "" || (s.MinVersion != "" && s.MinVersion >= v) {
		return
	}
	s.MinVersion = v
	s.addIssue(IssueHeuristicVersion,
		"fbparse: minimum version "+v+" detected heuristically; certification is the server's job (fb_info)", off)
}

// ---------------------------------------------------------------------------
// classifier

type classifier struct {
	in    string
	toks  []token
	depth []int // paren depth of each token
	cfg   *config
	s     *Statement
	i     int
}

func (c *classifier) tok(off int) *token {
	if c.i+off < len(c.toks) {
		return &c.toks[c.i+off]
	}
	return nil
}

func (c *classifier) at(off int, w string) bool {
	t := c.tok(off)
	return t != nil && t.isWord(w)
}

func (c *classifier) sym(off int) string {
	t := c.tok(off)
	if t == nil || t.kind != tkSymbol {
		return ""
	}
	return t.text(c.in)
}

func (c *classifier) text(t *token) string { return t.text(c.in) }

// readName consumes a possibly qualified name (word|qident|number
// ('.' part)*). Returns the normalized ref, the raw as-written text, and
// whether a name was found at all.
func (c *classifier) readName() (ObjectRef, string, bool) {
	var ref ObjectRef
	var rawTxt string
	t := c.tok(0)
	if t == nil {
		return ref, "", false
	}
	part := func() (ObjectRef, string, bool) {
		t := c.tok(0)
		if t == nil {
			return ObjectRef{}, "", false
		}
		switch t.kind {
		case tkQIdent:
			return ObjectRef{Name: unquote(c.text(t)), Quoted: true}, c.text(t), true
		case tkWord, tkNumber:
			return ObjectRef{Name: c.text(t)}, c.text(t), true
		}
		return ObjectRef{}, "", false
	}
	p, r, ok := part()
	if !ok {
		return ref, "", false
	}
	ref.Name, rawTxt = p.Name, r
	ref.Quoted = p.Quoted
	c.i++
	for c.sym(0) == "." && c.tok(1) != nil && (c.tok(1).kind == tkWord || c.tok(1).kind == tkQIdent) {
		c.i++ // '.'
		p, r, ok = part()
		if !ok {
			c.i--
			break
		}
		c.i++
		ref.Name += "." + p.Name
		ref.Quoted = ref.Quoted || p.Quoted
		rawTxt += "." + r
	}
	return ref, rawTxt, true
}

func (c *classifier) setTarget(ot ObjectType) {
	ref, raw, ok := c.readName()
	if !ok {
		off := len(c.in)
		if t := c.tok(0); t != nil {
			off = t.start
		}
		c.s.addIssue(IssueUnsupportedConstruct, "fbparse: expected object name after "+string(ot), off)
		return
	}
	c.s.ObjectType = ot
	c.s.Object = ref
	c.s.rawTarget = raw
}

// raiseVersion applies the FR-12 monotone version floor.
func (c *classifier) raiseVersion(v string, off int) {
	c.s.raiseVersion(v, off)
}

func parenDepths(toks []token, in string) []int {
	d := make([]int, len(toks))
	cur := 0
	for i, t := range toks {
		txt := t.text(in)
		switch {
		case t.kind == tkSymbol && txt == "(":
			d[i] = cur
			cur++
		case t.kind == tkSymbol && txt == ")":
			if cur > 0 {
				cur--
			}
			d[i] = cur
		default:
			d[i] = cur
		}
	}
	return d
}

// ---------------------------------------------------------------------------
// dispatch

func (c *classifier) run() {
	t := c.toks[0]
	if t.kind != tkWord {
		c.unknown("fbparse: statement does not start with a keyword")
		return
	}
	switch t.upper {
	case "SELECT":
		c.selectStmt()
	case "WITH":
		c.withStmt()
	case "INSERT":
		c.insertStmt()
	case "UPDATE":
		c.updateStmt()
	case "DELETE":
		c.deleteStmt()
	case "MERGE":
		c.mergeStmt()
	case "EXECUTE":
		c.executeStmt()
	case "CREATE":
		c.createStmt(VerbCreate)
	case "RECREATE":
		c.createStmt(VerbRecreate)
	case "DROP":
		c.dropStmt()
	case "ALTER":
		c.alterStmt()
	case "GRANT":
		c.dclStmt(true)
	case "REVOKE":
		c.dclStmt(false)
	case "COMMENT":
		c.commentStmt()
	case "DECLARE":
		c.declareStmt()
	case "SET":
		c.setStmt()
	case "COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE":
		c.unknown("fbparse: transaction-control statement is outside the classification vocabulary")
	case "CONNECT", "DISCONNECT":
		c.unknown("fbparse: session statement is outside the classification vocabulary")
	case "TRUNCATE":
		c.unknown("fbparse: Firebird has no TRUNCATE (lookalike of other dialects)")
	case "USE":
		c.unknown("fbparse: Firebird has no USE (lookalike of other dialects)")
	case "CALL", "USING", "VALUES":
		c.unknown("fbparse: statement form not supported in the Firebird versions in scope")
	default:
		c.unknown("fbparse: unrecognized leading keyword " + t.upper)
	}
}

func (c *classifier) unknown(msg string) {
	c.s.addIssue(IssueUnsupportedConstruct, msg, c.toks[0].start)
	// Verb stays Unknown; finalizeConfidence handles the rest.
}

// ---------------------------------------------------------------------------
// reads and DML (FR-7, FR-9, FR-10)

func (c *classifier) selectStmt() {
	c.s.Verb = VerbSelect
	c.s.Mutating = false
	c.s.Reversibility = ReversibilityNone
	c.detectWithLock()
}

func (c *classifier) withStmt() {
	// WITH ... SELECT is the only read form; WITH must be followed by a
	// top-level SELECT somewhere (CTE prefix).
	for k := 1; k < len(c.toks); k++ {
		if c.toks[k].isWord("SELECT") && c.depth[k] == 0 {
			c.s.Verb = VerbSelect
			c.s.Mutating = false
			c.s.Reversibility = ReversibilityNone
			c.detectWithLock()
			return
		}
	}
	c.unknown("fbparse: WITH without a top-level SELECT")
}

func (c *classifier) detectWithLock() {
	for k := 0; k+1 < len(c.toks); k++ {
		if c.toks[k].isWord("WITH") && c.toks[k+1].isWord("LOCK") && c.depth[k] == 0 {
			c.s.Flags.WithLock = true
			c.s.variant = varWithLock
			return
		}
	}
}

func (c *classifier) insertStmt() {
	c.s.Verb = VerbInsert
	c.i = 1
	if c.at(0, "INTO") {
		c.i++
	}
	c.setTarget(ObjTable)
	c.s.Mutating = true
	c.s.Reversibility = ReversibilityRestorePoint
	// INSERT INTO t SELECT ...: source tables are Secondary (best-effort).
	for k := c.i; k < len(c.toks); k++ {
		if c.toks[k].isWord("SELECT") {
			c.collectFromSources(k, c.depth[k])
			break
		}
	}
}

func (c *classifier) updateStmt() {
	c.s.Verb = VerbUpdate
	c.i = 1
	if c.at(0, "OR") && c.at(1, "INSERT") {
		c.s.Flags.Upsert = true
		c.s.variant = varOrInsert
		c.i += 2
		if c.at(0, "INTO") {
			c.i++
		}
	}
	c.setTarget(ObjTable)
	c.s.Mutating = true
	c.s.Reversibility = ReversibilityRestorePoint
	if !c.s.Flags.Upsert {
		c.extractWhere("PLAN", "ORDER", "ROWS", "RETURNING", "WITH")
	}
	c.detectWithLock()
	if c.s.Flags.WithLock && c.s.variant == "" {
		c.s.variant = varWithLock
	}
}

func (c *classifier) deleteStmt() {
	c.s.Verb = VerbDelete
	c.i = 1
	if c.at(0, "FROM") {
		c.i++
	}
	c.setTarget(ObjTable)
	c.s.Mutating = true
	c.s.Reversibility = ReversibilityRestorePoint
	c.extractWhere("PLAN", "ORDER", "ROWS", "SKIP", "RETURNING")
}

// extractWhere captures the depth-0 WHERE span for UPDATE/DELETE, stopping
// before the given trailing-clause keywords (FR-10: text span,
// uninterpreted).
func (c *classifier) extractWhere(stoppers ...string) {
	whereTok := -1
	for k := c.i; k < len(c.toks); k++ {
		if c.toks[k].isWord("WHERE") && c.depth[k] == 0 {
			whereTok = k
			break
		}
	}
	if whereTok < 0 {
		return
	}
	end := len(c.in)
	for k := whereTok + 1; k < len(c.toks); k++ {
		if c.depth[k] != 0 {
			continue
		}
		t := c.toks[k]
		if t.kind == tkWord {
			for _, st := range stoppers {
				if t.upper == st {
					end = t.start
					break
				}
			}
			if end != len(c.in) {
				break
			}
		}
	}
	c.s.Where = strings.TrimSpace(c.in[c.toks[whereTok].start:end])
}

func (c *classifier) mergeStmt() {
	c.s.Verb = VerbMerge
	c.i = 1
	if c.at(0, "INTO") {
		c.i++
	}
	c.setTarget(ObjTable)
	c.s.Mutating = true
	c.s.Reversibility = ReversibilityRestorePoint
	if c.at(0, "USING") {
		c.i++
		if c.sym(0) == "(" {
			// USING (sub-select): extract inner sources best-effort.
			base := c.depth[c.i]
			c.collectFromSources(c.i+1, base+1)
			c.s.variant = varMergeSubQ
			c.s.addIssue(IssueAmbiguousParse,
				"fbparse: MERGE source is a sub-select; Secondary objects are best-effort", c.toks[c.i].start)
		} else if ref, _, ok := c.readName(); ok {
			c.s.Secondary = append(c.s.Secondary, ref)
		}
	} else {
		c.s.addIssue(IssueAmbiguousParse, "fbparse: MERGE without USING", c.i)
	}
}

func (c *classifier) symAt(k int) string {
	if k < len(c.toks) && c.toks[k].kind == tkSymbol {
		return c.text(&c.toks[k])
	}
	return ""
}

// collectFromSources appends tables named by FROM/JOIN between token k and
// the next clause keyword at the same depth (best-effort Secondary).
func (c *classifier) collectFromSources(k, depth int) {
	save := c.i
	c.i = k
	for c.i < len(c.toks) && c.depth[c.i] >= depth {
		if c.depth[c.i] == depth && c.toks[c.i].kind == tkWord {
			w := c.toks[c.i].upper
			if w == "FROM" || w == "JOIN" {
				c.i++
				if ref, _, ok := c.readName(); ok {
					c.s.Secondary = append(c.s.Secondary, ref)
				}
				continue
			}
			// Clause end at this depth stops the scan.
			switch w {
			case "WHERE", "GROUP", "HAVING", "ORDER", "PLAN", "UNION", "SET", "ON", "USING", "WHEN", "RETURNING":
				c.i = save
				return
			}
		}
		c.i++
	}
	c.i = save
}

// ---------------------------------------------------------------------------
// EXECUTE (FR-2)

func (c *classifier) executeStmt() {
	switch {
	case c.at(1, "BLOCK"):
		c.executeBlock()
	case c.at(1, "PROCEDURE"):
		c.s.Verb = VerbExecuteProc
		c.i = 2
		c.setTarget(ObjProcedure)
		c.s.Mutating = true
		c.s.Reversibility = ReversibilityRestorePoint
	default:
		c.unknown("fbparse: expected BLOCK or PROCEDURE after EXECUTE")
	}
}

func (c *classifier) executeBlock() {
	c.s.Verb = VerbExecuteBlock
	c.s.Mutating = true // conservative: an anonymous block can do anything
	c.s.Reversibility = ReversibilityRestorePoint
	c.scanBody(1)
}

// scanBody analyzes the PSQL body region of EXECUTE BLOCK and
// CREATE/ALTER/RECREATE ... PROCEDURE|FUNCTION|TRIGGER|PACKAGE BODY,
// starting the search for AS at token offset base.
func (c *classifier) scanBody(base int) {
	asIdx := -1
	for k := base; k < len(c.toks); k++ {
		if c.toks[k].isWord("AS") && c.depth[k] == 0 {
			asIdx = k
			break
		}
	}
	beginIdx := -1
	for k := base; k < len(c.toks); k++ {
		if c.toks[k].isWord("BEGIN") && c.depth[k] == 0 {
			beginIdx = k
			break
		}
	}
	if asIdx < 0 && beginIdx < 0 {
		return // external body: no PSQL
	}

	bodyFrom := len(c.in)
	switch {
	case beginIdx >= 0:
		bodyFrom = c.toks[beginIdx].start
	case asIdx >= 0:
		bodyFrom = c.toks[asIdx].end
	}
	body := &BodyInfo{Bytes: len(c.in) - bodyFrom}
	seen := map[Verb]bool{}
	start := asIdx
	if start < 0 {
		start = beginIdx
	}
	if start < 0 {
		return
	}
	for k := start; k < len(c.toks); k++ {
		t := c.toks[k]
		if t.kind != tkWord {
			continue
		}
		var v Verb
		switch t.upper {
		case "INSERT", "UPDATE", "DELETE", "MERGE", "SELECT":
			v = Verb(t.upper)
			if t.upper != "SELECT" {
				body.HasDML = true
			}
			// Best-effort DML targets inside the body (FR-9).
			c.bodyDMLOrigin(k, t.upper)
		case "EXECUTE":
			if k+1 < len(c.toks) && c.toks[k+1].isWord("PROCEDURE") {
				v = VerbExecuteProc
			} else if k+1 < len(c.toks) && c.toks[k+1].isWord("BLOCK") {
				v = VerbExecuteBlock
			}
		}
		if v != "" && !seen[v] {
			seen[v] = true
			body.EmbeddedVerbs = append(body.EmbeddedVerbs, v)
		}
	}
	c.s.Body = body
	if body.HasDML && len(c.s.Secondary) > 0 {
		c.s.addIssue(IssueAmbiguousParse,
			"fbparse: DML targets inside the PSQL body are best-effort", start)
	}
}

// bodyDMLOrigin extracts INSERT INTO t / UPDATE t / DELETE FROM t /
// MERGE INTO t targets inside a PSQL body (best-effort).
func (c *classifier) bodyDMLOrigin(k int, verb string) {
	var ref ObjectRef
	switch verb {
	case "INSERT", "MERGE":
		if k+2 < len(c.toks) && c.toks[k+1].isWord("INTO") {
			save := c.i
			c.i = k + 2
			ref, _, _ = c.readName()
			c.i = save
		}
	case "DELETE":
		if k+2 < len(c.toks) && c.toks[k+1].isWord("FROM") {
			save := c.i
			c.i = k + 2
			ref, _, _ = c.readName()
			c.i = save
		}
	case "UPDATE":
		if k+1 < len(c.toks) && (c.toks[k+1].kind == tkWord || c.toks[k+1].kind == tkQIdent) &&
			!c.toks[k+1].isWord("OR") {
			save := c.i
			c.i = k + 1
			ref, _, _ = c.readName()
			c.i = save
		}
	}
	if ref.Name != "" {
		for _, e := range c.s.Secondary {
			if e == ref {
				return
			}
		}
		c.s.Secondary = append(c.s.Secondary, ref)
	}
}

// ---------------------------------------------------------------------------
// CREATE family (FR-4, FR-5, FR-6, FR-12)

func (c *classifier) createStmt(verb Verb) {
	c.i = 1
	if verb == VerbCreate && c.at(0, "OR") && c.at(1, "ALTER") {
		verb = VerbCreateOrAlter
		c.i += 2
	}
	c.s.Verb = verb

	// IF NOT EXISTS is noise for classification.
	if c.at(0, "IF") && c.at(1, "NOT") && c.at(2, "EXISTS") {
		c.s.Flags.setExtra("if_not_exists", "1")
		c.i += 3
	}

	switch {
	case c.at(0, "EXCEPTION"):
		c.i++
		c.finishNamed(ObjException)
	case c.at(0, "UNIQUE") || c.at(0, "ASC") || c.at(0, "ASCENDING") ||
		c.at(0, "DESC") || c.at(0, "DESCENDING") || c.at(0, "INDEX"):
		c.indexStmt()
	case c.at(0, "AGGREGATE"):
		c.i++
		if c.at(0, "FUNCTION") {
			c.i++
			c.s.variant = varFunctionAggr
			c.functionRest()
		} else {
			c.unknown("fbparse: expected FUNCTION after AGGREGATE")
		}
	case c.at(0, "FUNCTION"):
		c.i++
		c.functionRest()
	case c.at(0, "PROCEDURE"):
		c.i++
		c.finishNamed(ObjProcedure)
		c.scanBody(1)
	case c.at(0, "TABLE"):
		c.i++
		c.finishNamed(ObjTable)
		c.detectTypeVersions()
	case c.at(0, "GLOBAL") && c.at(1, "TEMPORARY") && c.at(2, "TABLE"):
		c.i += 3
		c.finishNamed(ObjGlobalTempTable)
		c.detectTypeVersions()
	case c.at(0, "LOCAL") && c.at(1, "TEMPORARY") && c.at(2, "TABLE"):
		c.unknown("fbparse: LOCAL TEMPORARY TABLE is outside the supported vocabulary")
	case c.at(0, "EXTERNAL") && c.at(1, "TABLE"):
		c.i += 2
		c.finishNamed(ObjExternalTable)
	case c.at(0, "TRIGGER"):
		c.i++
		c.triggerRest()
	case c.at(0, "VIEW"):
		c.i++
		c.finishNamed(ObjView)
	case c.at(0, "GENERATOR"), c.at(0, "SEQUENCE"):
		c.i++
		c.finishNamed(ObjSequence)
	case c.at(0, "DATABASE"):
		c.i++
		c.databaseName()
	case c.at(0, "DOMAIN"):
		c.i++
		c.finishNamed(ObjDomain)
		c.detectTypeVersions()
	case c.at(0, "SHADOW"):
		c.i++
		c.finishNamed(ObjShadow)
	case c.at(0, "ROLE"):
		c.i++
		c.finishNamed(ObjRole)
	case c.at(0, "COLLATION"):
		c.unknown("fbparse: COLLATION statements are outside the object vocabulary")
	case c.at(0, "CHARACTER") && c.at(1, "SET"):
		c.unknown("fbparse: CHARACTER SET statements are outside the object vocabulary")
	case c.at(0, "USER"):
		c.i++
		c.finishNamed(ObjUser)
	case c.at(0, "PACKAGE"):
		c.i++
		if c.at(0, "BODY") {
			c.i++
			c.s.variant = varPackageBody
			c.finishNamed(ObjPackage)
			c.raiseVersion("3.0", c.toks[0].start)
			c.scanBody(1)
		} else {
			c.finishNamed(ObjPackage)
			c.raiseVersion("3.0", c.toks[0].start)
		}
	case c.at(0, "MAPPING") || (c.at(0, "GLOBAL") && c.at(1, "MAPPING")):
		if c.at(0, "GLOBAL") {
			c.s.Flags.setExtra("global", "1")
			c.i++
		}
		c.i++
		c.finishNamed(ObjMapping)
	case c.at(0, "SCHEMA"):
		c.unknown("fbparse: SCHEMA statements are outside the supported vocabulary")
	default:
		c.unknown("fbparse: unrecognized CREATE object")
	}

	c.s.Mutating = true
	if c.s.Reversibility == ReversibilityNone {
		c.s.Reversibility = ReversibilityReverseDDL
	}
}

// finishNamed reads the object name at the cursor into the statement.
func (c *classifier) finishNamed(ot ObjectType) {
	c.setTarget(ot)
}

// databaseName reads the CREATE DATABASE target, which the grammar types
// as a string literal ('file') though a bare token is tolerated.
func (c *classifier) databaseName() {
	c.s.ObjectType = ObjDatabase
	t := c.tok(0)
	if t == nil {
		return
	}
	if t.kind == tkString {
		name := c.text(t)
		name = strings.Trim(name, "'")
		name = strings.ReplaceAll(name, "''", "'")
		c.s.Object = ObjectRef{Name: name}
		c.s.rawTarget = c.text(t)
		c.i++
		return
	}
	c.setTarget(ObjDatabase)
}

// detectTypeVersions scans type keywords anywhere in the statement for the
// FR-12 version floor: BOOLEAN → 3.0; DECFLOAT/INT128 → 4.0.
func (c *classifier) detectTypeVersions() {
	for _, t := range c.toks {
		if t.kind != tkWord {
			continue
		}
		switch t.upper {
		case "BOOLEAN":
			c.raiseVersion("3.0", t.start)
		case "DECFLOAT", "INT128":
			c.raiseVersion("4.0", t.start)
		}
	}
}

func (c *classifier) functionRest() {
	c.finishNamed(ObjFunction)
	c.scanBody(1)
}

func (c *classifier) indexStmt() {
	if c.at(0, "UNIQUE") {
		c.s.Flags.IndexUnique = true
		c.i++
	}
	if c.at(0, "DESC") || c.at(0, "DESCENDING") {
		c.s.Flags.IndexDescending = true
		c.i++
	} else if c.at(0, "ASC") || c.at(0, "ASCENDING") {
		c.i++
	}
	if !c.at(0, "INDEX") {
		c.unknown("fbparse: expected INDEX")
		return
	}
	c.i++
	c.finishNamed(ObjIndex)
	if c.at(0, "ACTIVE") || c.at(0, "INACTIVE") {
		c.i++
	}
	if c.at(0, "ON") {
		c.i++
		if ref, _, ok := c.readName(); ok {
			c.s.Secondary = append(c.s.Secondary, ref)
		}
	}
	// Index shape analysis (grammar index_column_expr: column list,
	// parenthesized column list, or COMPUTED BY expression).
	expr := c.at(0, "COMPUTED")
	desc := c.s.Flags.IndexDescending
	base := -1
	if c.sym(0) == "(" {
		base = c.i
	}
	if base >= 0 {
		inner := c.depth[base] + 1
		for k := base + 1; k < len(c.toks); k++ {
			if c.depth[k] < inner && c.symAt(k) == ")" {
				break // closing paren of the column list
			}
			t := c.toks[k]
			switch {
			case c.depth[k] > inner:
				expr = true // nested parens: function/expression index
			case t.kind == tkSymbol && c.text(&t) != ",":
				expr = true // operator at column level
			case t.kind == tkWord && (t.upper == "DESC" || t.upper == "DESCENDING"):
				desc = true
			case t.kind == tkWord && t.upper == "COMPUTED":
				expr = true
			}
		}
	}
	c.s.Flags.IndexDescending = desc

	// Partial index: depth-0 WHERE after the columns (parse.y
	// index_condition_opt).
	partial := false
	for k := c.i; k < len(c.toks); k++ {
		if c.depth[k] == 0 && c.toks[k].isWord("WHERE") {
			partial = true
			break
		}
	}
	switch {
	case expr:
		c.s.Flags.IndexExpression = true
		c.s.variant = varIndexExpression
	case partial:
		c.s.variant = varIndexPartial
	case c.s.Flags.IndexUnique && desc:
		c.s.variant = varIndexUniqueDesc
	case c.s.Flags.IndexUnique:
		c.s.variant = varIndexUnique
	case desc:
		c.s.variant = varIndexDesc
	}
	if partial {
		c.raiseVersion("5.0", c.toks[0].start)
	}
}

// triggerRest parses the trigger tail after the name (FR-5 sub-kind,
// best-effort) and scans the body.
func (c *classifier) triggerRest() {
	c.finishNamed(ObjTrigger)
	for c.at(0, "ACTIVE") || c.at(0, "INACTIVE") {
		c.i++
	}
	switch {
	case c.at(0, "FOR"):
		c.i++
		if ref, _, ok := c.readName(); ok {
			c.s.Secondary = append(c.s.Secondary, ref)
		}
		c.s.Flags.TriggerKind = "DML"
	case c.at(0, "ON"):
		c.i++
		c.s.Flags.TriggerKind = "DATABASE_EVENT"
	case c.at(0, "BEFORE"), c.at(0, "AFTER"):
		ev := c.tok(1)
		if ev != nil && ev.kind == tkWord {
			switch ev.upper {
			case "INSERT", "UPDATE", "DELETE":
				c.s.Flags.TriggerKind = "DML"
			default:
				c.s.Flags.TriggerKind = "DDL"
			}
		}
		// Table follows the event list's ON.
		for k := c.i; k < len(c.toks); k++ {
			if c.depth[k] != 0 {
				continue
			}
			if c.toks[k].isWord("ON") && k+1 < len(c.toks) {
				save := c.i
				c.i = k + 1
				if ref, _, ok := c.readName(); ok {
					c.s.Secondary = append(c.s.Secondary, ref)
				}
				c.i = save
				break
			}
		}
	default:
		// ALTER TRIGGER without type info: sub-kind stays "" (open q4).
	}
	c.scanBody(1)
}

// ---------------------------------------------------------------------------
// DROP (FR-4, FR-5, FR-11)

func (c *classifier) dropStmt() {
	c.s.Verb = VerbDrop
	c.i = 1
	c.s.Mutating = true
	c.s.Reversibility = ReversibilityReverseDDL

	// dropNamed skips the trailing IF EXISTS noise (grammar:
	// DROP <object> if_exists_opt <name>) and reads the name.
	dropNamed := func(ot ObjectType) {
		if c.at(0, "IF") && c.at(1, "EXISTS") {
			c.s.Flags.setExtra("if_exists", "1")
			c.i += 2
		}
		c.finishNamed(ot)
	}

	switch {
	case c.at(0, "DATABASE"):
		c.i++
		c.s.ObjectType = ObjDatabase                  // DROP DATABASE takes no name
		c.s.Reversibility = ReversibilityRestorePoint // FR-11
	case c.at(0, "TABLE"):
		c.i++
		dropNamed(ObjTable)
	case c.at(0, "VIEW"):
		c.i++
		dropNamed(ObjView)
	case c.at(0, "INDEX"):
		c.i++
		dropNamed(ObjIndex)
	case c.at(0, "PROCEDURE"):
		c.i++
		dropNamed(ObjProcedure)
	case c.at(0, "EXTERNAL") && c.at(1, "FUNCTION"):
		c.i += 2
		c.finishNamed(ObjFunction)
		c.s.variant = varFunctionExternal
	case c.at(0, "FUNCTION"):
		c.i++
		dropNamed(ObjFunction)
	case c.at(0, "TRIGGER"):
		c.i++
		dropNamed(ObjTrigger)
	case c.at(0, "DOMAIN"):
		c.i++
		dropNamed(ObjDomain)
	case c.at(0, "EXCEPTION"):
		c.i++
		dropNamed(ObjException)
	case c.at(0, "GENERATOR"), c.at(0, "SEQUENCE"):
		c.i++
		dropNamed(ObjSequence)
	case c.at(0, "ROLE"):
		c.i++
		dropNamed(ObjRole)
	case c.at(0, "USER"):
		c.i++
		dropNamed(ObjUser)
	case c.at(0, "PACKAGE"):
		c.i++
		if c.at(0, "BODY") {
			c.i++
			c.s.variant = varPackageBody
		}
		dropNamed(ObjPackage)
		c.raiseVersion("3.0", c.toks[0].start)
	case c.at(0, "FILTER"):
		c.i++
		dropNamed(ObjFilter)
	case c.at(0, "SHADOW"):
		c.i++
		dropNamed(ObjShadow)
	case c.at(0, "MAPPING") || (c.at(0, "GLOBAL") && c.at(1, "MAPPING")):
		if c.at(0, "GLOBAL") {
			c.s.Flags.setExtra("global", "1")
			c.i++
		}
		c.i++
		dropNamed(ObjMapping)
	case c.at(0, "COLLATION"):
		c.unknown("fbparse: COLLATION statements are outside the object vocabulary")
	case c.at(0, "CHARACTER") && c.at(1, "SET"):
		c.unknown("fbparse: CHARACTER SET statements are outside the object vocabulary")
	case c.at(0, "SCHEMA"):
		c.unknown("fbparse: SCHEMA statements are outside the supported vocabulary")
	default:
		c.unknown("fbparse: unrecognized DROP object")
	}
}

// ---------------------------------------------------------------------------
// ALTER (FR-5, FR-6, FR-12)

func (c *classifier) alterStmt() {
	c.s.Verb = VerbAlter
	c.i = 1
	c.s.Mutating = true
	c.s.Reversibility = ReversibilityReverseDDL

	switch {
	case c.at(0, "TABLE"):
		c.i++
		c.finishNamed(ObjTable)
		c.alterTableOps()
	case c.at(0, "VIEW"):
		c.i++
		c.finishNamed(ObjView)
	case c.at(0, "INDEX"):
		c.i++
		c.finishNamed(ObjIndex)
		switch {
		case c.at(0, "ACTIVE"):
			c.s.Flags.IndexActivation = "ACTIVE"
			c.s.variant = varIndexActive
		case c.at(0, "INACTIVE"):
			c.s.Flags.IndexActivation = "INACTIVE"
			c.s.variant = varIndexInactive
		default:
			c.s.addIssue(IssueUnsupportedConstruct, "fbparse: expected ACTIVE or INACTIVE", c.i)
		}
	case c.at(0, "DATABASE"):
		c.i++
		c.s.ObjectType = ObjDatabase // ALTER DATABASE takes no name
		c.s.Reversibility = ReversibilityRestorePoint
		c.alterDatabaseOps()
	case c.at(0, "PROCEDURE"):
		c.i++
		c.finishNamed(ObjProcedure)
		c.scanBody(1)
	case c.at(0, "FUNCTION"):
		c.i++
		c.finishNamed(ObjFunction)
		c.scanBody(1)
	case c.at(0, "AGGREGATE") && c.at(1, "FUNCTION"):
		c.i += 2
		c.finishNamed(ObjFunction)
		c.s.variant = varFunctionAggr
	case c.at(0, "EXTERNAL") && c.at(1, "FUNCTION"):
		c.i += 2
		c.finishNamed(ObjFunction)
		c.s.variant = varFunctionExternal
	case c.at(0, "TRIGGER"):
		c.i++
		c.triggerRest()
	case c.at(0, "PACKAGE"):
		c.i++
		if c.at(0, "BODY") {
			c.i++
			c.s.variant = varPackageBody
		}
		c.finishNamed(ObjPackage)
		c.raiseVersion("3.0", c.toks[0].start)
		c.scanBody(1)
	case c.at(0, "DOMAIN"):
		c.i++
		c.finishNamed(ObjDomain)
		c.alterDomainOps()
	case c.at(0, "GENERATOR"), c.at(0, "SEQUENCE"):
		c.i++
		c.finishNamed(ObjSequence)
		c.s.Reversibility = ReversibilityRestorePoint
	case c.at(0, "USER"):
		c.i++
		c.finishNamed(ObjUser)
	case c.at(0, "CURRENT") && c.at(1, "USER"):
		c.i += 2
		c.s.ObjectType = ObjUser
		c.s.variant = varCurrentUser
	case c.at(0, "ROLE"):
		c.i++
		c.finishNamed(ObjRole)
	case c.at(0, "EXCEPTION"):
		c.i++
		c.finishNamed(ObjException)
	case c.at(0, "MAPPING") || (c.at(0, "GLOBAL") && c.at(1, "MAPPING")):
		if c.at(0, "GLOBAL") {
			c.s.Flags.setExtra("global", "1")
			c.i++
		}
		c.i++
		c.finishNamed(ObjMapping)
	case c.at(0, "CHARACTER") && c.at(1, "SET"):
		c.unknown("fbparse: CHARACTER SET statements are outside the object vocabulary")
	case c.at(0, "EXTERNAL") && c.at(1, "CONNECTIONS") && c.at(2, "POOL"):
		c.unknown("fbparse: EXTERNAL CONNECTIONS POOL is outside the supported vocabulary")
	case c.at(0, "SCHEMA"):
		c.unknown("fbparse: SCHEMA statements are outside the supported vocabulary")
	default:
		c.unknown("fbparse: unrecognized ALTER object")
	}
}

// alterTableOps walks depth-0 comma-separated operations (parse.y
// alter_op) extracting column-mutation and constraint flags (FR-6).
func (c *classifier) alterTableOps() {
	colSet := false
	constraintSet := false
	for c.i < len(c.toks) {
		if c.depth[c.i] != 0 {
			c.i++
			continue
		}
		t := c.toks[c.i]
		if t.kind != tkWord {
			c.i++
			continue
		}
		switch t.upper {
		case "ADD":
			c.i++
			named := false
			if c.at(0, "CONSTRAINT") {
				c.i++
				named = true
				c.readName() // constraint name (noise)
			}
			if c.at(0, "PRIMARY") && c.at(1, "KEY") {
				c.setConstraint("PK", varConstraintPK, &constraintSet)
				c.i += 2
			} else if c.at(0, "UNIQUE") {
				c.setConstraint("UNIQUE", varConstraintUniq, &constraintSet)
				c.i++
			} else if c.at(0, "FOREIGN") && c.at(1, "KEY") {
				c.setConstraint("FK", varConstraintFK, &constraintSet)
				c.i += 2
				c.fkReferences()
			} else if c.at(0, "CHECK") {
				c.setConstraint("CHECK", varConstraintCheck, &constraintSet)
				c.i++
			} else {
				// ADD [COLUMN] <name> <type>
				if c.at(0, "COLUMN") {
					c.i++
				}
				if !named {
					c.columnOp(varColumnAdd, "", &colSet)
				}
			}
		case "DROP":
			c.i++
			if c.at(0, "CONSTRAINT") {
				c.i++
				c.readName()
				if !constraintSet {
					c.s.variant = varConstraintDrop
					constraintSet = true
				}
			} else {
				if c.at(0, "COLUMN") {
					c.i++
				}
				if c.at(0, "IF") && c.at(1, "EXISTS") {
					c.i += 2
				}
				c.columnOp(varColumnDrop, "", &colSet)
			}
		case "ALTER":
			c.i++
			if c.at(0, "SQL") && c.at(1, "SECURITY") {
				// ALTER SQL SECURITY DEFINER|INVOKER — relation-level, no column.
				c.i += 3
				continue
			}
			if c.at(0, "COLUMN") {
				c.i++
			}
			c.markColumn()
			switch {
			case c.at(0, "TYPE"):
				c.i++
				c.setOp(varColumnType, "TYPE", &colSet)
				c.s.Reversibility = ReversibilityRestorePoint // FR-11
				c.detectTypeVersions()
			case c.at(0, "SET") && c.at(1, "NOT") && c.at(2, "NULL"):
				c.i += 3
				c.setOp(varColumnNull, "NOT_NULL", &colSet)
			case c.at(0, "DROP") && c.at(1, "NOT") && c.at(2, "NULL"):
				c.i += 3
				c.setOp(varColumnNull, "NOT_NULL", &colSet)
			case c.at(0, "SET") && c.at(1, "DEFAULT"):
				c.i += 2
				c.setOp(varColumnDef, "DEFAULT", &colSet)
			case c.at(0, "DROP") && c.at(1, "DEFAULT"):
				c.i += 2
				c.setOp(varColumnDef, "DEFAULT", &colSet)
			case c.at(0, "TO"):
				c.i += 2 // TO <new name>
				c.setOp(varColumnRename, "RENAME", &colSet)
			case c.at(0, "POSITION"):
				c.i += 2
				c.setOp("", "", &colSet)
				c.s.Flags.setExtra("position", "1")
			default:
				// Remaining forms (computed columns, identity options):
				// column captured, no specific mutation claimed.
				c.setOp("", "", &colSet)
			}
		default:
			c.i++
		}
	}
}

// markColumn reads the column name at the cursor into Column (once).
func (c *classifier) markColumn() {
	if c.s.Column != nil {
		return
	}
	if ref, _, ok := c.readName(); ok {
		c.s.Column = &ref
	}
}

// setOp records, for the first ALTER TABLE operation seen, the OpKey
// variant and ColumnMutation flag. Column ops are data-touching (FR-11).
func (c *classifier) setOp(variant, mutation string, seen *bool) {
	if !*seen {
		*seen = true
		c.s.variant = variant
		c.s.Flags.ColumnMutation = mutation
	}
	c.s.Reversibility = ReversibilityRestorePoint
}

// columnOp = markColumn + setOp for ADD/DROP column forms.
func (c *classifier) columnOp(variant, mutation string, seen *bool) {
	c.markColumn()
	c.setOp(variant, mutation, seen)
}

func (c *classifier) setConstraint(kind, variant string, seen *bool) {
	if !*seen {
		*seen = true
		c.s.variant = variant
		c.s.Flags.ConstraintKind = kind
	}
}

// fkReferences captures FOREIGN KEY ... REFERENCES <table> into Secondary.
func (c *classifier) fkReferences() {
	for k := c.i; k < len(c.toks); k++ {
		if c.toks[k].isWord("REFERENCES") {
			save := c.i
			c.i = k + 1
			if ref, _, ok := c.readName(); ok {
				c.s.Secondary = append(c.s.Secondary, ref)
			}
			c.i = save
			return
		}
	}
}

func (c *classifier) alterDatabaseOps() {
	for k := c.i; k < len(c.toks); k++ {
		if c.toks[k].kind != tkWord {
			continue
		}
		switch c.toks[k].upper {
		case "LINGER":
			c.raiseVersion("4.0", c.toks[k].start)
		case "ENCRYPT", "DECRYPT":
			c.s.Flags.setExtra("crypt", "1")
		case "PUBLICATION":
			c.s.Flags.setExtra("publication", "1")
		}
	}
}

func (c *classifier) alterDomainOps() {
	for k := c.i; k < len(c.toks); k++ {
		if c.depth[k] != 0 || c.toks[k].kind != tkWord {
			continue
		}
		switch c.toks[k].upper {
		case "TYPE":
			c.s.Flags.setExtra("domain_op", "TYPE")
			c.s.Reversibility = ReversibilityRestorePoint
			c.detectTypeVersions()
		case "TO":
			c.s.Flags.setExtra("domain_op", "RENAME")
		}
	}
}
