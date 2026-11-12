package rcmapi

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// digestServer checks RFC 7616 MD5 Digest like RCM: a request without the
// right Authorization gets 401 with a challenge.
func digestServer(t *testing.T, user, pass string, next http.HandlerFunc) http.Handler {
	const nonce = "n0nce"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if h == "" {
			w.Header().Set("WWW-Authenticate", `Digest realm="hqbirdrcm", nonce="`+nonce+`", qop="auth", algorithm=MD5, opaque="op"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		p := parseChallenge(h)
		ha1 := md5Hex(p["username"] + ":hqbirdrcm:" + pass)
		ha2 := md5Hex(r.Method + ":" + p["uri"])
		want := md5Hex(ha1 + ":" + nonce + ":" + p["nc"] + ":" + p["cnonce"] + ":auth:" + ha2)
		if p["username"] != user || p["response"] != want || p["uri"] != r.URL.RequestURI() || p["opaque"] != "op" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"bad login"}}`))
			return
		}
		next(w, r)
	})
}

func TestDigestLoginAndTLS(t *testing.T) {
	var actor string
	srv := httptest.NewTLSServer(digestServer(t, "rcmadmin", "secret", func(w http.ResponseWriter, r *http.Request) {
		actor = r.Header.Get("X-Admin-User")
		_, _ = w.Write([]byte(`{"alive":true}`))
	}))
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	cl, err := New(srv.URL, ca, "rcmadmin", "secret")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := cl.WithActor("fbmcp:alice").Do(context.Background(), http.MethodGet, "/v1/nodes/n%201/config", nil)
	if err != nil || string(raw) != `{"alive":true}` {
		t.Fatalf("raw %s err %v", raw, err)
	}
	if actor != "fbmcp:alice" {
		t.Fatalf("actor %q", actor)
	}

	bad, _ := New(srv.URL, ca, "rcmadmin", "wrong")
	_, err = bad.Do(context.Background(), http.MethodGet, "/v1/health", nil)
	if err == nil || !strings.Contains(err.Error(), "fbmcp ctl secret set FBMCP_RCM_PASSWORD") {
		t.Fatalf("want the 401 hint, got %v", err)
	}

	// Without the CA the TLS check fails.
	nocA, _ := New(srv.URL, "", "rcmadmin", "secret")
	if _, err := nocA.Do(context.Background(), http.MethodGet, "/v1/health", nil); err == nil {
		t.Fatal("want a TLS error without the cluster CA")
	}
	if _, err := New(srv.URL, filepath.Join(t.TempDir(), "none"), "u", "p"); err == nil {
		t.Fatal("want an error for a missing CA file")
	}
}

func TestErrorDecode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/coded":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"duplicate_guid","message":"same guid"}}`))
		case "/flat":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"plain message","code":"bad_request"}`))
		case "/429":
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("upstream down"))
		}
	}))
	defer srv.Close()
	cl, _ := New(srv.URL, "", "u", "p")
	ctx := context.Background()
	_, err := cl.Do(ctx, http.MethodPost, "/coded", map[string]any{"a": 1})
	if !IsCode(err, "duplicate_guid") || err.Error() != "rcm 409 duplicate_guid: same guid" {
		t.Fatalf("coded: %v", err)
	}
	_, err = cl.Do(ctx, http.MethodGet, "/flat", nil)
	if !IsCode(err, "bad_request") || !strings.Contains(err.Error(), "plain message") {
		t.Fatalf("flat: %v", err)
	}
	if _, err = cl.Do(ctx, http.MethodGet, "/429", nil); err == nil || !strings.Contains(err.Error(), "throttled") {
		t.Fatalf("429: %v", err)
	}
	if _, err = cl.Do(ctx, http.MethodGet, "/other", nil); err == nil || !strings.Contains(err.Error(), "upstream down") {
		t.Fatalf("text: %v", err)
	}
	if _, err = cl.Do(ctx, http.MethodGet, "v1/health", nil); err == nil {
		t.Fatal("want an error for a path without /")
	}
}

func TestWaitCommandAndJob(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/commands/"):
			if n < 3 {
				_, _ = w.Write([]byte(`{"id":"c1","state":"running"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"c1","state":"failed","error":"node said no"}`))
		case strings.HasPrefix(r.URL.Path, "/v1/reinit/"):
			_, _ = w.Write([]byte(`{"id":"r1","status":"running","phase":"copy","percent":10}`))
		case strings.HasPrefix(r.URL.Path, "/v1/admin/pair/link/"):
			_, _ = w.Write([]byte(`{"id":"L1","state":"done","step":"done"}`))
		}
	}))
	defer srv.Close()
	cl, _ := New(srv.URL, "", "u", "p")
	cmd, _, err := cl.WaitCommand(context.Background(), "c1", time.Millisecond)
	if err != nil || cmd.State != "failed" || cmd.Error != "node said no" {
		t.Fatalf("cmd %+v err %v", cmd, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var phases int
	j, _, err := cl.WaitJob(ctx, "/v1/reinit", "r1", 5*time.Millisecond, func(Job) { phases++ })
	if err == nil || j.Phase != "copy" || phases == 0 {
		t.Fatalf("want a budget error, got %+v %v", j, err)
	}
	lj, err := cl.WaitLink(context.Background(), LinkJob{ID: "L1", State: LinkRunning}, time.Millisecond, nil)
	if err != nil || lj.State != LinkDone {
		t.Fatalf("link %+v %v", lj, err)
	}
}

func TestParseChallenge(t *testing.T) {
	p := parseChallenge(`Digest realm="hqbirdrcm", nonce="a,b", qop="auth", algorithm=MD5`)
	if p["realm"] != "hqbirdrcm" || p["nonce"] != "a,b" || p["qop"] != "auth" || p["algorithm"] != "MD5" {
		t.Fatalf("%v", p)
	}
}
