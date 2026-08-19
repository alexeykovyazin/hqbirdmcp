package transport

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/identity"
)

func TestCheckRemoteRefuse(t *testing.T) {
	if err := CheckRemote("", "", "", 0); err != nil {
		t.Fatal("stdio must be allowed")
	}
	cases := []struct {
		listen, cert, key string
		n                 int
	}{
		{"127.0.0.1:8443", "c", "k", 1},
		{"localhost:8443", "c", "k", 1},
		{"10.0.0.5:8443", "", "k", 1},
		{"10.0.0.5:8443", "c", "k", 0},
	}
	for _, c := range cases {
		if err := CheckRemote(c.listen, c.cert, c.key, c.n); err == nil {
			t.Errorf("expected refuse for %+v", c)
		}
	}
	if err := CheckRemote("10.0.0.5:8443", "c", "k", 1); err != nil {
		t.Fatal(err)
	}
}

func TestAuthBattery(t *testing.T) {
	okH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := identity.FromContext(r.Context())
		if !ok || id.Name != "op" {
			t.Errorf("identity missing: %+v %v", id, ok)
		}
		w.WriteHeader(204)
	})
	ids := []config.APIIdentity{{Name: "op", KeyEnv: "X", MaxTier: 2}}
	secrets := map[string]string{"op": "good-key"}
	h := Auth(ids, secrets, []string{"https://console.example"}, okH)

	for _, entry := range MCPEntries {
		req := httptest.NewRequest(http.MethodPost, entry, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s no auth → %d", entry, rr.Code)
		}

		req = httptest.NewRequest(http.MethodPost, entry, nil)
		req.Header.Set("Authorization", "Bearer wrong")
		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s bad key → %d", entry, rr.Code)
		}

		req = httptest.NewRequest(http.MethodPost, entry, nil)
		req.Header.Set("Authorization", "Bearer good-key")
		req.Header.Set("Origin", "https://evil.example")
		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s bad origin → %d", entry, rr.Code)
		}

		req = httptest.NewRequest(http.MethodPost, entry, nil)
		req.Header.Set("Authorization", "Bearer good-key")
		req.Header.Set("Origin", "https://console.example")
		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Errorf("%s valid → %d", entry, rr.Code)
		}
	}
}

func TestAuthenticatorReplace(t *testing.T) {
	okH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	a := NewAuthenticator([]config.APIIdentity{{Name: "op", KeyEnv: "X", MaxTier: 2}}, map[string]string{"op": "old-key"}, nil)
	h := a.Handler(okH)
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer old-key")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("old key: %d", rr.Code)
	}
	a.Replace([]config.APIIdentity{{Name: "op", KeyEnv: "Y", MaxTier: 2}}, map[string]string{"op": "new-key"}, nil)
	req = httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer old-key")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("old key after replace: %d", rr.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer new-key")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("new key: %d", rr.Code)
	}
}

func TestHealthzNoLeak(t *testing.T) {
	rr := httptest.NewRecorder()
	Healthz(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	b, _ := io.ReadAll(rr.Body)
	s := string(b)
	if rr.Code != 200 || strings.TrimSpace(s) != "ok" {
		t.Fatalf("%d %q", rr.Code, s)
	}
	for _, leak := range []string{"version", "spike", "SYSDBA", "registry"} {
		if strings.Contains(strings.ToLower(s), strings.ToLower(leak)) {
			t.Fatalf("health leaked %q: %s", leak, s)
		}
	}
}

func TestEntriesEnumerated(t *testing.T) {
	if len(MCPEntries) < 2 {
		t.Fatal("need /mcp and /sse")
	}
	seen := map[string]bool{}
	for _, e := range MCPEntries {
		if seen[e] {
			t.Fatalf("dup %s", e)
		}
		seen[e] = true
	}
}
