package secrets

import (
	"os"
	"testing"
)

// Env wins over the keyring (ADR-009 compat contract).
func TestEnvWins(t *testing.T) {
	os.Setenv("FBMCP_TEST_SECRET_X", "env-value")
	defer os.Unsetenv("FBMCP_TEST_SECRET_X")
	v, err := Get("FBMCP_TEST_SECRET_X")
	if err != nil || v != "env-value" {
		t.Fatalf("env must win: %v %q", err, v)
	}
}

func TestMissingIsError(t *testing.T) {
	if _, err := Get("FBMCP_TEST_SECRET_UNSET_9182"); err == nil {
		t.Fatal("missing secret must error")
	}
}

// Live keyring round-trip: run with FBMCP_TEST_KEYRING=1 on a host with a
// credential store (Windows Credential Manager / Linux Secret Service).
func TestKeyringRoundTripLive(t *testing.T) {
	if os.Getenv("FBMCP_TEST_KEYRING") != "1" {
		t.Skip("set FBMCP_TEST_KEYRING=1 to touch the real OS keyring")
	}
	const n = "FBMCP_TEST_KEYRING_RT"
	if err := Set(n, "kr-value"); err != nil {
		t.Skipf("no keyring available: %v", err)
	}
	defer Drop(n)
	os.Unsetenv(n)
	v, err := Get(n)
	if err != nil || v != "kr-value" {
		t.Fatalf("keyring fallback failed: %v %q", err, v)
	}
}
