package fbparse

// rawStmt is one split result before classification.
type rawStmt struct {
	span   Span
	term   string // active terminator when this statement was split
	issues []Issue
	// setTerm, when non-empty, is the new terminator requested by a
	// SET TERM directive statement (isql emulation, FR-1).
	setTerm string
}

// split walks the token stream once, emitting statement spans.
//
// Spans exclude the terminator and surrounding whitespace; bytes between
// statements are terminators and whitespace only, so splitting is lossless
// (§7 property).
//
// PSQL-bearing heads (CREATE/ALTER/RECREATE/CREATE OR ALTER over
// PROCEDURE|FUNCTION|TRIGGER|PACKAGE[BODY], EXECUTE BLOCK) suppress
// semicolons inside BEGIN..END (FR-2). Inside such statements, a depth-0
// ';' still does not terminate while the current segment opened with
// DECLARE (parse.y local_declarations_opt: "DECLARE var ...;" separators
// between AS and BEGIN). External-body forms (no BEGIN at all) terminate
// at the first depth-0 ';' like ordinary statements.
//
// CASE..END inside expressions would unbalance naive BEGIN/END counting,
// so a marker stack tracks both (P6.1 adversarial: nested BEGIN..END,
// CASE inside bodies).
func split(input string, cfg *config) []rawStmt {
	var out []rawStmt
	lx := newLexer(input, cfg)

	term := cfg.term
	start := -1   // offset of first significant token of current statement
	lastEnd := -1 // end of last significant non-terminator token
	var firstWords []string
	var head string // "" or psql head kind
	var markers []byte
	bodyComplete := false
	segDeclares := false
	inSetTerm := false
	var setTermOps []token

	reset := func() {
		start, lastEnd = -1, -1
		firstWords = firstWords[:0]
		head = ""
		markers = markers[:0]
		bodyComplete = false
		segDeclares = false
		inSetTerm = false
		setTermOps = setTermOps[:0]
	}

	emit := func() {
		if start < 0 || lastEnd <= start {
			// Nothing significant accumulated (e.g. stray terminator):
			// no phantom statement (adversarial: empty/whitespace input).
			reset()
			return
		}
		rs := rawStmt{span: Span{Start: start, End: lastEnd}, term: term, issues: lx.issues}
		lx.issues = nil // handed off to this statement
		if inSetTerm && len(setTermOps) > 0 {
			rs.setTerm = setTermOps[0].text(input)
		}
		out = append(out, rs)
		if rs.setTerm != "" {
			term = rs.setTerm
		}
		reset()
	}

	for {
		t := lx.next()
		if t.kind == tkEOF {
			emit()
			if len(lx.issues) > 0 {
				if len(out) > 0 {
					// Lexical issues with no significant tokens left
					// (e.g. unterminated comment after the last
					// statement) attach to the last statement.
					out[len(out)-1].issues = append(out[len(out)-1].issues, lx.issues...)
				} else if len(out) == 0 {
					// Whole input is lexical garbage (e.g. a lone
					// unterminated comment): still emit one Unknown
					// statement so the oddity is never silent (FR-3).
					out = append(out, rawStmt{span: Span{Start: 0, End: len(input)}, term: term, issues: lx.issues})
				}
			}
			break
		}

		if start < 0 {
			start = t.start
		}

		isTerm := isTerminator(t, term, input)

		if t.kind == tkWord {
			if len(firstWords) < 6 {
				firstWords = append(firstWords, t.upper)
			}
			if head == "" && len(firstWords) >= 2 {
				head = matchPSQLHead(firstWords)
			}
			if len(firstWords) == 2 && firstWords[0] == "SET" && firstWords[1] == "TERM" {
				inSetTerm = true
			}
		}

		// Capture SET TERM operands (the new terminator, optionally the
		// old one) before terminator handling so the closing terminator
		// itself is never mistaken for an operand.
		if inSetTerm && !isTerm && len(setTermOps) < 2 {
			setTermOps = append(setTermOps, t)
		}

		if !isTerm {
			lastEnd = t.end
		}

		if head != "" {
			switch {
			case t.isWord("BEGIN"):
				markers = append(markers, 'b')
			case t.isWord("CASE"):
				markers = append(markers, 'c')
			case t.isWord("END"):
				if n := len(markers); n > 0 {
					if markers[n-1] == 'b' && n == 1 {
						bodyComplete = true
					}
					markers = markers[:n-1]
				}
			case t.isWord("DECLARE"):
				if len(markers) == 0 && !bodyComplete {
					segDeclares = true
				}
			}
		}

		if isTerm {
			if head != "" {
				switch {
				case len(markers) > 0:
					// Semicolon inside BEGIN..END — part of the body.
					continue
				case segDeclares:
					// Local declaration separator (DECLARE ... ;).
					segDeclares = false
					continue
				default:
					// After a completed body, or in an external-body form
					// (no BEGIN at all), a depth-0 ';' terminates.
				}
			}
			emit()
			continue
		}
	}
	return out
}

// isTerminator reports whether t closes a statement under the active term.
// Symbolic terms match exactly; alphabetic terms match case-insensitively
// (isql-compatible).
func isTerminator(t token, term, input string) bool {
	if term == "" || t.kind == tkString || t.kind == tkQIdent || t.kind == tkHexStr {
		return false
	}
	if t.text(input) == term {
		return true
	}
	if t.kind == tkWord && isAllAlpha(term) && asciiUpper(term) == t.upper {
		return true
	}
	return false
}

func isAllAlpha(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && c != '_' {
			return false
		}
	}
	return true
}

// matchPSQLHead recognizes statement heads whose grammar carries a PSQL
// body ("" when not PSQL-bearing). CREATE AGGREGATE FUNCTION and other
// external-only forms are excluded: they never contain BEGIN..END, so
// plain terminator handling is correct.
func matchPSQLHead(w []string) string {
	at := func(i int, word string) bool { return i < len(w) && w[i] == word }
	switch {
	case at(0, "EXECUTE") && at(1, "BLOCK"):
		return "BLOCK"
	case at(0, "CREATE") && at(1, "OR") && at(2, "ALTER"):
		return psqlHeadAfterPrefix(w, 3)
	case at(0, "CREATE"), at(0, "ALTER"), at(0, "RECREATE"):
		return psqlHeadAfterPrefix(w, 1)
	}
	return ""
}

func psqlHeadAfterPrefix(w []string, i int) string {
	at := func(k int, word string) bool { return i+k < len(w) && w[i+k] == word }
	switch {
	case at(0, "PROCEDURE"):
		return "PROCEDURE"
	case at(0, "AGGREGATE"):
		return "" // CREATE AGGREGATE FUNCTION: external, no body
	case at(0, "FUNCTION"):
		return "FUNCTION"
	case at(0, "TRIGGER"):
		return "TRIGGER"
	case at(0, "PACKAGE"):
		if at(1, "BODY") {
			return "PACKAGE BODY"
		}
		return "PACKAGE"
	}
	return ""
}
