package backupsvc

import (
	"context"
	"testing"
)

func TestTraceTemplateAllowlist(t *testing.T) {
	for _, name := range []string{"audit-lite", "performance", "errors"} {
		if TraceTemplates[name] == "" {
			t.Fatalf("missing template %s", name)
		}
	}
	c := &Client{}
	if _, err := c.StartTrace(context.Background(), "x", "not-a-template", nil); err == nil {
		t.Fatal("unknown template accepted")
	}
}
