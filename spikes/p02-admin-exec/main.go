// P0.2 admin-execution spike — guarded subprocess harness proof (T7),
// credential-via-env (T2), golden-corpus capture (T5).
// Throwaway code: hardcoded paths and masterkey acceptable here only.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type inst struct {
	name, dir, port string
}

var instances = []inst{
	{"FB3.0", `C:\HQbird\Firebird30`, "3053"},
	{"FB4.0", `C:\HQbird\Firebird40`, "3054"},
	{"FB5.0", `C:\HQbird\Firebird50`, "3055"},
}

var spikeDir = `C:\HQbirdData\output\fbmcp-spike`
var goldenDir = `spikes/p02-admin-exec/golden`

// runGuarded is the minimal executor contract from the plan (4.1):
// absolute path, argv array (no shell), env-only secrets, wall-clock timeout,
// output size cap.
func runGuarded(ctx context.Context, bin string, args []string, timeout time.Duration, maxOutput int64) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "ISC_PASSWORD=masterkey") // never in argv
	// Both streams share ONE writer (same interface value) so os/exec uses a
	// single pipe; separate non-file writers lose output on Windows (spike finding).
	out := &limitedWriter{w: &bytes.Buffer{}, n: maxOutput}
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	exit := 0
	if cmd.ProcessState != nil {
		exit = cmd.ProcessState.ExitCode()
	}
	return out.w.(*bytes.Buffer).String(), exit, err
}

type limitedWriter struct {
	w io.Writer
	n int64
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil // swallow beyond cap
	}
	if int64(len(p)) > l.n {
		l.w.Write(p[:l.n])
		l.n = 0
		return len(p), nil
	}
	l.n -= int64(len(p))
	return l.w.Write(p)
}

func main() {
	os.MkdirAll(filepath.Join(goldenDir), 0o755)
	for _, in := range instances {
		fmt.Printf("=== %s\n", in.name)
		db := filepath.Join(spikeDir, "spike_"+in.name+".fdb")

		// gbak backup with verbose output
		out, exit, err := runGuarded(context.Background(),
			filepath.Join(in.dir, "gbak.exe"),
			[]string{"-b", "-v", "-user", "SYSDBA", db, filepath.Join(spikeDir, "spike_"+in.name+".fbk")},
			60*time.Second, 4<<20)
		report(in, "gbak -b -v", out, exit, err)
		capture(in, "gbak-backup-verbose", out)

		// gbak verify
		out, exit, err = runGuarded(context.Background(),
			filepath.Join(in.dir, "gbak.exe"),
			[]string{"-v", "-user", "SYSDBA", filepath.Join(spikeDir, "spike_"+in.name+".fbk")},
			60*time.Second, 4<<20)
		report(in, "gbak -v (verify)", out, exit, err)
		capture(in, "gbak-verify", out)

		// gstat header
		out, exit, err = runGuarded(context.Background(),
			filepath.Join(in.dir, "gstat.exe"),
			[]string{"-h", "-user", "SYSDBA", db},
			30*time.Second, 1<<20)
		report(in, "gstat -h", out, exit, err)
		capture(in, "gstat-header", out)

		// gfix validate
		out, exit, err = runGuarded(context.Background(),
			filepath.Join(in.dir, "gfix.exe"),
			[]string{"-validate", "-full", "-user", "SYSDBA", db},
			60*time.Second, 1<<20)
		report(in, "gfix -validate -full", out, exit, err)
		capture(in, "gfix-validate-full", out)

		// error-path evidence: wrong password (via env) → taxonomy sample
		cmd := exec.Command(filepath.Join(in.dir, "gstat.exe"), "-h", "-user", "SYSDBA", db)
		cmd.Env = append(os.Environ(), "ISC_PASSWORD=wrongpass")
		var eb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &eb, &eb
		eerr := cmd.Run()
		report(in, "gstat bad-password", eb.String(), cmd.ProcessState.ExitCode(), eerr)
		capture(in, "gstat-auth-failure", eb.String())
	}
	fmt.Println("\n-- argv visibility check: strings in our own command lines contain no password by construction (env-only).")
}

func report(in inst, what, out string, exit int, err error) {
	status := "OK"
	if err != nil {
		status = "ERR"
	}
	tail := ""
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(l) != "" {
			tail = strings.TrimSpace(l)
		}
	}
	fmt.Printf("[%s] %-22s %s exit=%d lastline=%q\n", in.name, what, status, exit, tail)
}

func capture(in inst, scenario, out string) {
	sub := filepath.Join(goldenDir, strings.ToLower(scenario))
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, strings.ToLower(in.name)+".txt"), []byte(out), 0o644)
}
