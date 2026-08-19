package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aleks/fbmcp/internal/policy"
)

// C22 fuse #4: policy Evaluate and audit.Log run on the real request path.
func TestC22DeadCodeDetector(t *testing.T) {
	gt := newTestGT(t)
	meta := policy.ToolMeta{Name: "fb_backup_start", Tier: 1, Scope: "database"}
	out := gt.requestGated(context.Background(), meta, "backup %s", "spike5", nil, "", nil)
	if strings.HasPrefix(out, "DENIED:") {
		t.Fatalf("unexpected deny: %s", out)
	}
	p := filepath.Join(gt.live().State.Dir, "audit.jsonl")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"tool":"fb_backup_start"`) {
		t.Fatalf("audit not invoked on tool path:\n%s", s)
	}
	if !strings.Contains(s, `"decision":"pending"`) {
		t.Fatalf("policy/gate pending not audited:\n%s", s)
	}
	if len(gt.st.Pending()) == 0 {
		t.Fatal("gate.Request not on the real path")
	}
}

func TestC22UnknownToolDeniedAndAudited(t *testing.T) {
	gt := newTestGT(t)
	meta := policy.ToolMeta{Name: "fb_nope", Tier: 1, Scope: "database"}
	out := gt.requestGated(context.Background(), meta, "x %s", "spike5", nil, "", nil)
	if !strings.HasPrefix(out, "DENIED:") {
		t.Fatalf("unknown tool not denied: %s", out)
	}
	b, _ := os.ReadFile(filepath.Join(gt.live().State.Dir, "audit.jsonl"))
	if !strings.Contains(string(b), `"decision":"denied"`) {
		t.Fatalf("deny not audited: %s", b)
	}
}
