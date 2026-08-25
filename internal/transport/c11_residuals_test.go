package transport

import (
	"net/http"
	"net/http/httptest"
	"sync"
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

// C11 closed (phase8_plan D4.1 / E.1): the former residuals are now
// enforced behavior. Empty Origin allowlist is default-DENY — any request
// carrying an Origin header is 403; requests without one still pass
// (non-browser clients send none).
func TestC11EmptyOriginDefaultDeny(t *testing.T) {
	okH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	ids := []config.APIIdentity{{Name: "op", KeyEnv: "X", MaxTier: 2}}
	h := Auth(ids, map[string]string{"op": "good-key"}, nil, config.Limits{}, okH)

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer good-key")
	req.Header.Set("Origin", "https://evil.example")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("empty allowlist + Origin header must 403 (default-deny): got %d", rr.Code)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req2.Header.Set("Authorization", "Bearer good-key")
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusNoContent {
		t.Fatalf("no-Origin request must pass: got %d", rr2.Code)
	}
}

// C11 closed: per-identity token bucket — sustained requests beyond the
// burst are 429.
func TestC11RateLimitEnforced(t *testing.T) {
	okH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	ids := []config.APIIdentity{{Name: "op", KeyEnv: "X", MaxTier: 2}}
	limits := config.Limits{MaxSessions: 64, RatePerMinute: 30, RateBurst: 3}
	h := Auth(ids, map[string]string{"op": "good-key"}, nil, limits, okH)
	saw429 := false
	for i := 0; i < 50; i++ {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer good-key")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code == http.StatusTooManyRequests {
			saw429 = true
			break
		}
		if rr.Code != http.StatusNoContent {
			t.Fatalf("request %d: unexpected %d", i, rr.Code)
		}
	}
	if !saw429 {
		t.Fatal("burst of 50 with burst=3 produced no 429 - rate limit not enforced")
	}
}

// C11 closed: per-identity concurrent-request (session) cap — a held
// request blocks the second at 429, released slots admit again.
func TestC11SessionCapEnforced(t *testing.T) {
	held := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	okH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(held) })
		<-release
		w.WriteHeader(204)
	})
	ids := []config.APIIdentity{{Name: "op", KeyEnv: "X", MaxTier: 2}}
	limits := config.Limits{MaxSessions: 1, RatePerMinute: 100000, RateBurst: 100000}
	h := Auth(ids, map[string]string{"op": "good-key"}, nil, limits, okH)

	go func() {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer good-key")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-held

	req2 := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req2.Header.Set("Authorization", "Bearer good-key")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req2)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("second concurrent request must 429 (cap 1): got %d", rr.Code)
	}
	close(release)
}
