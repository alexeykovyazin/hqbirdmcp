// Package secrets implements the D5 keyring provider (phase8_plan D4.3 /
// improvement-plan E.4, per ADR-029-adjacent ADR-009 rollout): OS credential
// store as a fallback when an environment variable is unset. Env always
// wins — "env remains supported" is the compat contract; the keyring is
// keyed by the env-var NAME (e.g. fbmcpctl secret set FBMCP_DEV_PW), so a
// config that names FBMCP_DEV_PW works unchanged with either source.
//
// Phase 12 B0.9 adds the file source between env and keyring: one file per
// name in the folder set by SetDir (fbmcp.yaml secrets.dir). A service
// account has no usable keyring (Windows virtual account, Linux system
// unit), and a file changes without a process restart.
package secrets

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"

	keyring "github.com/zalando/go-keyring"
)

const service = "fbmcp"

// Source names (Lookup / fbmcpctl apikey list / doctor --remote).
const (
	SourceEnv     = "env"
	SourceFile    = "file"
	SourceKeyring = "keyring"
)

// keyringStore is the minimal keyring surface, indirected so tests can
// stub it (a real keyring needs a desktop daemon / user session).
type keyringStoreT interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

type osKeyring struct{}

func (osKeyring) Get(service, user string) (string, error) { return keyring.Get(service, user) }
func (osKeyring) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}
func (osKeyring) Delete(service, user string) error { return keyring.Delete(service, user) }

var keyringStore keyringStoreT = osKeyring{}

var fileDir atomic.Value // string

// SetDir sets the file source folder (fbmcp.yaml secrets.dir). Empty
// turns the file source off.
func SetDir(dir string) { fileDir.Store(strings.TrimSpace(dir)) }

// Dir returns the file source folder ("" = off).
func Dir() string {
	v, _ := fileDir.Load().(string)
	return v
}

var nameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// ValidName reports whether name can be a secret name (env var shape; it
// is also a file name, so no separators or dots).
func ValidName(name string) bool { return nameRe.MatchString(name) }

// Get returns the secret for envName: the environment variable if set,
// then the file <dir>/<envName>, then the OS keyring entry "fbmcp/<envName>".
func Get(envName string) (string, error) {
	v, _, err := Lookup(envName)
	return v, err
}

// Lookup is Get plus the source that answered (env / file / keyring).
func Lookup(envName string) (string, string, error) {
	if envName == "" {
		return "", "", fmt.Errorf("empty secret env name")
	}
	if v := os.Getenv(envName); v != "" {
		return v, SourceEnv, nil
	}
	var fileErr error
	if dir := Dir(); dir != "" {
		v, err := ReadFile(dir, envName)
		if err == nil {
			return v, SourceFile, nil
		}
		if !os.IsNotExist(err) {
			fileErr = err
		}
	}
	v, err := keyringStore.Get(service, envName)
	if err == nil && v != "" {
		return v, SourceKeyring, nil
	}
	if fileErr != nil {
		return "", "", fmt.Errorf("secret %s: %v (env not set, no keyring entry)", envName, fileErr)
	}
	if dir := Dir(); dir != "" {
		return "", "", fmt.Errorf("secret env %s is not set (no file %s and no keyring entry fbmcp/%s)", envName, filepath.Join(dir, envName), envName)
	}
	return "", "", fmt.Errorf("secret env %s is not set (and no keyring entry fbmcp/%s)", envName, envName)
}

// ReadFile reads <dir>/<name>. Surrounding white space (the newline an
// editor adds) is dropped. A file that "other" users can read is refused on
// Unix (fail closed, like ssh).
func ReadFile(dir, name string) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("invalid secret name %q", name)
	}
	p := filepath.Join(dir, name)
	fi, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if err := checkPerm(p, fi); err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("file %s is empty", p)
	}
	return v, nil
}

// WriteFile stores value in <dir>/<name> atomically (temp file + rename),
// mode 0600. On Unix, when run as root, the file gets the owner of dir so
// the service account that owns the folder can read it.
func WriteFile(dir, name, value string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid secret name %q (letters, digits, _)", name)
	}
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("empty secret value")
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("secrets dir: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("secrets dir %s is not a folder", dir)
	}
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil && !isWindows() {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(value + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := chownLike(tmpName, fi); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, name))
}

// DropFile removes <dir>/<name>.
func DropFile(dir, name string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid secret name %q", name)
	}
	return os.Remove(filepath.Join(dir, name))
}

// Set writes the keyring entry (fbmcpctl secret set).
func Set(envName, value string) error {
	if envName == "" || value == "" {
		return fmt.Errorf("secret name and value are required")
	}
	return keyringStore.Set(service, envName, value)
}

// Drop removes the keyring entry (fbmcpctl secret drop).
func Drop(envName string) error {
	return keyringStore.Delete(service, envName)
}
