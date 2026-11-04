package transport

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/identity"
)

func authReq(method, path, key, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer "+key)
	return r
}

// B0.7 (F5): a held GET stream does not take a call slot; streams have
// their own cap.
func TestGETStreamDoesNotTakeCallSlot(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			entered <- struct{}{}
			<-release
		}
		w.WriteHeader(200)
	})
	ids := []config.APIIdentity{{Name: "op", KeyEnv: "X"}}
	a := NewAuthenticator(ids, map[string]string{"op": "k"}, []string{"https://x"}, config.Limits{MaxSessions: 1, MaxStreams: 1, RatePerMinute: 6000, RateBurst: 100})
	srv := a.Handler(h)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); srv.ServeHTTP(httptest.NewRecorder(), authReq("GET", "/sse", "k", "")) }()
	<-entered

	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, authReq("POST", "/sse", "k", `{}`))
	if rr.Code != 200 {
		t.Fatalf("call with one held stream: %d (stream took the only call slot)", rr.Code)
	}
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, authReq("GET", "/sse", "k", ""))
	if rr.Code != http.StatusTooManyRequests || !strings.Contains(rr.Body.String(), "stream cap") {
		t.Fatalf("second stream: %d %q, want 429 stream cap", rr.Code, rr.Body.String())
	}
	close(release)
	wg.Wait()
}

// B8: allow_from uses the TCP peer; X-Forwarded-For is ignored; a refused
// address is 403 and an audit event.
func TestAllowFromUsesPeerAddress(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := identity.FromContext(r.Context())
		if id.RemoteAddr == "" {
			t.Error("RemoteAddr not set on the identity")
		}
		w.WriteHeader(204)
	})
	ids := []config.APIIdentity{{Name: "bot", KeyEnv: "X", Profile: "autonomous", AllowFrom: []string{"10.0.0.0/8", "192.168.1.21"}}}
	a := NewAuthenticator(ids, map[string]string{"bot": "k"}, []string{"https://x"}, config.Limits{})
	var events []AuthEvent
	a.SetEventHook(func(e AuthEvent) { events = append(events, e) })
	srv := a.Handler(ok)

	r := authReq("POST", "/sse", "k", "{}")
	r.RemoteAddr = "192.168.1.5:5000"
	r.Header.Set("X-Forwarded-For", "10.0.0.1")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, r)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("outside allow_from with forged XFF: %d, want 403", rr.Code)
	}
	if len(events) != 1 || events[0].Kind != "auth.denied" || events[0].RemoteAddr != "192.168.1.5" || events[0].Profile != "autonomous" {
		t.Fatalf("audit event: %+v", events)
	}
	for _, peer := range []string{"10.1.2.3:4000", "192.168.1.21:4000"} {
		r = authReq("POST", "/sse", "k", "{}")
		r.RemoteAddr = peer
		rr = httptest.NewRecorder()
		srv.ServeHTTP(rr, r)
		if rr.Code != 204 {
			t.Fatalf("inside allow_from (%s): %d", peer, rr.Code)
		}
	}
}

// B0.7 / B8: per-identity limits override the global block; the
// autonomous profile has its own defaults.
func TestPerIdentityLimits(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	ids := []config.APIIdentity{
		{Name: "slow", KeyEnv: "A", Limits: &config.Limits{RatePerMinute: 1, RateBurst: 1}},
		{Name: "fast", KeyEnv: "B"},
	}
	a := NewAuthenticator(ids, map[string]string{"slow": "ks", "fast": "kf"}, []string{"https://x"}, config.Limits{RatePerMinute: 6000, RateBurst: 50})
	srv := a.Handler(ok)
	codes := func(key string, n int) []int {
		var out []int
		for i := 0; i < n; i++ {
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, authReq("POST", "/sse", key, "{}"))
			out = append(out, rr.Code)
		}
		return out
	}
	if c := codes("ks", 2); c[0] != 204 || c[1] != 429 {
		t.Fatalf("slow identity: %v, want [204 429]", c)
	}
	for _, c := range codes("kf", 5) {
		if c != 204 {
			t.Fatalf("fast identity throttled by the other identity's limits: %d", c)
		}
	}
	got := config.APIIdentity{Profile: "autonomous"}.EffectiveLimits(config.Limits{})
	if got.RatePerMinute != 10 || got.RateBurst != 20 || got.MaxSessions != 2 {
		t.Fatalf("autonomous defaults: %+v", got)
	}
}

// B0.5: live sessions per key are capped; DELETE frees a slot; a
// session-less POST that is not initialize does not count.
func TestLiveSessionCap(t *testing.T) {
	n := 0
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(sessionHeader) == "" && strings.Contains(readAll(r), `"initialize"`) {
			n++
			w.Header().Set(sessionHeader, "s"+string(rune('0'+n)))
		}
		w.WriteHeader(200)
	})
	ids := []config.APIIdentity{{Name: "op", KeyEnv: "X", Limits: &config.Limits{MaxLiveSessions: 2}}}
	a := NewAuthenticator(ids, map[string]string{"op": "k"}, []string{"https://x"}, config.Limits{RatePerMinute: 6000, RateBurst: 100})
	srv := a.Handler(h)
	post := func(body, sid string) int {
		r := authReq("POST", "/mcp", "k", body)
		if sid != "" {
			r.Header.Set(sessionHeader, sid)
		}
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, r)
		return rr.Code
	}
	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	if post(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "") != 200 {
		t.Fatal("non-initialize POST")
	}
	if post(initBody, "") != 200 || post(initBody, "") != 200 {
		t.Fatal("first two sessions must open")
	}
	if c := post(initBody, ""); c != http.StatusTooManyRequests {
		t.Fatalf("third session: %d, want 429", c)
	}
	if got := a.LiveSessions()["op"]; got != 2 {
		t.Fatalf("live sessions = %d, want 2 (the non-initialize POST must not count)", got)
	}
	r := authReq("DELETE", "/mcp", "k", "")
	r.Header.Set(sessionHeader, "s1")
	srv.ServeHTTP(httptest.NewRecorder(), r)
	if c := post(initBody, ""); c != 200 {
		t.Fatalf("after DELETE: %d, want 200", c)
	}
}

func readAll(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	var b strings.Builder
	buf := make([]byte, 512)
	for {
		n, err := r.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return b.String()
		}
	}
}

func TestSessionTrackerExpiresIdle(t *testing.T) {
	tr := newSessionTracker()
	now := time.Unix(1000, 0)
	tr.now = func() time.Time { return now }
	tr.add("a", "op")
	tr.add("b", "op")
	now = now.Add(20 * time.Minute)
	tr.touch("b", "op")
	tr.touch("a", "intruder") // another identity cannot keep a session alive
	now = now.Add(15 * time.Minute)
	if c := tr.count("op", 30*time.Minute); c != 1 {
		t.Fatalf("after 35 min idle for a, 15 for b: %d live, want 1", c)
	}
}

// Rotation keeps the guard of an identity whose limits did not change.
func TestReplaceKeepsGuardState(t *testing.T) {
	ids := []config.APIIdentity{{Name: "op", KeyEnv: "X", Limits: &config.Limits{RatePerMinute: 1, RateBurst: 1}}}
	a := NewAuthenticator(ids, map[string]string{"op": "old"}, []string{"https://x"}, config.Limits{})
	srv := a.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, authReq("POST", "/sse", "old", "{}"))
	a.Replace(ids, map[string]string{"op": "new"}, []string{"https://x"})
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, authReq("POST", "/sse", "old", "{}"))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("old key after rotation: %d, want 401", rr.Code)
	}
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, authReq("POST", "/sse", "new", "{}"))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("new key right after rotation: %d, want 429 (bucket must survive a rotation)", rr.Code)
	}
}
