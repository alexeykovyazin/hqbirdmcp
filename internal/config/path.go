package config

import (
	"path"
	"strings"
)

// NormalizeDBPath canonicalizes a database file path for matching
// (slash folding, Clean, Windows drive-letter case).
func NormalizeDBPath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, `\`, `/`))
	if p == "" {
		return ""
	}
	p = path.Clean(p)
	if len(p) >= 3 && p[1] == ':' && p[2] == '/' {
		return strings.ToLower(p)
	}
	return p
}
