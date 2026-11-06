package ctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/secrets"
)

// doctorFixture writes a config whose state folder does not exist yet and
// whose secrets folder holds only the read-only password.
func doctorFixture(t *testing.T) (cfgPath, stateDir, secDir string) {
	t.Helper()
	root := t.TempDir()
	stateDir = filepath.Join(root, "state")
	secDir = filepath.Join(root, "secrets")
	if err := os.Mkdir(secDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := secrets.WriteFile(secDir, "FBMCP_TEST_DOCTOR_RO", "x"); err != nil {
		t.Fatal(err)
	}
	y := "state:\n  dir: " + filepath.ToSlash(stateDir) + "\nsecrets:\n  dir: " + filepath.ToSlash(secDir) + `
instances:
  - id: fb5
    addr: 127.0.0.1:3055
    bin_dir: C:/HQbird/Firebird50
databases:
  - id: emp
    instance: fb5
    path: C:/db/emp.fdb
    ro_user: FBMCP_RO
    ro_secret_env: FBMCP_TEST_DOCTOR_RO
    admin_user: SYSDBA
    admin_secret_env: FBMCP_TEST_DOCTOR_ADMIN
`
	cfgPath = filepath.Join(root, "fbmcp.yaml")
	if err := os.WriteFile(cfgPath, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, stateDir, secDir
}

func stubLogin(t *testing.T, f func(admin bool) error) *[]bool {
	t.Helper()
	var calls []bool
	old := dbLogin
	dbLogin = func(_ context.Context, _ *config.Config, _ string, admin bool) error {
		calls = append(calls, admin)
		return f(admin)
	}
	t.Cleanup(func() { dbLogin = old })
	return &calls
}

// Every FAIL line says how to fix it.
func assertFailsHaveFix(t *testing.T, out string) {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "FAIL") && !strings.Contains(l, "Fix: ") {
			t.Errorf("FAIL line without a fix: %s", l)
		}
	}
}

func TestDoctorMakesStateDirAndNamesTheSecretFix(t *testing.T) {
	noKernel(t, false)
	cfgPath, stateDir, secDir := doctorFixture(t)
	calls := stubLogin(t, func(bool) error { return nil })
	var rc int
	out := captureStdout(t, func() { rc = cmdDoctor([]string{"--config", cfgPath}) })
	if rc != 1 {
		t.Fatalf("rc=%d, want 1 (admin password missing)\n%s", rc, out)
	}
	if !isDir(stateDir) || !strings.Contains(out, "made now") {
		t.Fatalf("state folder not made:\n%s", out)
	}
	if !strings.Contains(out, "read-only login as FBMCP_RO works (password from file)") {
		t.Fatalf("no real read-only login:\n%s", out)
	}
	if !strings.Contains(out, "Fix: fbmcp ctl secret set --dir "+filepath.ToSlash(secDir)+" FBMCP_TEST_DOCTOR_ADMIN") {
		t.Fatalf("missing admin secret: no fix with the secrets folder:\n%s", out)
	}
	if len(*calls) != 1 || (*calls)[0] {
		t.Fatalf("logins %v, want one read-only login (no admin login without a password)", *calls)
	}
	assertFailsHaveFix(t, out)
}

func TestDoctorLoginFailureNamesTheCause(t *testing.T) {
	noKernel(t, false)
	cfgPath, _, secDir := doctorFixture(t)
	if err := secrets.WriteFile(secDir, "FBMCP_TEST_DOCTOR_ADMIN", "x"); err != nil {
		t.Fatal(err)
	}
	stubLogin(t, func(admin bool) error {
		if admin {
			return errors.New("dial tcp 127.0.0.1:3055: connectex: No connection could be made because the target machine actively refused it.")
		}
		return errors.New("Your user name and password are not defined. Ask your database administrator to set up a Firebird login.")
	})
	out := captureStdout(t, func() { cmdDoctor([]string{"--config", cfgPath}) })
	if !strings.Contains(out, "if user FBMCP_RO does not exist, create it") {
		t.Fatalf("bad password: no user hint:\n%s", out)
	}
	if !strings.Contains(out, "Fix: start Firebird on 127.0.0.1:3055") {
		t.Fatalf("refused: no start hint:\n%s", out)
	}
	assertFailsHaveFix(t, out)
}

func TestDoctorGreen(t *testing.T) {
	noKernel(t, false)
	cfgPath, _, secDir := doctorFixture(t)
	if err := secrets.WriteFile(secDir, "FBMCP_TEST_DOCTOR_ADMIN", "x"); err != nil {
		t.Fatal(err)
	}
	stubLogin(t, func(bool) error { return nil })
	var rc int
	out := captureStdout(t, func() { rc = cmdDoctor([]string{cfgPath}) }) // the old positional form
	if rc != 0 || !strings.Contains(out, "doctor: green") {
		t.Fatalf("rc=%d\n%s", rc, out)
	}
	if !strings.Contains(out, "test event written") {
		t.Fatalf("no event check:\n%s", out)
	}
}
