package fbparse

import "strings"

// templateOf replaces literal values with '?' while leaving everything
// else — including identifier quoting — byte-for-byte unchanged (FR-14).
//
// Replaced: single-quoted strings (all concatenated segments as one '?'),
// hex string constants, and dialect-1 double-quoted strings. Preserved:
// numeric literals (Firebird grammar places bare numbers in name
// positions, e.g. DROP SHADOW n, POSITION n — and literals are inert for
// classification), quoted identifiers, all whitespace and comments.
func templateOf(raw string, d1 bool) string {
	if !strings.ContainsAny(raw, "'\"xX") {
		return raw
	}
	cfg := config{term: ";"}
	if d1 {
		cfg.dialect = Dialect1
	}
	lx := newLexer(raw, &cfg)
	var b strings.Builder
	prev := 0
	for {
		t := lx.next()
		if t.kind == tkEOF {
			break
		}
		switch t.kind {
		case tkString, tkHexStr:
			b.WriteString(raw[prev:t.start])
			b.WriteByte('?')
			prev = t.end
		}
	}
	if prev == 0 {
		return raw
	}
	b.WriteString(raw[prev:])
	return b.String()
}
