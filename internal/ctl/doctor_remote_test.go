package ctl

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/secrets"
)

func TestDoctorListenerAndPin(t *testing.T) {
	// The listener runs in this test binary (ctl.test), not in fbmcp.
	defer func(old string) { listenerProcess = old }(listenerProcess)
	listenerProcess = "ctl.test"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte("ok\n"))
		}
	}))
	defer srv.Close()
	h := sha256.Sum256(srv.Certificate().RawSubjectPublicKeyInfo)
	pin := "sha256:" + hex.EncodeToString(h[:])
	cfg := &config.Config{Listen: srv.Listener.Addr().String()}
	d := &doctorOut{}
	checkListener(d, cfg, pin)
	if d.fails != 0 || d.warns != 0 {
		t.Fatalf("healthy listener: %d fails %d warns", d.fails, d.warns)
	}
	d = &doctorOut{}
	checkListener(d, cfg, "sha256:00")
	if d.warns != 1 {
		t.Fatalf("other served key must warn: %d warns", d.warns)
	}
	srv.Close()
	d = &doctorOut{}
	checkListener(d, cfg, pin)
	if d.fails == 0 {
		t.Fatal("closed port must fail")
	}
}

func TestDoctorIdentityWarnings(t *testing.T) {
	dir := t.TempDir()
	old := secrets.Dir()
	secrets.SetDir(dir)
	defer secrets.SetDir(old)
	os.Unsetenv("FBMCP_DOC_A")
	os.Unsetenv("FBMCP_DOC_MISSING")
	secrets.WriteFile(dir, "FBMCP_DOC_A", "v")
	t.Setenv("FBMCP_DOC_ENV", "v")
	cfg := &config.Config{Identities: []config.APIIdentity{
		{Name: "a", KeyEnv: "FBMCP_DOC_A"},                                                   // ok, file
		{Name: "e", KeyEnv: "FBMCP_DOC_ENV"},                                                 // warn: env
		{Name: "bot", KeyEnv: "FBMCP_DOC_A", Profile: "autonomous", MaxTier: config.Tier(1)}, // warn tier + no allow_from
	}}
	d := &doctorOut{}
	checkIdentities(d, cfg, nil)
	if d.fails != 0 || d.warns != 3 {
		t.Fatalf("got %d fails %d warns, want 0 / 3", d.fails, d.warns)
	}
	cfg.Identities = []config.APIIdentity{{Name: "m", KeyEnv: "FBMCP_DOC_MISSING"}}
	d = &doctorOut{}
	checkIdentities(d, cfg, nil)
	if d.fails != 1 {
		t.Fatal("missing key must fail")
	}
}

// A key the service gets from its unit/registry environment is not a FAIL
// (doctor runs in another shell) but a WARN: it wins over the secrets dir.
func TestDoctorServiceEnvKey(t *testing.T) {
	names := envNames([]string{"# comment", "FBMCP_KEY_ADMIN=x", "export FBMCP_B = y", "", "bad line", "=v"})
	if !names["FBMCP_KEY_ADMIN"] || !names["FBMCP_B"] || len(names) != 2 {
		t.Fatalf("envNames = %v", names)
	}
	os.Unsetenv("FBMCP_DOC_SVC")
	cfg := &config.Config{Identities: []config.APIIdentity{{Name: "admin", KeyEnv: "FBMCP_DOC_SVC"}}}
	d := &doctorOut{}
	checkIdentities(d, cfg, map[string]bool{"FBMCP_DOC_SVC": true})
	if d.fails != 0 || d.warns != 1 {
		t.Fatalf("got %d fails %d warns, want 0 / 1", d.fails, d.warns)
	}
}
