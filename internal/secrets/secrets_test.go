package secrets

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// stubKeyring replaces the OS keyring for tests (the real one needs a
// Secret Service daemon on Linux and user interaction on macOS).
type stubKeyring struct{ store map[string]string }

func (s *stubKeyring) Get(service, user string) (string, error) {
	if v, ok := s.store[user]; ok {
		return v, nil
	}
	return "", errNotFound
}
func (s *stubKeyring) Set(service, user, password string) error {
	s.store[user] = password
	return nil
}
func (s *stubKeyring) Delete(service, user string) error {
	delete(s.store, user)
	return nil
}

var errNotFound = errors.New("secret not found")

func withStubStore(t *testing.T) *map[string]string {
	t.Helper()
	stub := &stubKeyring{store: map[string]string{}}
	prev := keyringStore
	keyringStore = stub
	t.Cleanup(func() { keyringStore = prev })
	return &stub.store
}

func TestGetEnvWins(t *testing.T) {
	store := withStubStore(t)
	(*store)["FBMCP_DEV_PW"] = "from-keyring"
	t.Setenv("FBMCP_DEV_PW", "from-env")
	got, err := Get("FBMCP_DEV_PW")
	if err != nil || got != "from-env" {
		t.Fatalf("Get=%q err=%v, want env to win", got, err)
	}
}

func TestGetFallsBackToKeyring(t *testing.T) {
	store := withStubStore(t)
	(*store)["FBMCP_DEV_PW"] = "from-keyring"
	os.Unsetenv("FBMCP_DEV_PW")
	got, err := Get("FBMCP_DEV_PW")
	if err != nil || got != "from-keyring" {
		t.Fatalf("Get=%q err=%v, want keyring fallback", got, err)
	}
}

func TestGetMissingFailsClosed(t *testing.T) {
	withStubStore(t)
	os.Unsetenv("FBMCP_DEV_PW")
	_, err := Get("FBMCP_DEV_PW")
	if err == nil || !strings.Contains(err.Error(), "not set") {
		t.Fatalf("err=%v, want fail-closed message", err)
	}
}

func TestGetEmptyName(t *testing.T) {
	if _, err := Get(""); err == nil {
		t.Fatal("empty env name must error")
	}
}

func TestSetDropRoundTrip(t *testing.T) {
	store := withStubStore(t)
	if err := Set("FBMCP_X", "v"); err != nil || (*store)["FBMCP_X"] != "v" {
		t.Fatalf("Set: %v", err)
	}
	if err := Set("FBMCP_X", ""); err == nil {
		t.Fatal("Set with empty value must error")
	}
	if err := Drop("FBMCP_X"); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if _, ok := (*store)["FBMCP_X"]; ok {
		t.Fatal("entry still present after Drop")
	}
}
