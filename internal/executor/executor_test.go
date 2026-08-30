package executor

import (
	"strings"
	"testing"
)

func TestPrepareDeniesGarbage(t *testing.T) {
	if _, err := Prepare("~~~ nonsense ~~~"); err == nil {
		t.Fatal("expected deny")
	}
}

func TestPrepareDeniesDropDatabase(t *testing.T) {
	if _, err := Prepare("DROP DATABASE"); err == nil {
		t.Fatal("expected tier-3 deny")
	}
}

func TestPrepareDeniesReads(t *testing.T) {
	_, err := Prepare("SELECT * FROM T")
	if err == nil {
		t.Fatal("expected read deny")
	}
	if !strings.Contains(err.Error(), "fb_query") {
		t.Fatalf("read deny must redirect to fb_query, got: %v", err)
	}
}

func TestPrepareDeniesMixedTiers(t *testing.T) {
	if _, err := Prepare("INSERT INTO T VALUES (1); DROP TABLE T"); err == nil {
		t.Fatal("expected mixed-tier deny")
	}
}

func TestPrepareAcceptsDML(t *testing.T) {
	p, err := Prepare("INSERT INTO T (A) VALUES (1)")
	if err != nil {
		t.Fatal(err)
	}
	if p.MaxTier != 1 || p.HasDDL {
		t.Fatalf("got tier=%d ddl=%v", p.MaxTier, p.HasDDL)
	}
}

func TestPrepareDDL(t *testing.T) {
	p, err := Prepare("CREATE TABLE T (A INT)")
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasDDL || p.MaxTier != 1 {
		t.Fatalf("got tier=%d ddl=%v", p.MaxTier, p.HasDDL)
	}
}

func TestImpactOmitsSafe(t *testing.T) {
	p, err := Prepare("DELETE FROM T WHERE ID = 1")
	if err != nil {
		t.Fatal(err)
	}
	txt := (*Service)(nil).Impact(nil, "", p)
	if containsFold(txt, "safe") {
		t.Fatalf("preview used forbidden word: %s", txt)
	}
	if !containsFold(txt, "compensation") {
		t.Fatalf("missing compensation: %s", txt)
	}
}

func containsFold(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub ||
		len(s) > 0 && (indexFold(s, sub) >= 0))
}

func indexFold(s, sub string) int {
	ls, lsub := []rune(s), []rune(sub)
	for i := 0; i+len(lsub) <= len(ls); i++ {
		ok := true
		for j := range lsub {
			a, b := ls[i+j], lsub[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}
