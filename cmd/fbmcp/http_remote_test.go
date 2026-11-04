package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/secrets"
)

// Phase 12 B0: the real TLS listener (bind, cert reload, key rotation).

// writePair writes a self-signed ECDSA pair for 127.0.0.1 and returns the
// certificate.
func writePair(t *testing.T, certPath, keyPath string, serial int64) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: "fbmcp-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c
}

type remoteFixture struct {
	cfg      *config.Config
	l        *httpListener
	pool     *x509.CertPool
	certPath string
	keyPath  string
}

// newRemote starts a real listener on 0.0.0.0:0 with one identity whose key
// is in a secrets folder file.
func newRemote(t *testing.T, key string) *remoteFixture {
	t.Helper()
	dir := t.TempDir()
	sdir := filepath.Join(dir, "secrets")
	os.Mkdir(sdir, 0o700)
	old := secrets.Dir()
	secrets.SetDir(sdir)
	t.Cleanup(func() { secrets.SetDir(old) })
	os.Unsetenv("FBMCP_P12_TEST_KEY")
	if err := secrets.WriteFile(sdir, "FBMCP_P12_TEST_KEY", key); err != nil {
		t.Fatal(err)
	}
	f := &remoteFixture{certPath: filepath.Join(dir, "cert.pem"), keyPath: filepath.Join(dir, "key.pem"), pool: x509.NewCertPool()}
	f.pool.AddCert(writePair(t, f.certPath, f.keyPath, 1))
	f.cfg = &config.Config{
		Listen:         "0.0.0.0:0",
		TLS:            config.TLS{Cert: f.certPath, Key: f.keyPath},
		Identities:     []config.APIIdentity{{Name: "op", KeyEnv: "FBMCP_P12_TEST_KEY", MaxTier: config.Tier(1)}},
		AllowedOrigins: []string{"https://console.example"},
		Secrets:        config.Secrets{Dir: sdir},
	}
	f.l = &httpListener{mcp: mcp.NewServer(&mcp.Implementation{Name: "p12-test", Version: "0"}, nil)}
	if err := f.l.Start(f.cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { f.l.Close(context.Background()) })
	return f
}

func (f *remoteFixture) url(t *testing.T, path string) string {
	_, port, err := net.SplitHostPort(f.l.bound)
	if err != nil {
		t.Fatal(err)
	}
	return "https://127.0.0.1:" + port + path
}

// client makes a new TLS connection per request (cert reload is per handshake).
func (f *remoteFixture) client() *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: f.pool}, DisableKeepAlives: true}}
}

func (f *remoteFixture) initialize(t *testing.T, key string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", f.url(t, "/mcp"), strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := f.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestRemoteListenerServesTLS(t *testing.T) {
	f := newRemote(t, "key-one")
	resp, err := f.client().Get(f.url(t, "/healthz"))
	if err != nil {
		t.Fatalf("healthz over TLS: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}
	if r := f.initialize(t, "wrong"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad key: %d", r.StatusCode)
	}
	r := f.initialize(t, "key-one")
	if r.StatusCode != 200 || r.Header.Get("Mcp-Session-Id") == "" {
		t.Fatalf("initialize: %d session=%q", r.StatusCode, r.Header.Get("Mcp-Session-Id"))
	}
}

// B0.3: start-up errors come back from Start (non-zero exit), not a log line.
func TestRemoteStartErrorsAreSynchronous(t *testing.T) {
	f := newRemote(t, "k")
	busy, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	_, port, _ := net.SplitHostPort(busy.Addr().String())
	cfg := *f.cfg
	cfg.Listen = "0.0.0.0:" + port
	l2 := &httpListener{mcp: f.l.mcp}
	if err := l2.Start(&cfg); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("busy port: %v", err)
	}
	cfg.Listen = "0.0.0.0:0"
	cfg.TLS.Key = cfg.TLS.Cert // not a key
	if err := l2.Start(&cfg); err == nil || !strings.Contains(err.Error(), "TLS pair") {
		t.Fatalf("bad key file: %v", err)
	}
	l2.Close(context.Background())
}

// B0.6: a renewed pair at the same path is served without a restart.
func TestRemoteCertReloadSamePath(t *testing.T) {
	f := newRemote(t, "k")
	f.l.certs.mu.Lock()
	f.l.certs.interval = 0
	f.l.certs.mu.Unlock()
	serial := func() int64 {
		resp, err := f.client().Get(f.url(t, "/healthz"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.TLS.PeerCertificates[0].SerialNumber.Int64()
	}
	if s := serial(); s != 1 {
		t.Fatalf("serial %d, want 1", s)
	}
	f.pool.AddCert(writePair(t, f.certPath, f.keyPath, 2))
	later := time.Now().Add(time.Minute)
	os.Chtimes(f.certPath, later, later)
	if s := serial(); s != 2 {
		t.Fatalf("after renewal: serial %d, want 2", s)
	}
	// A broken file keeps the old pair.
	os.WriteFile(f.keyPath, []byte("broken"), 0o600)
	later = later.Add(time.Minute)
	os.Chtimes(f.keyPath, later, later)
	if s := serial(); s != 2 {
		t.Fatalf("broken renewal: serial %d, want the old 2", s)
	}
}

// B0.2: a rotated key file applies on RefreshAuth with no fbmcp.yaml change.
func TestRemoteKeyRotationFromFile(t *testing.T) {
	f := newRemote(t, "key-one")
	if r := f.initialize(t, "key-one"); r.StatusCode != 200 {
		t.Fatalf("old key before rotation: %d", r.StatusCode)
	}
	if changed, err := f.l.RefreshAuth(f.cfg); err != nil || len(changed) != 0 {
		t.Fatalf("refresh with no change: %v %v", changed, err)
	}
	if err := secrets.WriteFile(f.cfg.Secrets.Dir, "FBMCP_P12_TEST_KEY", "key-two"); err != nil {
		t.Fatal(err)
	}
	changed, err := f.l.RefreshAuth(f.cfg)
	if err != nil || len(changed) != 1 || changed[0] != "op" {
		t.Fatalf("refresh after rotation: %v %v", changed, err)
	}
	if r := f.initialize(t, "key-one"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old key after rotation: %d", r.StatusCode)
	}
	if r := f.initialize(t, "key-two"); r.StatusCode != 200 {
		t.Fatalf("new key after rotation: %d", r.StatusCode)
	}
	// The key file is removed: fail closed, report the error.
	secrets.DropFile(f.cfg.Secrets.Dir, "FBMCP_P12_TEST_KEY")
	if _, err := f.l.RefreshAuth(f.cfg); err == nil {
		t.Fatal("missing key not reported")
	}
	if r := f.initialize(t, "key-two"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("removed key still works: %d", r.StatusCode)
	}
}

// B0.4: serve mode returns when ctx is cancelled (Windows service Stop).
func TestServeModeReturnsOnCancel(t *testing.T) {
	cfg := &config.Config{State: config.State{Dir: t.TempDir()}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil), config.NewHandle(cfg), &gatedTools{}, true)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after ctx cancel")
	}
}
