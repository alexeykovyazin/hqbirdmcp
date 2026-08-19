package fbparse

import "strings"

// ---------------------------------------------------------------------------
// GRANT / REVOKE (FR-4, FR-6; grammar: parse.y grant0/revoke0)

func (c *classifier) dclStmt(grant bool) {
	if grant {
		c.s.Verb = VerbGrant
	} else {
		c.s.Verb = VerbRevoke
	}
	c.s.Mutating = true
	c.s.Reversibility = ReversibilityReverseDDL
	c.i = 1

	if !grant {
		switch {
		case c.at(0, "GRANT") && c.at(1, "OPTION") && c.at(2, "FOR"):
			c.s.Flags.GrantOption = true
			c.s.variant = varRevokeGrantOption
			c.i += 3
		case c.at(0, "ADMIN") && c.at(1, "OPTION") && c.at(2, "FOR"):
			c.s.Flags.AdminOption = true
			c.s.variant = varRevokeAdminOption
			c.i += 3
		case c.at(0, "ALL"):
			// REVOKE ALL ... is treated as a plain privilege list.
		}
	}

	// Phase 1: privileges (or role names) up to the depth-0 ON and the
	// grantee keyword (TO for GRANT, FROM for REVOKE — parse.y
	// non_role_grantee_list / role_grantee_list).
	granteeWord := "TO"
	if !grant {
		granteeWord = "FROM"
	}
	onIdx, granteeIdx := -1, -1
	for k := c.i; k < len(c.toks); k++ {
		if c.depth[k] != 0 {
			continue
		}
		t := c.toks[k]
		if t.kind != tkWord {
			continue
		}
		if t.upper == "ON" && onIdx < 0 {
			onIdx = k
		} else if t.upper == granteeWord && granteeIdx < 0 {
			granteeIdx = k
			break
		}
	}
	if granteeIdx < 0 {
		c.unknown("fbparse: GRANT/REVOKE without " + granteeWord)
		return
	}

	if onIdx < 0 || onIdx > granteeIdx {
		// Role grant/revoke: GRANT role TO grantee / REVOKE role FROM grantee.
		c.roleGrant(granteeIdx)
		return
	}

	// Privileges as written (FR-6 / open q5: kept as written).
	c.collectPrivileges(c.i, onIdx)

	// Object after ON.
	c.i = onIdx + 1
	sysPriv := c.parseDCLObject()

	// Grantees.
	c.i = granteeIdx + 1
	c.parseGrantees()

	// Trailing options.
	c.parseDCLOptions()

	// System-privilege vocabulary (USAGE, ALTER/DROP ANY, class DDL
	// grants) is Firebird 3.0+ (FR-12).
	if sysPriv || c.s.variant == varDCLClass {
		c.raiseVersion("3.0", c.toks[onIdx].start)
	}
}

// roleGrant handles GRANT role TO grantee [WITH ADMIN OPTION] and the
// REVOKE ... FROM mirror.
func (c *classifier) roleGrant(granteeIdx int) {
	c.s.ObjectType = ObjRole
	if ref, _, ok := c.readName(); ok {
		c.s.Object = ref
	} else {
		c.unknown("fbparse: expected role name")
		return
	}
	c.i = granteeIdx + 1
	c.parseGrantees()
	c.parseDCLOptions()
}

// collectPrivileges records the privilege list as written (FR-6).
func (c *classifier) collectPrivileges(from, to int) {
	for k := from; k < to; k++ {
		t := c.toks[k]
		if t.kind != tkWord {
			continue
		}
		switch t.upper {
		case "ALL":
			// ALL [PRIVILEGES]
			c.s.Privileges = append(c.s.Privileges, "ALL")
			if k+1 < to && c.toks[k+1].isWord("PRIVILEGES") {
				k++
			}
		case "SELECT", "INSERT", "DELETE", "EXECUTE", "USAGE":
			c.s.Privileges = append(c.s.Privileges, t.upper)
		case "UPDATE", "REFERENCES":
			// UPDATE (col, ...) — column list dropped (best-effort).
			c.s.Privileges = append(c.s.Privileges, t.upper)
			if k+1 < to && c.symAt(k+1) == "(" {
				depth := c.depth[k+1]
				k++
				for k < to && c.depth[k] > depth {
					k++
				}
			}
		case "ALTER", "DROP":
			// DDL privileges: ALTER ANY / DROP ANY (system privileges).
			name := t.upper
			if k+1 < to && c.toks[k+1].isWord("ANY") {
				name += " ANY"
				k++
			}
			c.s.Privileges = append(c.s.Privileges, name)
		case "CREATE":
			c.s.Privileges = append(c.s.Privileges, "CREATE")
		}
	}
}

// parseDCLObject consumes the ON <object> clause; reports whether system
// privileges (3.0+ vocabulary) were involved.
func (c *classifier) parseDCLObject() bool {
	sysPriv := false
	for _, p := range c.s.Privileges {
		if p == "USAGE" || p == "ALTER ANY" || p == "DROP ANY" {
			sysPriv = true
		}
	}

	kindNoise := map[string]ObjectType{
		"TABLE": ObjTable, "PROCEDURE": ObjProcedure, "FUNCTION": ObjFunction,
		"PACKAGE": ObjPackage, "GENERATOR": ObjSequence, "SEQUENCE": ObjSequence,
		"EXCEPTION": ObjException, "DOMAIN": ObjDomain,
	}

	switch {
	case c.at(0, "CHARACTER") && c.at(1, "SET"):
		c.s.ObjectType = ObjUnknown
		c.unknown("fbparse: CHARACTER SET grants are outside the object vocabulary")
		return sysPriv
	case c.at(0, "COLLATION"):
		c.s.ObjectType = ObjUnknown
		c.unknown("fbparse: COLLATION grants are outside the object vocabulary")
		return sysPriv
	case c.at(0, "SCHEMA"):
		c.s.ObjectType = ObjUnknown
		c.unknown("fbparse: SCHEMA grants are outside the supported vocabulary")
		return sysPriv
	case c.at(0, "DATABASE"):
		c.i++
		c.s.ObjectType = ObjDatabase
		c.s.variant = varDCLClass
		return sysPriv
	case c.at(0, "ROLE"), c.at(0, "FILTER"):
		ot := ObjRole
		if c.at(0, "FILTER") {
			ot = ObjFilter
		}
		c.i++
		c.s.ObjectType = ot
		c.s.variant = varDCLClass
		return sysPriv
	case c.at(0, "VIEW"):
		// Class grant ON VIEW (no name follows).
		c.i++
		c.s.ObjectType = ObjView
		c.s.variant = varDCLClass
		return sysPriv
	}

	if t := c.tok(0); t != nil && t.kind == tkWord {
		if ot, ok := kindNoise[t.upper]; ok {
			c.i++
			// ON [TABLE] <name>: named object, or class grant when no
			// name follows (the next token is the grantee keyword or a
			// trailing option).
			if nt := c.tok(0); nt != nil && (nt.kind == tkQIdent ||
				(nt.kind == tkWord && !nt.isWord("TO") && !nt.isWord("FROM") &&
					!nt.isWord("WITH") && !nt.isWord("GRANTED") && !nt.isWord("AS"))) {
				c.setTarget(ot)
			} else {
				c.s.ObjectType = ot
				c.s.variant = varDCLClass
			}
			return sysPriv
		}
	}

	// ON <name> without object keyword (table_noise).
	c.setTarget(ObjTable)
	return sysPriv
}

// parseGrantees records the first TO/FROM grantee and stashes the rest.
func (c *classifier) parseGrantees() {
	var names []string
	expectName := true
done:
	for c.i < len(c.toks) {
		t := c.tok(0)
		if t.kind == tkWord {
			switch t.upper {
			case "WITH", "GRANTED", "AS":
				break done
			case "USER", "GROUP":
				c.i++
				continue
			case "PROCEDURE", "FUNCTION", "PACKAGE", "TRIGGER", "VIEW":
				// Grantee of another object kind: skip keyword, keep name.
				if c.s.Flags.Extras["grantee_kind"] == "" {
					c.s.Flags.setExtra("grantee_kind", t.upper)
				}
				c.i++
				continue
			}
		}
		if expectName {
			if ref, _, ok := c.readName(); ok {
				names = append(names, ref.Name)
				if c.s.Grantee.Name == "" {
					c.s.Grantee = ref
				}
				expectName = false
				continue
			}
			break
		}
		if c.sym(0) == "," {
			c.i++
			expectName = true
			continue
		}
		break
	}
	if len(names) > 1 {
		c.s.Flags.setExtra("grantees", strings.Join(names[1:], ","))
	}
}

func (c *classifier) parseDCLOptions() {
	for c.i < len(c.toks) {
		t := c.tok(0)
		if t.kind == tkWord {
			switch t.upper {
			case "WITH":
				if c.at(1, "GRANT") && c.at(2, "OPTION") {
					c.s.Flags.GrantOption = true
					if c.s.variant == "" {
						c.s.variant = varGrantWithOption
					}
					c.i += 3
					continue
				}
				if c.at(1, "ADMIN") && c.at(2, "OPTION") {
					c.s.Flags.AdminOption = true
					if c.s.variant == "" {
						c.s.variant = varGrantAdminOption
					}
					c.i += 3
					continue
				}
			case "GRANTED":
				if c.at(1, "BY") {
					c.i += 2
					if ref, _, ok := c.readName(); ok {
						c.s.Flags.setExtra("grantor", ref.Name)
					}
					continue
				}
			case "AS":
				c.i++
				if ref, _, ok := c.readName(); ok {
					c.s.Flags.setExtra("grantor", ref.Name)
				}
				continue
			}
		}
		c.i++
	}
}

// ---------------------------------------------------------------------------
// COMMENT ON (FR-5; grammar: parse.y comment)

func (c *classifier) commentStmt() {
	c.s.Verb = VerbComment
	c.s.Mutating = true
	c.s.Reversibility = ReversibilityReverseDDL
	if !c.at(1, "ON") {
		c.unknown("fbparse: expected ON after COMMENT")
		return
	}
	c.i = 2

	simple := map[string]ObjectType{
		"DOMAIN": ObjDomain, "TABLE": ObjTable,
		"VIEW": ObjView, "TRIGGER": ObjTrigger, "EXCEPTION": ObjException,
		"GENERATOR": ObjSequence, "SEQUENCE": ObjSequence, "INDEX": ObjIndex,
		"PACKAGE": ObjPackage, "FILTER": ObjFilter, "ROLE": ObjRole,
		"PROCEDURE": ObjProcedure, "USER": ObjUser, "MAPPING": ObjMapping,
	}

	switch {
	case c.at(0, "DATABASE"):
		c.i++
		c.s.ObjectType = ObjDatabase // COMMENT ON DATABASE takes no name
	case c.at(0, "COLUMN"):
		c.i++
		c.containerAndColumn(ObjTable, varColumnSub)
	case c.at(0, "CHARACTER") && c.at(1, "SET"):
		c.unknown("fbparse: CHARACTER SET comments are outside the object vocabulary")
	case c.at(0, "COLLATION"):
		c.unknown("fbparse: COLLATION comments are outside the object vocabulary")
	case c.at(0, "SCHEMA"):
		c.unknown("fbparse: SCHEMA comments are outside the supported vocabulary")
	case c.at(0, "PARAMETER"):
		// PARAMETER p.x — procedure parameters (parse.y ddl_type3).
		c.i++
		c.containerAndColumn(ObjProcedure, varParameter)
	case c.at(0, "PROCEDURE") && c.at(1, "PARAMETER"):
		c.i += 2
		c.containerAndColumn(ObjProcedure, varParameter)
	case c.at(0, "FUNCTION") && c.at(1, "PARAMETER"):
		c.i += 2
		c.containerAndColumn(ObjFunction, varParameter)
	case c.at(0, "CONSTANT"):
		// Package constant: COMMENT ON CONSTANT pkg.c
		c.i++
		c.containerAndColumn(ObjPackage, varConstant)
	case c.at(0, "EXTERNAL") && c.at(1, "FUNCTION"):
		c.i += 2
		c.finishNamed(ObjFunction)
	default:
		if t := c.tok(0); t != nil && t.kind == tkWord {
			if ot, ok := simple[t.upper]; ok {
				c.i++
				c.finishNamed(ot)
				// COMMENT ON USER u USING PLUGIN p
				if ot == ObjUser && c.at(0, "USING") {
					c.i++
					c.readName()
				}
				return
			}
		}
		c.unknown("fbparse: unrecognized COMMENT ON object")
	}
}

// containerAndColumn reads a container '.' column reference. readName is
// greedy across dots, so the composite is split at the last separator —
// the grammar guarantees exactly container.column here.
func (c *classifier) containerAndColumn(ot ObjectType, variant string) {
	c.s.ObjectType = ot
	c.s.variant = variant
	ref, _, ok := c.readName()
	if !ok {
		c.s.addIssue(IssueUnsupportedConstruct, "fbparse: expected object name", c.i)
		return
	}
	if idx := strings.LastIndex(ref.Name, "."); idx > 0 {
		col := ObjectRef{Name: ref.Name[idx+1:]}
		ref.Name = ref.Name[:idx]
		c.s.Object = ref
		c.s.Column = &col
		return
	}
	c.s.Object = ref
	c.s.addIssue(IssueAmbiguousParse, "fbparse: expected container.column reference", c.i)
}

// ---------------------------------------------------------------------------
// DECLARE (FR-4: external functions and filters)

func (c *classifier) declareStmt() {
	c.s.Verb = VerbDeclare
	c.s.Mutating = true
	c.s.Reversibility = ReversibilityReverseDDL
	c.i = 1
	switch {
	case c.at(0, "FILTER"):
		c.i++
		c.finishNamed(ObjFilter)
	case c.at(0, "EXTERNAL") && c.at(1, "FUNCTION"):
		c.i += 2
		c.finishNamed(ObjFunction)
		c.s.variant = varFunctionExternal
	case c.at(0, "TABLE"):
		c.unknown("fbparse: DECLARE TABLE is embedded-SQL only, not a Firebird statement")
	default:
		c.unknown("fbparse: expected FILTER or EXTERNAL FUNCTION after DECLARE")
	}
}

// ---------------------------------------------------------------------------
// SET (FR-4, FR-16; grammar: parse.y set_statistics / set_generator_clause
// / set_transaction / session statements). Session/transaction SETs carry
// a Variant and no object — the server-side mapping decides their row.

func (c *classifier) setStmt() {
	c.s.Verb = VerbSet
	c.i = 1

	switch {
	case c.at(0, "GENERATOR"):
		c.i++
		c.finishNamed(ObjSequence)
		c.s.variant = varSetGenerator
		c.s.Mutating = true
		c.s.Reversibility = ReversibilityRestorePoint
	case c.at(0, "STATISTICS"):
		c.i++
		if c.at(0, "INDEX") {
			c.i++
		}
		c.finishNamed(ObjIndex)
		c.s.variant = varSetStatistics
		c.s.Mutating = true
		c.s.Reversibility = ReversibilityReverseDDL
	case c.at(0, "TRANSACTION"):
		c.s.variant = varSetTransaction
		c.s.Mutating = false
		c.s.Reversibility = ReversibilityNone
	case c.at(0, "TERM"):
		c.s.variant = varSetTerm
		c.s.Mutating = false
		c.s.Reversibility = ReversibilityNone
		if c.tok(0) != nil && c.tok(1) != nil {
			c.s.Flags.setExtra("new_term", c.text(c.tok(1)))
		}
	case c.at(0, "SESSION"), c.at(0, "ROLE"), c.at(0, "TRUSTED"),
		c.at(0, "DECFLOAT"), c.at(0, "BIND"), c.at(0, "OPTIMIZE"),
		c.at(0, "SEARCH"), c.at(0, "TIME"), c.at(0, "DEBUG"):
		c.s.variant = varSetSession
		c.s.Mutating = false
		c.s.Reversibility = ReversibilityNone
		if c.at(0, "ROLE") {
			if ref, _, ok := c.readName(); ok {
				c.s.Flags.setExtra("role", ref.Name)
			}
		}
	default:
		c.unknown("fbparse: unrecognized SET statement")
	}
}

// ---------------------------------------------------------------------------
// Dialect-1 signals (FR-3, NFR-7)
//
// In dialect 3 a double-quoted token is a delimited identifier. Some
// positions are only legal for string values, so a qident there is a
// strong signal of dialect-1 quoting. Signals are deliberately narrow:
// comparing two identifiers ("A" = "B") is valid dialect-3 SQL and must
// not trigger.

// dialect1Signal scans for double-quote usage that only makes sense as
// dialect-1 string quoting. Strong signals make the whole statement
// untrustworthy (Unknown per FR-3); weak signals only flag ambiguity —
// the verb/target extraction is robust either way.
func dialect1Signal(toks []token, in string) (off int, strong, any bool) {
	weakOps := map[string]bool{
		"=": true, "<>": true, "!=": true, "<": true, ">": true,
		"<=": true, ">=": true, "^=": true, "~=": true,
	}
	weakPreds := map[string]bool{
		"LIKE": true, "CONTAINING": true, "STARTING": true, "SIMILAR": true,
	}
	for i, t := range toks {
		if t.kind != tkQIdent {
			continue
		}
		// (a) Empty delimited identifier is not legal dialect 3.
		if t.end-t.start <= 2 {
			return t.start, true, true
		}
		// (c) qident as an argument of a VALUES(...) list — INSERT VALUES
		// cannot reference table columns, so only a literal fits.
		if i > 0 && toks[i-1].kind == tkWord && toks[i-1].upper == "VALUES" {
			return t.start, true, true
		}
		if i > 0 && toks[i-1].kind == tkSymbol && (toks[i-1].text(in) == "," || toks[i-1].text(in) == "(") {
			// Walk back over the argument list (paren-balance aware) to
			// its opening paren and check the list head word.
			bal := 0
		walk:
			for j := i - 1; j >= 0; j-- {
				if toks[j].kind == tkSymbol {
					switch txt := toks[j].text(in); {
					case txt == ")":
						bal++
					case txt == "(":
						if bal > 0 {
							bal--
							continue
						}
						if j > 0 && toks[j-1].kind == tkWord && toks[j-1].upper == "VALUES" {
							return t.start, true, true
						}
						break walk
					case txt == "," && bal == 0:
						// argument separator
					default:
						break walk
					}
				}
			}
		}
		// (d) Weak: qident in a value position (comparison operand,
		// string predicate, concatenation operand). Legal dialect 3 too
		// (column reference), hence flagged, not fatal.
		if i > 0 {
			prev := toks[i-1]
			if prev.kind == tkSymbol && (weakOps[prev.text(in)] || prev.text(in) == "||") {
				return t.start, false, true
			}
			if prev.kind == tkWord && weakPreds[prev.upper] {
				return t.start, false, true
			}
		}
	}
	return 0, false, false
}

// firstDQuoteString finds the first double-quoted string token produced in
// dialect-1 mode (lexer returns them as tkString with d1 flag bits — we
// re-detect by scanning the raw text).
func firstDQuoteString(toks []token, in string) (int, bool) {
	for _, t := range toks {
		if t.start < len(in) && in[t.start] == '"' && t.kind == tkString {
			return t.start, true
		}
	}
	return 0, false
}
