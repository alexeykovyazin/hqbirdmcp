package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTailLines(t *testing.T) {
	s := "a\nb\nc\nd\n"
	if got := tailLines(s, 2); got != "c\nd\n" {
		t.Errorf("tail 2 = %q", got)
	}
	if got := tailLines(s, 10); got != s {
		t.Errorf("tail 10 = %q", got)
	}
	if got := tailLines(s, 0); got != s {
		t.Errorf("tail 0 = %q", got)
	}
	if got := tailLines("x\r\ny\r\nz\r\n", 1); strings.TrimSpace(got) != "z" {
		t.Errorf("CRLF tail 1 = %q", got)
	}
}

func TestRedactSecrets(t *testing.T) {
	var doc any
	_ = json.Unmarshal([]byte(`{"vbackup":{"enabled":true,"auth_token":"T","jobs":[{"db":"x","Password":"P","backup_dir":"E:\\b"}]},"secret_key":"S","ApiCredentials":{"u":"v"}}`), &doc)
	b, _ := json.Marshal(redactSecrets(doc))
	out := string(b)
	for _, leak := range []string{`"T"`, `"P"`, `"S"`, `"v"`} {
		if strings.Contains(out, leak) {
			t.Errorf("secret %s leaked: %s", leak, out)
		}
	}
	for _, keep := range []string{`"enabled":true`, `"backup_dir":"E:\\b"`, `"db":"x"`} {
		if !strings.Contains(out, keep) {
			t.Errorf("non-secret %s lost: %s", keep, out)
		}
	}
}
