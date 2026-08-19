package notify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestC10WebhookSecretNotInLog(t *testing.T) {
	dir := t.TempDir()
	secret := "super-hook-secret-value"
	bus, err := New(dir, "", secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Emit(Event{Type: "job.failed", Message: "backup failed", Detail: map[string]string{"note": "ok"}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secret) {
		t.Fatalf("webhook secret leaked into events.jsonl:\n%s", b)
	}
}
