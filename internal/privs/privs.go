// Package privs formats and diffs Firebird RDB$USER_PRIVILEGES rows for
// P4.3 effective-access previews (before/after).
package privs

import (
	"fmt"
	"sort"
	"strings"
)

// Grant is one privilege row.
type Grant struct {
	User, Privilege, Relation, Field string
	GrantOption                      bool
}

var privName = map[string]string{
	"S": "SELECT", "I": "INSERT", "U": "UPDATE", "D": "DELETE",
	"R": "REFERENCES", "X": "EXECUTE", "A": "ALL", "G": "USAGE",
	"M": "ROLE",
}

func (g Grant) Line() string {
	p := g.Privilege
	if n, ok := privName[strings.ToUpper(p)]; ok {
		p = n
	}
	rel := strings.TrimSpace(g.Relation)
	if rel == "" {
		rel = "*"
	}
	s := fmt.Sprintf("%s ON %s", p, rel)
	if f := strings.TrimSpace(g.Field); f != "" {
		s += "." + f
	}
	if g.GrantOption {
		s += " WITH GRANT OPTION"
	}
	return s
}

// Format lists grants sorted, one per line.
func Format(gs []Grant) string {
	lines := make([]string, 0, len(gs))
	for _, g := range gs {
		lines = append(lines, g.Line())
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return "(no privileges)"
	}
	return strings.Join(lines, "\n")
}

// Diff returns added and removed lines (after vs before). Traversal is
// capped by the caller; this is a set-diff of formatted lines.
func Diff(before, after []Grant) (added, removed []string) {
	bset := map[string]bool{}
	aset := map[string]bool{}
	for _, g := range before {
		bset[g.Line()] = true
	}
	for _, g := range after {
		aset[g.Line()] = true
	}
	for l := range aset {
		if !bset[l] {
			added = append(added, l)
		}
	}
	for l := range bset {
		if !aset[l] {
			removed = append(removed, l)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// ApplyPreview synthesizes the after-set for a single GRANT/REVOKE without
// hitting the engine (preview-only; depth cap = 1, no role expansion).
func ApplyPreview(before []Grant, verb, privilege, relation, grantee string) []Grant {
	priv := strings.ToUpper(privilege)
	letter := priv
	for k, v := range privName {
		if v == priv {
			letter = k
			break
		}
	}
	row := Grant{User: grantee, Privilege: letter, Relation: relation}
	if strings.EqualFold(verb, "REVOKE") {
		var out []Grant
		for _, g := range before {
			if strings.EqualFold(g.User, grantee) &&
				(strings.EqualFold(g.Privilege, letter) || strings.EqualFold(privName[strings.ToUpper(g.Privilege)], priv)) &&
				strings.EqualFold(strings.TrimSpace(g.Relation), relation) {
				continue
			}
			out = append(out, g)
		}
		return out
	}
	return append(append([]Grant(nil), before...), row)
}
