// Package transport hardens remote MCP HTTP/SSE (ADR-022). Stdio remains
// the default; remote mode refuses to start without bind≠localhost + TLS +
// at least one identity.
package transport

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/policy"
)

// MCPEntries are authenticated MCP surfaces (fuse completeness).
var MCPEntries = []string{"/mcp", "/sse"}

// CheckRemote enforces ADR-022 + E.1: remote mode requires non-localhost
// bind, TLS, >= 1 identity, and a non-empty Origin allowlist (default-deny —
// an empty allowlist would accept any Origin header). Empty listen means
// stdio-only (ok).
func CheckRemote(listen, cert, key string, nIdentities, nOrigins int) error {
	if strings.TrimSpace(listen) == "" {
		return nil
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if isLoopback(host) {
		return fmt.Errorf("remote mode requires a non-localhost bind (got %q) — ADR-022", listen)
	}
	if cert == "" || key == "" {
		return fmt.Errorf("remote mode requires TLS cert and key — ADR-022")
	}
	if nIdentities < 1 {
		return fmt.Errorf("remote mode requires at least one identity — ADR-022")
	}
	if nOrigins < 1 {
		return fmt.Errorf("remote mode requires a non-empty allowed_origins (default-deny, E.1) — an empty allowlist would accept any Origin header")
	}
	return nil
}

func isLoopback(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Auth wraps an MCP handler: Bearer token must match a configured identity,
// then per-identity rate/session guards apply (E.1). Origin default-deny:
// any Origin header not in the allowlist is 403; no Origin header passes
// (non-browser clients). X-Forwarded-For is ignored (untrusted by default).
func Auth(ids []config.APIIdentity, secrets map[string]string, origins []string, limits config.Limits, next http.Handler) http.Handler {
	a := NewAuthenticator(ids, secrets, origins, limits)
	return a.Handler(next)
}

// guard is the per-identity E.1 state: token bucket + in-flight cap.
type guard struct {
	limiter  *rate.Limiter
	mu       sync.Mutex
	inflight int
	max      int
}

func (g *guard) enter() string {
	if !g.limiter.Allow() {
		return "rate limit exceeded (per-identity token bucket)"
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inflight >= g.max {
		return "session cap reached (concurrent request limit)"
	}
	g.inflight++
	return ""
}

func (g *guard) leave() {
	g.mu.Lock()
	g.inflight--
	g.mu.Unlock()
}

// Authenticator is a swappable Bearer lookup (identities-only config reload).
type Authenticator struct {
	mu     sync.Mutex
	lookup map[string]policy.Identity
	allow  map[string]bool
	limits config.Limits
	guards map[string]*guard
}

func NewAuthenticator(ids []config.APIIdentity, secrets map[string]string, origins []string, limits config.Limits) *Authenticator {
	a := &Authenticator{limits: limits.OrDefault()}
	a.Replace(ids, secrets, origins)
	return a
}

func (a *Authenticator) Replace(ids []config.APIIdentity, secrets map[string]string, origins []string) {
	lookup := map[string]policy.Identity{}
	guards := map[string]*guard{}
	for _, id := range ids {
		sec := secrets[id.Name]
		if sec == "" {
			continue
		}
		lookup[sec] = identity.APIKey(id.Name, id.MaxTier)
		guards[id.Name] = &guard{
			limiter: rate.NewLimiter(rate.Every(time.Minute/time.Duration(a.limits.RatePerMinute)), a.limits.RateBurst),
			max:     a.limits.MaxSessions,
		}
	}
	allow := map[string]bool{}
	for _, o := range origins {
		allow[strings.ToLower(o)] = true
	}
	a.mu.Lock()
	a.lookup = lookup
	a.allow = allow
	a.guards = guards
	a.mu.Unlock()
}

func (a *Authenticator) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		lookup, allow := a.lookup, a.allow
		a.mu.Unlock()
		// E.1 default-deny: any Origin header must be allowlisted; requests
		// without an Origin header (non-browser clients) still pass.
		if orig := strings.ToLower(r.Header.Get("Origin")); orig != "" && !allow[orig] {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		tok := bearer(r.Header.Get("Authorization"))
		if tok == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="fbmcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var found policy.Identity
		ok := false
		for secret, id := range lookup {
			if subtle.ConstantTimeCompare([]byte(secret), []byte(tok)) == 1 {
				found, ok = id, true
				break
			}
		}
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if g := a.guards[found.Name]; g != nil {
			if reason := g.enter(); reason != "" {
				w.Header().Set("Retry-After", "2")
				http.Error(w, reason, http.StatusTooManyRequests)
				return
			}
			defer g.leave()
		}
		next.ServeHTTP(w, r.WithContext(identity.With(r.Context(), found)))
	})
}

func bearer(h string) string {
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

// Healthz is liveness only — no version, no registry (ADR-022 T3).
func Healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}
