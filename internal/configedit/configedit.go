// Package configedit implements P4.6: parse → validate → atomic apply of
// firebird.conf / databases.conf against the ADR-020 parameter registry.
package configedit

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Param is one registry entry.
type Param struct {
	Name             string
	Kind             string // int | string | enum | bool
	Enum             []string
	Min, Max         *int
	Restart          bool
	Security         bool // escalate to Tier 2
	MinFB            string
	Default          string
}

// Registry is the curated firebird.conf parameter set.
var Registry = map[string]Param{
	"DefaultDbCachePages": {Name: "DefaultDbCachePages", Kind: "int", Restart: true, Default: "2048"},
	"TempCacheLimit":      {Name: "TempCacheLimit", Kind: "int", Restart: true},
	"TempDirectories":     {Name: "TempDirectories", Kind: "string", Restart: true},
	"RemoteServicePort":   {Name: "RemoteServicePort", Kind: "int", Restart: true, Security: true},
	"RemoteBindAddress":   {Name: "RemoteBindAddress", Kind: "string", Restart: true, Security: true},
	"WireCrypt":           {Name: "WireCrypt", Kind: "enum", Enum: []string{"Enabled", "Required", "Disabled"}, Restart: true, Security: true, MinFB: "3.0", Default: "Enabled"},
	"AuthServer":          {Name: "AuthServer", Kind: "string", Restart: true, Security: true, MinFB: "3.0"},
	"AuthClient":          {Name: "AuthClient", Kind: "string", Restart: true, Security: true, MinFB: "3.0"},
	"WireCompression":     {Name: "WireCompression", Kind: "bool", Restart: true, MinFB: "3.0"},
	"MaxUnflushedWrites":  {Name: "MaxUnflushedWrites", Kind: "int", Restart: false},
	"BugcheckAbort":       {Name: "BugcheckAbort", Kind: "bool", Restart: true},
	"FileSystemCacheThreshold": {Name: "FileSystemCacheThreshold", Kind: "int", Restart: true},
}

// Line is one physical line of a conf file.
type Line struct {
	Raw     string // original text including newline stripped
	Comment bool
	Blank   bool
	Key     string // canonical key if this is an assignment
	Value   string
}

// File is a parsed conf.
type File struct {
	Path  string
	Lines []Line
}

// ParseFile reads a firebird.conf-style file, preserving comments/blanks.
func ParseFile(path string) (File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	return Parse(path, string(b)), nil
}

func Parse(path, src string) File {
	var lines []Line
	for _, raw := range strings.Split(src, "\n") {
		raw = strings.TrimRight(raw, "\r")
		l := Line{Raw: raw}
		trim := strings.TrimSpace(raw)
		if trim == "" {
			l.Blank = true
			lines = append(lines, l)
			continue
		}
		if strings.HasPrefix(trim, "#") || strings.HasPrefix(trim, ";") {
			l.Comment = true
			lines = append(lines, l)
			continue
		}
		key, val, ok := splitAssign(trim)
		if !ok {
			l.Comment = true // keep unrecognized as opaque
			lines = append(lines, l)
			continue
		}
		l.Key, l.Value = key, val
		lines = append(lines, l)
	}
	return File{Path: path, Lines: lines}
}

func splitAssign(s string) (string, string, bool) {
	i := strings.IndexAny(s, "=\t ")
	if i <= 0 {
		return "", "", false
	}
	key := s[:i]
	rest := strings.TrimLeftFunc(s[i:], func(r rune) bool { return r == '=' || unicode.IsSpace(r) })
	// strip trailing comment
	if j := strings.IndexAny(rest, "#;"); j >= 0 {
		rest = strings.TrimSpace(rest[:j])
	}
	return key, rest, true
}

func (f File) Get(name string) (string, bool) {
	for i := len(f.Lines) - 1; i >= 0; i-- {
		if strings.EqualFold(f.Lines[i].Key, name) {
			return f.Lines[i].Value, true
		}
	}
	return "", false
}

func (f File) String() string {
	var b strings.Builder
	for i, l := range f.Lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		if l.Key != "" {
			fmt.Fprintf(&b, "%s = %s", l.Key, l.Value)
		} else {
			b.WriteString(l.Raw)
		}
	}
	if !strings.HasSuffix(b.String(), "\n") && len(f.Lines) > 0 {
		b.WriteByte('\n')
	}
	return b.String()
}

// ValidateSet checks the registry and type/range.
func ValidateSet(name, value string) (Param, error) {
	p, ok := Registry[name]
	if !ok {
		// case-insensitive lookup
		for k, v := range Registry {
			if strings.EqualFold(k, name) {
				p, ok = v, true
				break
			}
		}
	}
	if !ok {
		return Param{}, fmt.Errorf("unknown parameter %q (registry-validated only, ADR-020)", name)
	}
	switch p.Kind {
	case "int":
		n, err := strconv.Atoi(value)
		if err != nil {
			return p, fmt.Errorf("%s: not an int", name)
		}
		if p.Min != nil && n < *p.Min {
			return p, fmt.Errorf("%s: below min", name)
		}
		if p.Max != nil && n > *p.Max {
			return p, fmt.Errorf("%s: above max", name)
		}
	case "bool":
		switch strings.ToLower(value) {
		case "1", "0", "true", "false", "yes", "no":
		default:
			return p, fmt.Errorf("%s: not a bool", name)
		}
	case "enum":
		ok := false
		for _, e := range p.Enum {
			if strings.EqualFold(e, value) {
				ok = true
				break
			}
		}
		if !ok {
			return p, fmt.Errorf("%s: value %q not in %v", name, value, p.Enum)
		}
	}
	return p, nil
}

// Apply sets name=value, returning a new File (original unchanged).
func (f File) Apply(name, value string) File {
	out := File{Path: f.Path, Lines: append([]Line(nil), f.Lines...)}
	for i := range out.Lines {
		if strings.EqualFold(out.Lines[i].Key, name) {
			out.Lines[i].Key = name
			out.Lines[i].Value = value
			return out
		}
	}
	out.Lines = append(out.Lines, Line{Key: name, Value: value})
	return out
}

// AtomicWrite writes f to path via .new + rename, keeping .prev.
func AtomicWrite(path string, body string) error {
	prev := path + ".prev"
	neu := path + ".new"
	if err := os.WriteFile(neu, []byte(body), 0o644); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(prev, data, 0o644); err != nil {
			return err
		}
	}
	if err := os.Rename(neu, path); err != nil {
		// Windows: replace existing
		_ = os.Remove(path)
		if err2 := os.Rename(neu, path); err2 != nil {
			return err
		}
	}
	return nil
}

// AppendJournal writes one JSONL record under stateDir/config-journal/.
func AppendJournal(stateDir, instance, path, name, oldV, newV string) error {
	dir := filepath.Join(stateDir, "config-journal")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	line := fmt.Sprintf(`{"ts":%q,"instance":%q,"file":%q,"param":%q,"old":%q,"new":%q}`+"\n",
		time.Now().UTC().Format(time.RFC3339), instance, path, name, oldV, newV)
	f, err := os.OpenFile(filepath.Join(dir, instance+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line)
	return err
}

// ConfPath returns the default firebird.conf for an install root.
func ConfPath(binDir string) string {
	return filepath.Join(binDir, "firebird.conf")
}

func DatabasesConfPath(binDir string) string {
	return filepath.Join(binDir, "databases.conf")
}
