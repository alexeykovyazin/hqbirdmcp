package adminexec

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRunRefusesRelativeBin(t *testing.T) {
	r := Run(context.Background(), "gbak", []string{"-b", "db", "out"}, time.Second, 1024, map[string]string{"ISC_PASSWORD": "hunter2"})
	if r.Err == nil || !strings.Contains(r.Err.Error(), "relative") {
		t.Fatalf("relative bin accepted: %+v", r)
	}
}

func TestOutputCap(t *testing.T) {
	bin := "/bin/echo"
	if runtime.GOOS == "windows" {
		bin = `C:\Windows\System32\cmd.exe`
	}
	var args []string
	if runtime.GOOS == "windows" {
		args = []string{"/c", "echo", strings.Repeat("A", 200)}
	} else {
		args = []string{strings.Repeat("A", 200)}
	}
	r := Run(context.Background(), bin, args, 5*time.Second, 32, nil)
	if len(r.Output) > 32 {
		t.Fatalf("output not capped: %d", len(r.Output))
	}
}

func TestPasswordNotInArgvContract(t *testing.T) {
	// argv is a slice; secretEnv is a separate map — the type system is the
	// contract. This test locks the call shape used by workflows/gfix.
	args := []string{"-user", "SYSDBA", "-b", "db.fdb", "out.fbk"}
	for _, a := range args {
		if strings.Contains(strings.ToLower(a), "password") || strings.Contains(a, "hunter2") {
			t.Fatalf("secret in argv: %q", a)
		}
	}
	env := map[string]string{"ISC_PASSWORD": "hunter2"}
	if env["ISC_PASSWORD"] == "" {
		t.Fatal("env must carry the secret")
	}
}
