package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/transport"
)

type httpListener struct {
	mcp    *mcp.Server
	mu     sync.Mutex
	hs     *http.Server
	addr   string
	bound  string // actual bound address (port 0 → real port)
	auth   *transport.Authenticator
	certs  *certSource
	done   chan struct{}
	closed bool
	// keyHash is sha256(key) per identity of the applied auth; authFP
	// covers identities, origins and limits too (B0.2 change detection).
	keyHash map[string]string
	authFP  string
	// onAuthEvent receives transport events for the audit (B0.8).
	onAuthEvent func(transport.AuthEvent)
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fbmcp: "+format+"\n", args...)
}

func secretsFrom(cfg *config.Config) (map[string]string, error) {
	secrets := map[string]string{}
	for _, id := range cfg.Identities {
		v, err := config.SecretFromEnv(id.KeyEnv)
		if err != nil {
			return nil, fmt.Errorf("identity %s: %w", id.Name, err)
		}
		secrets[id.Name] = v
	}
	return secrets, nil
}

// secretsPartial resolves every identity key it can. An identity whose key
// no longer resolves is left out (its key stops working: fail closed) and
// reported. Used on reload; start-up uses secretsFrom (all or nothing).
func secretsPartial(cfg *config.Config) (map[string]string, []error) {
	secrets := map[string]string{}
	var errs []error
	for _, id := range cfg.Identities {
		v, err := config.SecretFromEnv(id.KeyEnv)
		if err != nil {
			errs = append(errs, fmt.Errorf("identity %s: %w", id.Name, err))
			continue
		}
		secrets[id.Name] = v
	}
	return secrets, errs
}

func hashKeys(secrets map[string]string) map[string]string {
	out := map[string]string{}
	for n, v := range secrets {
		h := sha256.Sum256([]byte(v))
		out[n] = hex.EncodeToString(h[:])
	}
	return out
}

func authFingerprint(cfg *config.Config, keyHash map[string]string) string {
	names := make([]string, 0, len(keyHash))
	for n := range keyHash {
		names = append(names, n)
	}
	sort.Strings(names)
	b, _ := json.Marshal(struct {
		IDs     []config.APIIdentity
		Origins []string
		Limits  config.Limits
		Names   []string
		Keys    map[string]string
	}{cfg.Identities, cfg.AllowedOrigins, cfg.Limits, names, keyHash})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ReplaceAuth applies the identities, origins and limits of cfg (reload).
func (l *httpListener) ReplaceAuth(cfg *config.Config) error {
	_, err := l.RefreshAuth(cfg)
	return err
}

// RefreshAuth resolves every identity key again (env → file → keyring) and
// applies keys, origins and limits when anything changed (phase 12 B0.2:
// a rotated key applies even when fbmcp.yaml did not change). It returns
// the identities whose key changed (added, removed or new value). A key
// that no longer resolves is dropped and reported as an error.
func (l *httpListener) RefreshAuth(cfg *config.Config) ([]string, error) {
	secrets, errs := secretsPartial(cfg)
	kh := hashKeys(secrets)
	fp := authFingerprint(cfg, kh)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.auth == nil {
		return nil, joinErrs(errs) // stdio-only; auth applies when HTTP starts
	}
	if fp == l.authFP {
		return nil, joinErrs(errs)
	}
	var changed []string
	for n, h := range kh {
		if l.keyHash[n] != h {
			changed = append(changed, n)
		}
	}
	for n := range l.keyHash {
		if _, ok := kh[n]; !ok {
			changed = append(changed, n)
		}
	}
	sort.Strings(changed)
	l.auth.SetLimits(cfg.Limits, cfg.Identities)
	l.auth.Replace(cfg.Identities, secrets, cfg.AllowedOrigins)
	l.keyHash, l.authFP = kh, fp
	return changed, joinErrs(errs)
}

func joinErrs(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	parts := make([]string, len(errs))
	for i, e := range errs {
		parts[i] = e.Error()
	}
	return fmt.Errorf("%s", strings.Join(parts, "; "))
}

// Start binds the listener synchronously (phase 12 B0.3): a bad cert or key,
// a busy port or a missing key is returned here, so the kernel exits
// non-zero at start-up instead of running with no port.
func (l *httpListener) Start(cfg *config.Config) error {
	if strings.TrimSpace(cfg.Listen) == "" {
		return l.Stop(context.Background())
	}
	if err := transport.CheckRemote(cfg.Listen, cfg.TLS.Cert, cfg.TLS.Key, len(cfg.Identities), len(cfg.AllowedOrigins)); err != nil {
		return err
	}
	secrets, err := secretsFrom(cfg)
	if err != nil {
		return err
	}
	l.mu.Lock()
	closed := l.closed
	l.mu.Unlock()
	if closed {
		return fmt.Errorf("http listener closed")
	}
	certs, err := newCertSource(cfg.TLS.Cert, cfg.TLS.Key)
	if err != nil {
		return err
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return fmt.Errorf("http listener closed")
	}
	// Same address: the old socket must go first. A new address binds
	// first, so a failed bind keeps the old listener up.
	if l.hs != nil && l.addr == cfg.Listen {
		old := l.hs
		l.hs = nil
		l.mu.Unlock()
		shutdown(old)
		l.mu.Lock()
	}
	l.mu.Unlock()
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Listen, err)
	}

	lim := cfg.Limits.OrDefault()
	auth := transport.NewAuthenticator(cfg.Identities, secrets, cfg.AllowedOrigins, cfg.Limits)
	if l.onAuthEvent != nil {
		auth.SetEventHook(l.onAuthEvent)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", transport.Healthz)
	stream := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return l.mcp },
		&mcp.StreamableHTTPOptions{SessionTimeout: time.Duration(lim.SessionIdleMinutes) * time.Minute})
	sse := mcp.NewSSEHandler(func(r *http.Request) *mcp.Server { return l.mcp }, nil)
	mux.Handle("/mcp", auth.Handler(stream))
	mux.Handle("/sse", auth.Handler(sse))
	s := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No ReadTimeout/WriteTimeout: /sse holds a long-lived streaming
		// response and the streamable handler long-polls; a whole-request
		// deadline would sever healthy sessions. IdleTimeout only reaps
		// keep-alive connections BETWEEN requests, so it is stream-safe.
		IdleTimeout:    2 * time.Minute,
		MaxHeaderBytes: 64 << 10,
		// GetCertificate re-reads a renewed pair at the same path (B0.6).
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: certs.Get},
	}
	l.mu.Lock()
	old := l.hs
	l.hs, l.addr, l.bound, l.auth, l.certs = s, cfg.Listen, ln.Addr().String(), auth, certs
	l.keyHash = hashKeys(secrets)
	l.authFP = authFingerprint(cfg, l.keyHash)
	l.mu.Unlock()
	if old != nil {
		shutdown(old)
	}
	go func() {
		if err := s.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed {
			logf("http: %v", err)
		}
	}()
	logf("listening on https://%s (/mcp, /sse, /healthz)", ln.Addr().String())
	return nil
}

func shutdown(s *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.Shutdown(ctx)
}

func (l *httpListener) Stop(ctx context.Context) error {
	l.mu.Lock()
	s := l.hs
	l.hs = nil
	l.addr = ""
	l.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.Shutdown(ctx)
}

// Close stops HTTP and unblocks Wait (process exit). Reload uses Stop so attach stays up.
func (l *httpListener) Close(ctx context.Context) error {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		if l.done == nil {
			l.done = make(chan struct{})
		}
		close(l.done)
	}
	s := l.hs
	l.hs = nil
	l.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.Shutdown(ctx)
}

// Replace applies a reloaded config. A new address restarts the listener.
// The same address keeps the socket: new TLS paths swap the certificate
// source (a bad new pair keeps the old one), then auth is refreshed.
func (l *httpListener) Replace(old, new *config.Config) error {
	newL := strings.TrimSpace(new.Listen)
	if newL == "" {
		return l.Stop(context.Background())
	}
	l.mu.Lock()
	running, sameAddr, certs := l.hs != nil, l.addr == newL, l.certs
	l.mu.Unlock()
	if !running || !sameAddr {
		return l.Start(new)
	}
	if old.TLS.Cert != new.TLS.Cert || old.TLS.Key != new.TLS.Key {
		if err := certs.SetPaths(new.TLS.Cert, new.TLS.Key); err != nil {
			return fmt.Errorf("tls: %w (the old certificate stays in use)", err)
		}
	}
	return l.ReplaceAuth(new)
}

func (l *httpListener) Wait() error {
	l.mu.Lock()
	if l.done == nil {
		l.done = make(chan struct{})
	}
	d := l.done
	l.mu.Unlock()
	<-d
	return nil
}

// certSource serves the TLS pair and loads it again when the files change
// (phase 12 B0.6). A broken new pair keeps the old one and is logged once.
type certSource struct {
	mu                sync.Mutex
	certPath, keyPath string
	cert              *tls.Certificate
	stamp             string
	checked           time.Time
	lastErr           string
	interval          time.Duration
}

func newCertSource(certPath, keyPath string) (*certSource, error) {
	c := &certSource{interval: 2 * time.Second}
	if err := c.SetPaths(certPath, keyPath); err != nil {
		return nil, err
	}
	return c, nil
}

func fileStamp(paths ...string) string {
	var b strings.Builder
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			b.WriteString("missing;")
			continue
		}
		fmt.Fprintf(&b, "%d/%d;", fi.ModTime().UnixNano(), fi.Size())
	}
	return b.String()
}

// SetPaths loads a new pair; on error the current pair stays.
func (c *certSource) SetPaths(certPath, keyPath string) error {
	stamp := fileStamp(certPath, keyPath)
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return fmt.Errorf("load TLS pair (cert %s, key %s): %w", certPath, keyPath, err)
	}
	c.mu.Lock()
	c.certPath, c.keyPath, c.cert, c.stamp, c.checked, c.lastErr = certPath, keyPath, &pair, stamp, time.Now(), ""
	c.mu.Unlock()
	return nil
}

// Get is tls.Config.GetCertificate.
func (c *certSource) Get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.checked) >= c.interval {
		c.checked = time.Now()
		if st := fileStamp(c.certPath, c.keyPath); st != c.stamp {
			pair, err := tls.LoadX509KeyPair(c.certPath, c.keyPath)
			if err != nil {
				if msg := err.Error(); msg != c.lastErr {
					c.lastErr = msg
					logf("tls: changed certificate not loaded, the old one stays in use: %v", err)
				}
			} else {
				c.cert, c.stamp, c.lastErr = &pair, st, ""
				logf("tls: certificate reloaded from %s", c.certPath)
			}
		}
	}
	return c.cert, nil
}
