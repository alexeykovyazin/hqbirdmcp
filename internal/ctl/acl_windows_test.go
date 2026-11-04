//go:build windows

package ctl

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The virtual-account SID is computed, so the key can be granted before
// the service exists (value from `sc showsid fbmcp`).
func TestServiceSID(t *testing.T) {
	if got := serviceSID("fbmcp"); got != "S-1-5-80-3528931336-1215061719-3137965508-4178364515-1928918086" {
		t.Fatalf("serviceSID(fbmcp) = %s", got)
	}
	if sid, err := accountSID(`NT SERVICE\fbmcp`); err != nil || sid != serviceSID("fbmcp") {
		t.Fatalf("accountSID(NT SERVICE fbmcp) = %s, %v", sid, err)
	}
	if sid, err := accountSID("*S-1-5-18"); err != nil || sid != "S-1-5-18" {
		t.Fatalf("accountSID(*S-1-5-18) = %s, %v", sid, err)
	}
}

// Phase 12 test W: icacls failed (1332) for a service that does not exist
// yet. The API path must grant the computed SID anyway.
func TestGrantKeyReadUnknownService(t *testing.T) {
	key := filepath.Join(t.TempDir(), "tls.key")
	if err := os.WriteFile(key, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	const svc = "fbmcp-test-no-such-service"
	if err := grantKeyRead(key, `NT SERVICE\`+svc); err != nil {
		t.Fatalf("grantKeyRead: %v", err)
	}
	out, err := exec.Command("icacls", key).CombinedOutput()
	if err != nil {
		t.Fatalf("icacls: %v %s", err, out)
	}
	if !strings.Contains(string(out), serviceSID(svc)) {
		t.Fatalf("ACL has no entry for %s:\n%s", serviceSID(svc), out)
	}
	if strings.Contains(string(out), "(I)") {
		t.Fatalf("ACL still inherits:\n%s", out)
	}
}
