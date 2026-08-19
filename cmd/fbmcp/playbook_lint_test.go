package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

var toolNameRe = regexp.MustCompile(`fb_[a-z][a-z0-9_]*`)

func TestPlaybookHonestyLint(t *testing.T) {
	known := map[string]bool{}
	for _, m := range toolMeta {
		known[m.Name] = true
	}
	_, thisFile, _, _ := runtime.Caller(0)
	cmdDir := filepath.Dir(thisFile)
	roots := []string{
		filepath.Join(cmdDir, "..", "..", "prompts"),
		filepath.Join(cmdDir, "..", "..", "docs"),
	}
	n := 0
	for _, root := range roots {
		ents, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			n++
			p := filepath.Join(root, e.Name())
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range toolNameRe.FindAllString(string(b), -1) {
				if !known[name] {
					t.Errorf("%s references phantom tool %s", e.Name(), name)
				}
			}
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
