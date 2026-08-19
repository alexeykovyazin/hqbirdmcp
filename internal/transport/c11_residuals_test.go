package transport

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aleks/fbmcp/internal/config"
)

func FuzzBearer(f *testing.F) {
	f.Add("Bearer abc")
	f.Add("bearer XYZ")
	f.Add("")
	f.Add("Basic nope")
	f.Fuzz(func(t *testing.T, h string) {
		if len(h) > 4096 {
			return
		}
		_ = bearer(h)
	})
}

// C11 residual: empty Origin allowlist permits any Origin (ADR-022 / P5.1).
func TestC11EmptyOriginAllowsAny(t *testing.T) {
	okH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	ids := []config.APIIdentity{{Name: "op", KeyEnv: "X", MaxTier: 2}}
	h := Auth(ids, map[string]string{"op": "good-key"}, nil, okH)
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer good-key")
	req.Header.Set("Origin", "https://evil.example")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("empty origin allowlist must not 403 (residual): got %d", rr.Code)
	}
}

func TestC11NoRateLimitSurface(t *testing.T) {
	// Documented residual: transport.Auth has no rate-limit or session cap.
	okH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	ids := []config.APIIdentity{{Name: "op", KeyEnv: "X", MaxTier: 2}}
	h := Auth(ids, map[string]string{"op": "good-key"}, nil, okH)
	for i := 0; i < 50; i++ {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer good-key")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("burst %d: %d (no rate-limit residual)", i, rr.Code)
		}
	}
}
