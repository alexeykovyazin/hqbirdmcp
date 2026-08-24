package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

var toolNameRe = regexp.MustCompile(`fb_[a-z][a-z0-9_]*`)

// Files that intentionally reference tools that do not exist (known-gap
// notes) are exempt from the phantom-tool lint.
var lintExempt = map[string]bool{
	"docs/decisions/phase5-gap-notes.md": true,
}

func lintOneMarkdown(t *testing.T, known map[string]bool, repoRoot, path string) int {
	t.Helper()
	rel, err := filepath.Rel(repoRoot, path)
	if err != nil {
		t.Fatal(err)
	}
	if lintExempt[filepath.ToSlash(rel)] {
		return 0
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range toolNameRe.FindAllString(string(b), -1) {
		if !known[name] {
			t.Errorf("%s references phantom tool %s", rel, name)
		}
	}
	return 1
}

func TestPlaybookHonestyLint(t *testing.T) {
	known := map[string]bool{}
	for _, m := range toolMeta {
		known[m.Name] = true
	}
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	roots := []string{
		filepath.Join(root, "prompts"),
		filepath.Join(root, "docs"),
		filepath.Join(root, "README.md"),
	}
	n := 0
	for _, r := range roots {
		info, err := os.Stat(r)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			n += lintOneMarkdown(t, known, root, r)
			continue
		}
		err = filepath.WalkDir(r, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
				return nil
			}
			n += lintOneMarkdown(t, known, root, p)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if n == 0 {
		t.Fatal("no markdown found")
	}
}

func TestToolReferenceCoversToolMeta(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	p := filepath.Join(filepath.Dir(thisFile), "..", "..", "docs", "tool-reference.md")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	for _, m := range toolMeta {
		if !strings.Contains(body, "`"+m.Name+"`") {
			t.Errorf("tool-reference.md missing %s", m.Name)
		}
	}
}
