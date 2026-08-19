package notify

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSignVerify(t *testing.T) {
	body := []byte(`{"id":"e1","type":"job.failed"}`)
	sig := Sign("s3cret", body)
	if !Verify("s3cret", sig, body) {
		t.Fatal("valid signature rejected")
	}
	if Verify("s3cret", sig, []byte("tampered")) {
		t.Fatal("tampered body accepted")
	}
	if Verify("other", sig, body) {
		t.Fatal("wrong secret accepted")
	}
}

func TestWebhookAndReplay(t *testing.T) {
	guard := NewReplayGuard()
	var gotID, gotSig string
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID = r.Header.Get("X-FBMCP-Event-Id")
		gotSig = strings.TrimPrefix(r.Header.Get("X-FBMCP-Signature"), "sha256=")
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if !Verify("hook-secret", gotSig, b) {
			t.Error("handler: signature mismatch")
			w.WriteHeader(400)
			return
		}
		if !guard.Accept(gotID) {
			w.WriteHeader(409)
			return
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()

	bus, err := New(t.TempDir(), srv.URL, "hook-secret")
	if err != nil {
		t.Fatal(err)
	}
	e := Event{ID: "evt-1", Type: "scheduler.skip", Message: "overlap"}
	if err := bus.Emit(e); err != nil {
		t.Fatal(err)
	}
	if gotID != "evt-1" {
		t.Fatalf("event id header = %q", gotID)
	}
	if !guard.Accept("evt-1") == false {
		// already seen
	}
	if guard.Accept("evt-1") {
		t.Fatal("replay of evt-1 accepted")
	}
	logb, _ := os.ReadFile(filepath.Join(t.TempDir(), "unused"))
	_ = logb
	if len(bodies) != 1 {
		t.Fatalf("deliveries = %d", len(bodies))
	}
}

func TestLocalLogAlwaysWritten(t *testing.T) {
	dir := t.TempDir()
	bus, _ := New(dir, "", "")
	if err := bus.Emit(Event{Type: "job.succeeded", Message: "ok"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil || !strings.Contains(string(b), "job.succeeded") {
		t.Fatalf("event log missing: %v %s", err, b)
	}
}
