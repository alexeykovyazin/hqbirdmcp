package hqbirdsetup

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/aleks/fbmcp/internal/config"
)

// The databases of a line, registered by hqbird-install (owner 2026-11-05):
// the aliases of databases.conf and the databases the agent saw attached
// lately. Each one gets SYSDBA for the read and the admin pool (reads go
// through a read-only transaction).

// recentTail is how much of the newest getactdb file is read (fbagent
// configeditor reads the same 8 MB).
const recentTail = 8 << 20

// securityRe matches the security databases (security2.fdb … security6.fdb):
// they are attached all the time and are no user database.
var securityRe = regexp.MustCompile(`(?i)^security\d*\.fdb$`)

// DatabasesConfPaths returns the file paths of the aliases in a
// databases.conf: "alias = path", with or without a { } block of settings.
// Paths with $(macro), the security.db entry and remote paths are skipped.
func DatabasesConfPaths(src string) []string {
	var out []string
	depth := 0
	for _, raw := range strings.Split(src, "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if depth > 0 {
			depth += strings.Count(line, "{") - strings.Count(line, "}")
			continue
		}
		if strings.HasPrefix(line, "{") {
			depth = strings.Count(line, "{") - strings.Count(line, "}")
			continue
		}
		alias, path, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if j := strings.Index(path, "{"); j >= 0 {
			depth = strings.Count(path[j:], "{") - strings.Count(path[j:], "}")
			path = path[:j]
		}
		path = strings.Trim(strings.TrimSpace(path), `"`)
		if strings.EqualFold(strings.TrimSpace(alias), "security.db") || !localDBPath(path) {
			continue
		}
		out = append(out, path)
	}
	return out
}

// RecentDatabases returns the databases in the newest
// <dataPath>/server_level/GetActiveDatabases/getactdb_*.jsonl that fbagent
// writes each minute.
func RecentDatabases(dataPath string) []string {
	files, _ := filepath.Glob(filepath.Join(dataPath, "server_level", "GetActiveDatabases", "getactdb_*.jsonl*"))
	var newest string
	var newestMod int64
	for _, f := range files {
		if strings.HasSuffix(f, ".gz") {
			continue
		}
		if fi, err := os.Stat(f); err == nil && fi.ModTime().UnixNano() > newestMod {
			newest, newestMod = f, fi.ModTime().UnixNano()
		}
	}
	if newest == "" {
		return nil
	}
	f, err := os.Open(newest)
	if err != nil {
		return nil
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > recentTail {
		_, _ = f.Seek(fi.Size()-recentTail, io.SeekStart)
	}
	b, _ := io.ReadAll(f)
	var out []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var e struct {
			Databases []struct {
				Path string `json:"path"`
			} `json:"databases"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil { // the first line after a seek is cut
			continue
		}
		for _, d := range e.Databases {
			p := strings.TrimSpace(d.Path)
			if k := config.NormalizeDBPath(p); p != "" && !seen[k] && localDBPath(p) {
				seen[k] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// localDBPath reports a full local file path of a user database.
func localDBPath(p string) bool {
	switch {
	case p == "", strings.ContainsAny(p, "$`"), // config.Validate refuses them
		strings.HasPrefix(p, `\\`), strings.HasPrefix(p, "//"), strings.Contains(p, ".."):
		return false
	case securityRe.MatchString(filepath.Base(filepath.FromSlash(strings.ReplaceAll(p, `\`, "/")))):
		return false
	}
	if len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		return !strings.Contains(p[2:], ":") // C:\db\x.fdb
	}
	return strings.HasPrefix(p, "/") && !strings.Contains(p, ":") // not host:/path
}

// lineDatabases is the list of the line from both sources, without the
// files that do not exist.
func lineDatabases(confPath, dataPath string) []string {
	var cands []string
	if b, err := os.ReadFile(confPath); err == nil {
		cands = append(cands, DatabasesConfPaths(string(b))...)
	}
	if dataPath != "" {
		cands = append(cands, RecentDatabases(dataPath)...)
	}
	var out []string
	seen := map[string]bool{}
	for _, p := range cands {
		if k := config.NormalizeDBPath(p); !seen[k] {
			seen[k] = true
			if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
				out = append(out, p)
			}
		}
	}
	return out
}
