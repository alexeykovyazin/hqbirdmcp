package ctl

import (
	"crypto/tls"
	"github.com/aleks/fbmcp/internal/config"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// Phase 12 B1.
func TestSelfsignRenewKeepsPin(t *testing.T) {
	dir := t.TempDir()
	cp, kp := filepath.Join(dir, tlsCertName), filepath.Join(dir, tlsKeyName)
	ips := []net.IP{net.ParseIP("10.0.0.5")}
	c1, err := selfsign(cp, kp, []string{"srv1", "srv1.corp.local"}, ips, 825, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.LoadX509KeyPair(cp, kp); err != nil {
		t.Fatalf("the kernel cannot load the pair: %v", err)
	}
	if c1.Subject.CommonName != "srv1" || len(c1.DNSNames) != 2 || !c1.IPAddresses[0].Equal(ips[0]) {
		t.Fatalf("SANs: %v %v", c1.DNSNames, c1.IPAddresses)
	}
	if _, err := selfsign(cp, kp, []string{"srv1"}, nil, 825, false, false); err == nil {
		t.Fatal("overwrite without --force/--renew accepted")
	}
	c2, err := selfsign(cp, kp, []string{"srv1"}, nil, 30, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if spkiPin(c2) != spkiPin(c1) || c2.SerialNumber.Cmp(c1.SerialNumber) == 0 {
		t.Fatalf("--renew: pin %s→%s, serial same=%v", spkiPin(c1), spkiPin(c2), c2.SerialNumber.Cmp(c1.SerialNumber) == 0)
	}
	if _, err := tls.LoadX509KeyPair(cp, kp); err != nil {
		t.Fatalf("renewed pair: %v", err)
	}
	c3, err := selfsign(cp, kp, []string{"srv1"}, nil, 825, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if spkiPin(c3) == spkiPin(c1) {
		t.Fatal("--force must make a new key (new pin)")
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 2 {
		t.Fatalf("temp files left: %d entries", len(ents))
	}
}

func TestDefaultKeyEnvAndValue(t *testing.T) {
	for in, want := range map[string]string{"alice": "FBMCP_KEY_ALICE", "openclaw-1": "FBMCP_KEY_OPENCLAW_1", "a.b c": "FBMCP_KEY_A_B_C"} {
		if got := defaultKeyEnv(in); got != want {
			t.Errorf("%s → %s, want %s", in, got, want)
		}
	}
	a, _ := newKeyValue()
	b, _ := newKeyValue()
	if len(a) != 43 || a == b {
		t.Fatalf("key value len %d (want 43), distinct=%v", len(a), a != b)
	}
}

// B2: new stores in the file source, refuses a second new, rotate refuses
// when env would shadow it.
func TestAPIKeyNewRotateFileSource(t *testing.T) {
	dir := t.TempDir()
	os.Unsetenv("FBMCP_KEY_LAB")
	if code := apikeyNew([]string{"lab", "--max-tier", "1", "--secrets-dir", dir, "--no-show"}); code != 0 {
		t.Fatalf("new: exit %d", code)
	}
	b1, err := os.ReadFile(filepath.Join(dir, "FBMCP_KEY_LAB"))
	if err != nil {
		t.Fatal(err)
	}
	if code := apikeyNew([]string{"lab", "--secrets-dir", dir, "--no-show"}); code == 0 {
		t.Fatal("second new for the same name accepted")
	}
	if code := apikeyNew([]string{"bot", "--profile", "autonomous", "--allow-from", "10.0.0.0/33", "--secrets-dir", dir}); code != 2 {
		t.Fatalf("bad allow_from: exit %d, want 2", code)
	}
	if code := apikeyRotate([]string{"lab", "--secrets-dir", dir, "--no-show"}); code != 0 {
		t.Fatalf("rotate: exit %d", code)
	}
	b2, _ := os.ReadFile(filepath.Join(dir, "FBMCP_KEY_LAB"))
	if string(b1) == string(b2) {
		t.Fatal("rotate did not change the value")
	}
	t.Setenv("FBMCP_KEY_LAB", "x")
	if code := apikeyRotate([]string{"lab", "--secrets-dir", dir, "--no-show"}); code != 1 {
		t.Fatalf("rotate with env set: exit %d, want 1 (F2)", code)
	}
}

// apikey new --config adds the identity to fbmcp.yaml and stores the key in
// its secrets.dir, so "apikey new, then hqbird-install" turns remote mode on.
func TestAPIKeyNewConfig(t *testing.T) {
	root := t.TempDir()
	sec := filepath.Join(root, "secrets")
	cfgPath := filepath.Join(root, "fbmcp.yaml")
	body := "state:\n  dir: " + filepath.ToSlash(filepath.Join(root, "state")) + "\nsecrets:\n  dir: " + filepath.ToSlash(sec) + "\n" +
		"instances:\n  - id: fb5\n    addr: localhost:3055\n    bin_dir: C:/HQbird/Firebird50\n"
	if err := os.MkdirAll(sec, 0o700); err != nil { // hqbird-install made it
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("FBMCP_KEY_OPS")
	if code := apikeyNew([]string{"ops", "--max-tier", "2", "--config", cfgPath, "--no-show"}); code != 0 {
		t.Fatalf("new --config: exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(sec, "FBMCP_KEY_OPS")); err != nil {
		t.Fatalf("key not in secrets.dir: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Identities) != 1 || cfg.Identities[0].Name != "ops" || cfg.Identities[0].KeyEnv != "FBMCP_KEY_OPS" || cfg.Identities[0].EffectiveMaxTier() != 2 {
		t.Fatalf("identities = %+v", cfg.Identities)
	}
	if code := apikeyNew([]string{"ops", "--config", cfgPath, "--force", "--no-show"}); code != 1 {
		t.Fatalf("second new for the same identity: exit %d, want 1", code)
	}
}
