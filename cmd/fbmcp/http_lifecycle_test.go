package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/config"
)

// P2.7 (test_plan): httpListener lifecycle — the guard paths are testable
// without TLS material; the full TLS listener is exercised by the live
// kernel (fbmcp.dev.yaml remote profile) and the C3 tests.

func TestStartEmptyListenIsStop(t *testing.T) {
	l := &httpListener{}
	if err := l.Start(&config.Config{}); err != nil {
		t.Fatalf("empty listen: %v", err)
	}
	if l.hs != nil {
		t.Fatal("no listener expected for empty listen")
	}
}

func TestStartRejectsLoopbackBind(t *testing.T) {
	l := &httpListener{}
	err := l.Start(&config.Config{Listen: "127.0.0.1:8443"})
	if err == nil || !strings.Contains(err.Error(), "non-localhost bind") {
		t.Fatalf("loopback bind: %v", err)
	}
}

func TestStartRequiresTLSAndIdentitiesAndOrigins(t *testing.T) {
	l := &httpListener{}
	cfg := &config.Config{Listen: "0.0.0.0:8443"}
	err := l.Start(cfg)
	if err == nil || !strings.Contains(err.Error(), "TLS cert and key") {
		t.Fatalf("no TLS: %v", err)
	}
	cfg.TLS = config.TLS{Cert: "c.pem", Key: "k.pem"}
	err = l.Start(cfg)
	if err == nil || !strings.Contains(err.Error(), "at least one identity") {
		t.Fatalf("no identities: %v", err)
	}
	cfg.Identities = []config.APIIdentity{{Name: "op", KeyEnv: "FBMCP_TEST_KEY", MaxTier: 1}}
	t.Setenv("FBMCP_TEST_KEY", "sekrit")
	err = l.Start(cfg)
	if err == nil || !strings.Contains(err.Error(), "allowed_origins") {
		t.Fatalf("no origins: %v", err)
	}
}

func TestStartMissingIdentitySecretFails(t *testing.T) {
	l := &httpListener{}
	cfg := &config.Config{
		Listen:         "0.0.0.0:8443",
		TLS:            config.TLS{Cert: "c.pem", Key: "k.pem"},
		Identities:     []config.APIIdentity{{Name: "op", KeyEnv: "FBMCP_MISSING_KEY_XYZ"}},
		AllowedOrigins: []string{"https://x.example"},
	}
	err := l.Start(cfg)
	if err == nil || !strings.Contains(err.Error(), "identity op") {
		t.Fatalf("missing secret: %v", err)
	}
}

func TestStartAfterCloseRefuses(t *testing.T) {
	l := &httpListener{}
	if err := l.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// config must clear CheckRemote to reach the closed guard
	cfg := &config.Config{
		Listen:         "0.0.0.0:8443",
		TLS:            config.TLS{Cert: "c.pem", Key: "k.pem"},
		Identities:     []config.APIIdentity{{Name: "op", KeyEnv: "FBMCP_TEST_KEY3"}},
		AllowedOrigins: []string{"https://x.example"},
	}
	t.Setenv("FBMCP_TEST_KEY3", "k")
	err := l.Start(cfg)
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("start after close: %v", err)
	}
}

func TestWaitUnblocksOnClose(t *testing.T) {
	l := &httpListener{}
	done := make(chan error, 1)
	go func() { done <- l.Wait() }()
	select {
	case <-done:
		t.Fatal("Wait returned before Close")
	case <-time.After(50 * time.Millisecond):
	}
	if err := l.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wait did not unblock on Close")
	}
}

func TestReplaceRoutesToStopAuthOrStart(t *testing.T) {
	l := &httpListener{}
	old := &config.Config{}
	// empty new listen → Stop (no-op on stdio-only listener)
	if err := l.Replace(old, &config.Config{}); err != nil {
		t.Fatalf("replace to empty: %v", err)
	}
	// same (empty) listen → ReplaceAuth → stdio-only no-op
	new := &config.Config{Identities: []config.APIIdentity{{Name: "op", KeyEnv: "FBMCP_TEST_KEY2"}}}
	t.Setenv("FBMCP_TEST_KEY2", "k")
	if err := l.Replace(old, new); err != nil {
		t.Fatalf("replace auth-only: %v", err)
	}
	// non-loopback new listen without TLS → Start fails via CheckRemote
	if err := l.Replace(old, &config.Config{Listen: "0.0.0.0:8443"}); err == nil {
		t.Fatal("replace to remote without TLS must fail")
	}
}
