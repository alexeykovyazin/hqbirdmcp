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

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/policy"
)

// MCPEntries are authenticated MCP surfaces (fuse completeness).
var MCPEntries = []string{"/mcp", "/sse"}

// CheckRemote enforces ADR-022. Empty listen means stdio-only (ok).
func CheckRemote(listen, cert, key string, nIdentities int) error {
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

// Auth wraps an MCP handler: Bearer token must match a configured identity.
// Origin allowlist: empty list allows any Origin; non-empty rejects others.
// X-Forwarded-For is ignored (untrusted by default).
func Auth(ids []config.APIIdentity, secrets map[string]string, origins []string, next http.Handler) http.Handler {
	a := NewAuthenticator(ids, secrets, origins)
	return a.Handler(next)
}

// Authenticator is a swappable Bearer lookup (identities-only config reload).
type Authenticator struct {
	mu     sync.Mutex
	lookup map[string]policy.Identity
	allow  map[string]bool
}

func NewAuthenticator(ids []config.APIIdentity, secrets map[string]string, origins []string) *Authenticator {
	a := &Authenticator{}
	a.Replace(ids, secrets, origins)
	return a
}

func (a *Authenticator) Replace(ids []config.APIIdentity, secrets map[string]string, origins []string) {
	lookup := map[string]policy.Identity{}
	for _, id := range ids {
		sec := secrets[id.Name]
		if sec == "" {
			continue
		}
		lookup[sec] = identity.APIKey(id.Name, id.MaxTier)
	}
	allow := map[string]bool{}
	for _, o := range origins {
		allow[strings.ToLower(o)] = true
	}
	a.mu.Lock()
	a.lookup = lookup
	a.allow = allow
	a.mu.Unlock()
}

func (a *Authenticator) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		lookup, allow := a.lookup, a.allow
		a.mu.Unlock()
		if len(allow) > 0 {
			orig := strings.ToLower(r.Header.Get("Origin"))
			if orig != "" && !allow[orig] {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
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
