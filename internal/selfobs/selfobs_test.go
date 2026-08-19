package selfobs

import (
	"strings"
	"testing"
)

func TestCapsAndDump(t *testing.T) {
	r := New()
	r.Add("jobs_succeeded", 1)
	r.Add("jobs_succeeded", 2)
	r.Set("gate_pending", 4)
	if r.Get("jobs_succeeded") != 3 {
		t.Fatal(r.Get("jobs_succeeded"))
	}
	dump := r.Prometheus()
	if !strings.Contains(dump, "jobs_succeeded 3") || !strings.Contains(dump, "gate_pending 4") {
		t.Fatal(dump)
	}
	for i := 0; i < 300; i++ {
		r.Set("m"+string(rune('a'+(i%26)))+string(rune('0'+i%10)), i)
	}
	lines := strings.Count(r.Prometheus(), "\n")
	if lines > 200 {
		t.Fatalf("cardinality cap failed: %d lines", lines)
	}
}
