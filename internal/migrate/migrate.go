// Package migrate implements the C.1 migration surface: ordered .sql files
// in a config-registered, confine-confined directory, each with an up
// section and an optional down section after a `-- @down` separator, a
// sha256 checksum per file, and the FBMCP_MIGRATIONS history table
// (id, version, checksum, down text, applied_at, applied_by).
//
// This package parses, orders and verifies; it never executes. All execution
// goes through the gated flow in cmd/fbmcp (ADR-030 batch semantics; the
// bootstrap CREATE TABLE itself is classified through the executor).
package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/aleks/fbmcp/internal/confine"
	"github.com/aleks/fbmcp/internal/fbparse"
)

// Table is the history table name; TableDDL is its definition. The CREATE
// goes through executor.Prepare like any other statement (ADR-030 #4).
const (
	Table    = "FBMCP_MIGRATIONS"
	TableDDL = `CREATE TABLE FBMCP_MIGRATIONS (
	ID VARCHAR(255) NOT NULL PRIMARY KEY,
	VERSION INTEGER NOT NULL UNIQUE,
	CHECKSUM CHAR(64) NOT NULL,
	DOWN_TEXT BLOB SUB_TYPE TEXT,
	APPLIED_AT TIMESTAMP NOT NULL,
	APPLIED_BY VARCHAR(255) NOT NULL
)`
)

// DownSeparator starts the down section inside a migration file.
const DownSeparator = "-- @down"

// nameRe: NNN_description.sql with a zero-padded 1–5 digit version.
var nameRe = regexp.MustCompile(`^([0-9]{1,5})[_-]([A-Za-z0-9_.-]+)\.sql$`)

// Migration is one parsed file.
type Migration struct {
	Version  int
	Name     string // file base name
	Checksum string // sha256 hex over the whole file content
	Up       string // raw up SQL (statements, not re-split here)
	Down     string // raw down SQL ("" when absent)
}

// HasDown reports whether the file carries a down section.
func (m Migration) HasDown() bool { return strings.TrimSpace(m.Down) != "" }

// Statements splits a raw section into statements via the fbparse splitter
// (the same boundaries the executor would see).
func Statements(section string) []string {
	var out []string
	for _, sp := range fbparse.Split(section) {
		s := strings.TrimSpace(section[sp.Start:sp.End])
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ParseFile parses one migration file's content.
func ParseFile(name, content string) (Migration, error) {
	m := nameRe.FindStringSubmatch(filepath.Base(name))
	if m == nil {
		return Migration{}, fmt.Errorf("migration name %q must match NNN_description.sql", filepath.Base(name))
	}
	v, err := strconv.Atoi(m[1])
	if err != nil || v <= 0 {
		return Migration{}, fmt.Errorf("migration %q: bad version %q", name, m[1])
	}
	up, down := splitSections(content)
	if strings.TrimSpace(up) == "" {
		return Migration{}, fmt.Errorf("migration %q: empty up section", name)
	}
	sum := sha256.Sum256([]byte(content))
	return Migration{
		Version:  v,
		Name:     filepath.Base(name),
		Checksum: hex.EncodeToString(sum[:]),
		Up:       up,
		Down:     down,
	}, nil
}

// splitSections cuts at the first `-- @down` line (up = before, down = after).
func splitSections(content string) (up, down string) {
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == DownSeparator {
			idx := strings.Index(content, line)
			return content[:idx], content[idx+len(line):]
		}
	}
	return content, ""
}

// LoadDir reads and orders a migrations directory. The directory must pass
// confine.Dir (config-registered root, C9); entries must be plain files
// (no subdirectories — deliberate: a flat, totally-ordered set).
func LoadDir(dir string) ([]Migration, error) {
	if err := confine.Dir(dir); err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var migs []Migration
	seenV := map[int]string{}
	for _, e := range ents {
		if e.IsDir() {
			return nil, fmt.Errorf("migrations dir: subdirectory %q not allowed (flat set)", e.Name())
		}
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue // README etc. tolerated
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		m, err := ParseFile(e.Name(), string(b))
		if err != nil {
			return nil, err
		}
		if prev, dup := seenV[m.Version]; dup {
			return nil, fmt.Errorf("duplicate version %d: %s and %s", m.Version, prev, m.Name)
		}
		seenV[m.Version] = m.Name
		migs = append(migs, m)
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].Version < migs[j].Version })
	return migs, nil
}

// Applied is one history row.
type Applied struct {
	Version   int
	Name      string
	Checksum  string
	AppliedBy string
}

// Pending returns the not-yet-applied tail of migs given history (ordered,
// contiguous enforcement: every applied version must exist in migs with the
// same checksum — history ahead of the directory is an error).
func Pending(migs []Migration, history []Applied) ([]Migration, error) {
	byV := map[int]Migration{}
	for _, m := range migs {
		byV[m.Version] = m
	}
	applied := map[int]Applied{}
	for _, h := range history {
		m, ok := byV[h.Version]
		if !ok {
			return nil, fmt.Errorf("history has version %d (%s) not present in the migrations dir — refusing; restore the file or baseline a fresh directory", h.Version, h.Name)
		}
		if m.Checksum != h.Checksum {
			return nil, fmt.Errorf("migration %s (v%d) checksum mismatch: file changed since it was applied (tamper or edit) — refusing", m.Name, m.Version)
		}
		applied[h.Version] = h
	}
	var out []Migration
	for _, m := range migs {
		if _, ok := applied[m.Version]; !ok {
			out = append(out, m)
		}
	}
	// gap check: pending versions must not be below an applied one
	maxApplied := 0
	for v := range applied {
		if v > maxApplied {
			maxApplied = v
		}
	}
	for _, m := range out {
		if m.Version < maxApplied {
			return nil, fmt.Errorf("migration %s (v%d) is pending but below applied version %d — versions must apply in order", m.Name, m.Version, maxApplied)
		}
	}
	return out, nil
}

// ManifestJSON renders the canonical batch manifest bound into the gate's
// argHash (ADR-030 #2). baseline is included so the same files with a
// different mode cannot reuse a confirmation.
func ManifestJSON(baseline bool, migs []Migration) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"baseline":%v,"files":[`, baseline)
	for i, m := range migs {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"version":%d,"name":%q,"checksum":%q}`, m.Version, m.Name, m.Checksum)
	}
	b.WriteString("]}")
	return b.String()
}
