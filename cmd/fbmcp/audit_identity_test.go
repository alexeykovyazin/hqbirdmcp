package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// L1 finding (phase 12): Tier-0 tools audited remote calls as "local"
// because the identity was hard-coded. Tool handlers must audit
// identity.Caller(ctx); only kernel events (reload_hooks.go) are "local".
func TestToolAuditsUseCaller(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "reload_hooks.go" {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, `audit.Entry{Identity: "local"`) {
				t.Errorf("%s:%d audits a hard-coded \"local\" identity; use identity.Caller(ctx).Name", f, i+1)
			}
		}
	}
}
