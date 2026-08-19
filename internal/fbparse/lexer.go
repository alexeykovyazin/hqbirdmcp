package fbparse

import "strings"

// Token kinds produced by the lexer. The split mirrors the engine's
// yylex()/yylexAux() (src/dsql/Parser.cpp): strings concatenate adjacent
// segments across whitespace and comments, quoted identifiers use doubled
// quotes as escapes, and x'..' is a hex string constant.
type tokKind uint8

const (
	tkEOF    tokKind = iota
	tkWord           // bare identifier / keyword (ASCII letters, digits, _, $; non-ASCII runs kept whole)
	tkNumber         // numeric literal (incl. 0x hex integers)
	tkString         // '...' with '' escapes; adjacent segments concatenated
	tkQIdent         // "..." with "" escapes (dialect 3)
	tkHexStr         // x'...' hex string constant
	tkSymbol         // run of punctuation bytes
)

type token struct {
	kind  tokKind
	start int
	end   int    // exclusive
	upper string // ASCII-uppercased text for tkWord; "" otherwise
}

func (t token) text(in string) string {
	if t.kind == tkEOF {
		return ""
	}
	return in[t.start:t.end]
}

func (t token) isWord(w string) bool { return t.kind == tkWord && t.upper == w }

// lexer is a single-pass, allocation-light scanner. It records lexical
// issues instead of failing (FR-3, NFR-2).
type lexer struct {
	in     string
	pos    int
	d1     bool // dialect 1: " lexes as string
	issues []Issue
	done   bool
}

func newLexer(in string, cfg *config) *lexer {
	return &lexer{in: in, d1: cfg.dialect == Dialect1}
}

func (lx *lexer) addIssue(k IssueKind, msg string, off int) {
	lx.issues = append(lx.issues, Issue{Kind: k, Msg: msg, Offset: off})
}

func isASCIISpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

func isASCIIIdentStart(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b >= 0x80
}

func isASCIIIdentCont(b byte) bool {
	return isASCIIIdentStart(b) || isDigit(b) || b == '$'
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func isHexDigit(b byte) bool {
	return isDigit(b) || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

// skipSpaceComments advances past whitespace and comments, returning false
// at end of input. Unterminated block comments consume to EOF and are
// reported once. Mirrors yylexSkipSpaces, including the fact that line
// comments may end at EOF without an error (unterminated block comments
// are the engine's "unterminated block comment" error).
func (lx *lexer) skipSpaceComments() bool {
	in := lx.in
	for lx.pos < len(in) {
		c := in[lx.pos]
		if isASCIISpace(c) {
			lx.pos++
			continue
		}
		if c == '-' && lx.pos+1 < len(in) && in[lx.pos+1] == '-' {
			lx.pos += 2
			for lx.pos < len(in) && in[lx.pos] != '\n' {
				lx.pos++
			}
			continue
		}
		if c == '/' && lx.pos+1 < len(in) && in[lx.pos+1] == '*' {
			start := lx.pos
			lx.pos += 2
			closed := false
			for lx.pos < len(in) {
				if in[lx.pos] == '*' && lx.pos+1 < len(in) && in[lx.pos+1] == '/' {
					lx.pos += 2
					closed = true
					break
				}
				lx.pos++
			}
			if !closed {
				lx.addIssue(IssueUnclosedLiteral, "fbparse: unterminated block comment", start)
			}
			continue
		}
		return true
	}
	return false
}

// next returns the next token; tkEOF once exhausted.
func (lx *lexer) next() token {
	if lx.done || !lx.skipSpaceComments() {
		lx.done = true
		return token{kind: tkEOF, start: lx.pos, end: lx.pos}
	}
	in := lx.in
	start := lx.pos
	c := in[lx.pos]

	switch {
	case c == '\'':
		lx.pos++
		lx.scanQuotedBody('\'')
		// Adjacent segments separated by whitespace/comments concatenate
		// into one string constant (Parser.cpp StrMark loop).
		for {
			save := lx.pos
			if !lx.skipSpaceComments() || in[lx.pos] != '\'' {
				lx.pos = save
				break
			}
			lx.pos++
			lx.scanQuotedBody('\'')
		}
		return token{kind: tkString, start: start, end: lx.pos}

	case c == '"':
		lx.pos++
		lx.scanQuotedBody('"')
		kind := tokKind(tkQIdent)
		if lx.d1 {
			kind = tkString
		}
		return token{kind: kind, start: start, end: lx.pos}

	case (c == 'x' || c == 'X') && lx.pos+1 < len(in) && in[lx.pos+1] == '\'':
		lx.pos += 2
		for lx.pos < len(in) {
			if in[lx.pos] == '\'' {
				lx.pos++
				break
			}
			if isHexDigit(in[lx.pos]) || in[lx.pos] == ' ' {
				lx.pos++
				continue
			}
			// Illegal nibble: treat the rest up to a quote as part of the
			// token so the lexer resynchronizes at the next token boundary.
			lx.addIssue(IssueAmbiguousParse, "fbparse: malformed hex string constant", start)
			for lx.pos < len(in) && in[lx.pos] != '\'' {
				lx.pos++
			}
			if lx.pos < len(in) {
				lx.pos++
			}
			break
		}
		return token{kind: tkHexStr, start: start, end: lx.pos}

	case isDigit(c) || (c == '.' && lx.pos+1 < len(in) && isDigit(in[lx.pos+1])):
		lx.scanNumber()
		return token{kind: tkNumber, start: start, end: lx.pos}

	case isASCIIIdentStart(c):
		lx.pos++
		for lx.pos < len(in) && isASCIIIdentCont(in[lx.pos]) {
			lx.pos++
		}
		return token{kind: tkWord, start: start, end: lx.pos, upper: asciiUpper(in[start:lx.pos])}

	default:
		// Punctuation. Structural delimiters stay single-token; known
		// two-char operators merge; runs of one repeated character
		// (isql terminators like "!!") merge. This keeps ";" always
		// visible to terminator matching and "(" / ")" exact for depth
		// tracking.
		lx.pos++
		if lx.pos < len(in) {
			two := string(c) + string(in[lx.pos])
			if twoCharOps[two] {
				lx.pos++
				return token{kind: tkSymbol, start: start, end: lx.pos}
			}
		}
		switch c {
		case ';', '(', ')', ',', '.':
			// single-token delimiters
		default:
			for lx.pos < len(in) && in[lx.pos] == c {
				lx.pos++
			}
		}
		return token{kind: tkSymbol, start: start, end: lx.pos}
	}
}

// twoCharOps mirrors the multi-character tokens of ParserTokens.h.
var twoCharOps = map[string]bool{
	"!<": true, "!=": true, "!>": true,
	"<=": true, "<>": true, "=>": true, ">=": true,
	":=": true, "||": true,
	"^<": true, "^=": true, "^>": true,
	"~<": true, "~=": true, "~>": true,
}

// scanQuotedBody consumes the inside of a quote-delimited region whose
// opening quote was already consumed. Doubling is the escape. Unterminated
// regions consume to EOF and are reported once (yyerror "unterminated
// string").
func (lx *lexer) scanQuotedBody(quote byte) {
	in := lx.in
	for lx.pos < len(in) {
		if in[lx.pos] == quote {
			if lx.pos+1 < len(in) && in[lx.pos+1] == quote {
				lx.pos += 2
				continue
			}
			lx.pos++
			return
		}
		lx.pos++
	}
	lx.addIssue(IssueUnclosedLiteral, "fbparse: unterminated string literal", lx.pos)
}

func (lx *lexer) scanNumber() {
	in := lx.in
	// Hex integer literal 0x...
	if in[lx.pos] == '0' && lx.pos+1 < len(in) && (in[lx.pos+1] == 'x' || in[lx.pos+1] == 'X') {
		lx.pos += 2
		for lx.pos < len(in) && isHexDigit(in[lx.pos]) {
			lx.pos++
		}
		return
	}
	for lx.pos < len(in) && isDigit(in[lx.pos]) {
		lx.pos++
	}
	if lx.pos < len(in) && in[lx.pos] == '.' {
		lx.pos++
		for lx.pos < len(in) && isDigit(in[lx.pos]) {
			lx.pos++
		}
	}
	if lx.pos < len(in) && (in[lx.pos] == 'e' || in[lx.pos] == 'E') {
		p := lx.pos + 1
		if p < len(in) && (in[p] == '+' || in[p] == '-') {
			p++
		}
		if p < len(in) && isDigit(in[p]) {
			lx.pos = p
			for lx.pos < len(in) && isDigit(in[lx.pos]) {
				lx.pos++
			}
		}
	}
}

// asciiUpper uppercases ASCII letters only. Non-ASCII bytes pass through
// unchanged so Unicode-confusable verbs (Cyrillic-lookalike CREATE) can
// never equal an ASCII keyword (P6.1 T2 adversarial class).
func asciiUpper(s string) string {
	hasLower := false
	for i := 0; i < len(s); i++ {
		if s[i] >= 'a' && s[i] <= 'z' {
			hasLower = true
			break
		}
	}
	if !hasLower {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

// unquote strips delimiters and undoubles embedded quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		s = s[1 : len(s)-1]
	}
	return strings.ReplaceAll(s, `""`, `"`)
}

// lexAll collects all tokens of in (used per statement at classify time).
func lexAll(in string, cfg *config) ([]token, []Issue) {
	lx := newLexer(in, cfg)
	var toks []token
	for {
		t := lx.next()
		if t.kind == tkEOF {
			return toks, lx.issues
		}
		toks = append(toks, t)
	}
}
