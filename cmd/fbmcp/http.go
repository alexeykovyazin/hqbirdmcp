package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
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
	auth   *transport.Authenticator
	done   chan struct{}
	closed bool
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

func (l *httpListener) ReplaceAuth(cfg *config.Config) error {
	secrets, err := secretsFrom(cfg)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.auth == nil {
		return nil // stdio-only; auth applies when HTTP starts
	}
	l.auth.Replace(cfg.Identities, secrets, nil)
	return nil
}

func (l *httpListener) Start(cfg *config.Config) error {
	if strings.TrimSpace(cfg.Listen) == "" {
		return l.Stop(context.Background())
	}
	if err := transport.CheckRemote(cfg.Listen, cfg.TLS.Cert, cfg.TLS.Key, len(cfg.Identities)); err != nil {
		return err
	}
	secrets, err := secretsFrom(cfg)
	if err != nil {
		return err
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return fmt.Errorf("http listener closed")
	}
	if l.hs != nil {
		old := l.hs
		l.hs = nil
		l.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = old.Shutdown(ctx)
		cancel()
		l.mu.Lock()
	}
	auth := transport.NewAuthenticator(cfg.Identities, secrets, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", transport.Healthz)
	stream := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return l.mcp }, nil)
	sse := mcp.NewSSEHandler(func(r *http.Request) *mcp.Server { return l.mcp }, nil)
	mux.Handle("/mcp", auth.Handler(stream))
	mux.Handle("/sse", auth.Handler(sse))
	s := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
	l.hs = s
	l.auth = auth
	l.mu.Unlock()
	go func() {
		err := s.ListenAndServeTLS(cfg.TLS.Cert, cfg.TLS.Key)
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "fbmcp: http: %v\n", err)
		}
	}()
	return nil
}

func (l *httpListener) Stop(ctx context.Context) error {
	l.mu.Lock()
	s := l.hs
	l.hs = nil
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

func (l *httpListener) Replace(old, new *config.Config) error {
	oldL := strings.TrimSpace(old.Listen)
	newL := strings.TrimSpace(new.Listen)
	tlsChanged := old.TLS.Cert != new.TLS.Cert || old.TLS.Key != new.TLS.Key
	if newL == "" {
		return l.Stop(context.Background())
	}
	if oldL == newL && !tlsChanged {
		return l.ReplaceAuth(new)
	}
	return l.Start(new)
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
