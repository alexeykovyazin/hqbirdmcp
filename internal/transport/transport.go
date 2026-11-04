// Package transport hardens remote MCP HTTP/SSE (ADR-022). Stdio remains
// the default; remote mode refuses to start without bind≠localhost + TLS +
// at least one identity.
package transport

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/time/rate"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/policy"
)

// MCPEntries are authenticated MCP surfaces (fuse completeness).
var MCPEntries = []string{"/mcp", "/sse"}

// sessionHeader is the streamable HTTP session header (MCP 2025-03-26+).
const sessionHeader = "Mcp-Session-Id"

// maxPeekBytes bounds the body read that classifies a session-less POST
// (the SDK's own request limit is 4 MiB).
const maxPeekBytes = 4 << 20

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

// guard is the per-identity E.1 state: token bucket, in-flight call cap and
// held-stream cap (phase 12 B0.7: a GET stream does not take a call slot).
type guard struct {
	limits   config.Limits
	limiter  *rate.Limiter
	mu       sync.Mutex
	inflight int
	streams  int
}

func newGuard(l config.Limits) *guard {
	return &guard{
		limits:  l,
		limiter: rate.NewLimiter(rate.Every(time.Minute/time.Duration(l.RatePerMinute)), l.RateBurst),
	}
}

func (g *guard) enter(stream bool) string {
	if !g.limiter.Allow() {
		return "rate limit exceeded (per-identity token bucket)"
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if stream {
		if g.streams >= g.limits.MaxStreams {
			return "stream cap reached (held GET streams per identity)"
		}
		g.streams++
		return ""
	}
	if g.inflight >= g.limits.MaxSessions {
		return "session cap reached (concurrent request limit)"
	}
	g.inflight++
	return ""
}

func (g *guard) leave(stream bool) {
	g.mu.Lock()
	if stream {
		g.streams--
	} else {
		g.inflight--
	}
	g.mu.Unlock()
}

// keyEntry is one configured key: the identity it maps to and its
// source-address rule (B8 allow_from).
type keyEntry struct {
	id   policy.Identity
	nets []*net.IPNet
}

// AuthEvent is an audit-worthy transport event (phase 12 B0.8): a new MCP
// session, or a valid key refused by allow_from. Unknown keys (401) are not
// events — a scanner must not grow the audit log.
type AuthEvent struct {
	Kind       string // "session.open" | "auth.denied"
	Identity   string
	Profile    string
	RemoteAddr string
	Reason     string
	UserAgent  string
}

// Authenticator is a swappable Bearer lookup (identities-only config reload).
type Authenticator struct {
	mu       sync.Mutex
	lookup   map[string]keyEntry
	allow    map[string]bool
	limits   config.Limits
	guards   map[string]*guard
	sessions *sessionTracker
	onEvent  func(AuthEvent)
}

func NewAuthenticator(ids []config.APIIdentity, secrets map[string]string, origins []string, limits config.Limits) *Authenticator {
	a := &Authenticator{limits: limits.OrDefault(), sessions: newSessionTracker()}
	a.Replace(ids, secrets, origins)
	return a
}

// SetEventHook receives AuthEvents (audit wiring; nil = off).
func (a *Authenticator) SetEventHook(f func(AuthEvent)) {
	a.mu.Lock()
	a.onEvent = f
	a.mu.Unlock()
}

// SetLimits replaces the global limits block (reload) and rebuilds guards
// whose effective limits changed.
func (a *Authenticator) SetLimits(l config.Limits, ids []config.APIIdentity) {
	a.mu.Lock()
	a.limits = l.OrDefault()
	a.mu.Unlock()
	a.rebuildGuards(ids)
}

// Limits returns the global limits in force (with defaults).
func (a *Authenticator) Limits() config.Limits {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.limits
}

func (a *Authenticator) Replace(ids []config.APIIdentity, secrets map[string]string, origins []string) {
	lookup := map[string]keyEntry{}
	for _, id := range ids {
		sec := secrets[id.Name]
		if sec == "" {
			continue
		}
		lookup[sec] = keyEntry{id: identity.FromConfig(id), nets: id.AllowNets()}
	}
	allow := map[string]bool{}
	for _, o := range origins {
		allow[strings.ToLower(o)] = true
	}
	a.mu.Lock()
	a.lookup = lookup
	a.allow = allow
	a.mu.Unlock()
	a.rebuildGuards(ids)
}

// rebuildGuards keeps the guard (rate state, in-flight counts) of every
// identity whose effective limits did not change, so a key rotation does
// not reset the limits.
func (a *Authenticator) rebuildGuards(ids []config.APIIdentity) {
	a.mu.Lock()
	defer a.mu.Unlock()
	guards := map[string]*guard{}
	for _, id := range ids {
		l := id.EffectiveLimits(a.limits)
		if old := a.guards[id.Name]; old != nil && old.limits == l {
			guards[id.Name] = old
			continue
		}
		guards[id.Name] = newGuard(l)
	}
	a.guards = guards
}

// peerIP is the TCP peer address of r (never a forwarded header).
func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func netsContain(nets []*net.IPNet, ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func (a *Authenticator) Handler(next http.Handler) http.Handler {
	// The SDK binds each streamable session to TokenInfo.UserID, so a
	// session id is only usable with the key that created it.
	bound := auth.RequireBearerToken(func(ctx context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
		id, ok := identity.FromContext(ctx)
		if !ok {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: id.Name, Expiration: time.Now().Add(time.Hour)}, nil
	}, nil)(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		lookup, allow, onEvent, idle := a.lookup, a.allow, a.onEvent, time.Duration(a.limits.SessionIdleMinutes)*time.Minute
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
		var found keyEntry
		ok := false
		for secret, e := range lookup {
			if subtle.ConstantTimeCompare([]byte(secret), []byte(tok)) == 1 {
				found, ok = e, true
				break
			}
		}
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		peer := peerIP(r)
		if len(found.nets) > 0 && !netsContain(found.nets, peer) {
			if onEvent != nil {
				onEvent(AuthEvent{Kind: "auth.denied", Identity: found.id.Name, Profile: found.id.Profile, RemoteAddr: peer,
					Reason: "source address outside allow_from", UserAgent: userAgent(r)})
			}
			http.Error(w, "source address not allowed for this key", http.StatusForbidden)
			return
		}
		stream := r.Method == http.MethodGet
		a.mu.Lock()
		g := a.guards[found.id.Name]
		a.mu.Unlock()
		if g != nil {
			if reason := g.enter(stream); reason != "" {
				w.Header().Set("Retry-After", "2")
				http.Error(w, reason, http.StatusTooManyRequests)
				return
			}
			defer g.leave(stream)
		}

		// Session bookkeeping (B0.5) for the streamable endpoint.
		sid := r.Header.Get(sessionHeader)
		opening := false
		if r.URL.Path == "/mcp" {
			switch {
			case sid != "" && r.Method != http.MethodGet:
				a.sessions.touch(sid, found.id.Name)
			case sid == "" && r.Method == http.MethodPost:
				body, isInit, err := peekInitialize(r)
				if err != nil {
					http.Error(w, "request body: "+err.Error(), http.StatusBadRequest)
					return
				}
				r.Body = body
				if isInit {
					max := 0
					if g != nil {
						max = g.limits.MaxLiveSessions
					}
					if max > 0 && a.sessions.count(found.id.Name, idle) >= max {
						w.Header().Set("Retry-After", "60")
						http.Error(w, "live session cap reached for this key (close unused sessions or wait for the idle timeout)", http.StatusTooManyRequests)
						return
					}
					opening = true
				}
			}
		}

		id := found.id
		id.RemoteAddr = peer
		rec := &statusRecorder{ResponseWriter: w}
		bound.ServeHTTP(rec, r.WithContext(identity.With(r.Context(), id)))

		if r.URL.Path != "/mcp" {
			return
		}
		switch {
		case opening && rec.status < 400:
			if newSID := rec.Header().Get(sessionHeader); newSID != "" {
				a.sessions.add(newSID, found.id.Name)
				if onEvent != nil {
					onEvent(AuthEvent{Kind: "session.open", Identity: found.id.Name, Profile: found.id.Profile, RemoteAddr: peer, UserAgent: userAgent(r)})
				}
			}
		case sid != "" && (rec.status == http.StatusNotFound || (r.Method == http.MethodDelete && rec.status < 300)):
			a.sessions.remove(sid)
		}
	})
}

// LiveSessions reports the tracked live sessions per identity (doctor and
// tests).
func (a *Authenticator) LiveSessions() map[string]int {
	a.mu.Lock()
	idle := time.Duration(a.limits.SessionIdleMinutes) * time.Minute
	a.mu.Unlock()
	return a.sessions.counts(idle)
}

func userAgent(r *http.Request) string {
	ua := r.Header.Get("User-Agent")
	if len(ua) > 120 {
		ua = ua[:120]
	}
	return ua
}

// peekInitialize reads the body of a session-less POST and reports whether
// it is an initialize request. The returned body replays what was read.
func peekInitialize(r *http.Request) (io.ReadCloser, bool, error) {
	if r.Body == nil {
		return http.NoBody, false, nil
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, maxPeekBytes+1))
	if err != nil {
		return nil, false, err
	}
	rest := r.Body
	body := struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(buf), rest), rest}
	t := bytes.TrimSpace(buf)
	var m struct {
		Method string `json:"method"`
	}
	if len(t) > 0 && t[0] == '[' { // JSON-RPC batch (protocol 2025-03-26)
		var arr []struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(t, &arr) == nil {
			for _, x := range arr {
				if x.Method == "initialize" {
					return body, true, nil
				}
			}
		}
		return body, false, nil
	}
	if json.Unmarshal(t, &m) == nil && m.Method == "initialize" {
		return body, true, nil
	}
	return body, false, nil
}

// statusRecorder keeps the response status for session bookkeeping. It
// passes Flush through (streamable answers are SSE streams).
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

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
