// Package adminexec implements the ADR-003 subprocess backend: guarded
// utility execution with absolute paths, argv arrays (never a shell),
// env-only credentials, wall-clock timeout, and an output-size cap.
// P0.2 constraint: Stdout and Stderr share ONE writer value (separate
// non-file writers lose output on Windows).
package adminexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Result of a guarded run.
type Result struct {
	Output string
	Exit   int
	Err    error
}

// Run executes bin with args under the guard contract. secretEnv maps env
// var names to values (e.g. ISC_PASSWORD) — they never appear in argv.
func Run(ctx context.Context, bin string, args []string, timeout time.Duration, maxOutput int64, secretEnv map[string]string) Result {
	if !filepath.IsAbs(bin) {
		return Result{Err: fmt.Errorf("adminexec: refusing relative binary path %q (§4.1)", bin), Exit: -1}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = os.Environ()
	for k, v := range secretEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	buf := &bytes.Buffer{}
	out := &limited{w: buf, n: maxOutput}
	cmd.Stdout = out // same writer value for both — see P0.2 finding
	cmd.Stderr = out
	err := cmd.Run()
	exit := -1
	if cmd.ProcessState != nil {
		exit = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("timeout after %s", timeout)
	}
	return Result{Output: buf.String(), Exit: exit, Err: err}
}

type limited struct {
	w interface{ Write([]byte) (int, error) }
	n int64
}

func (l *limited) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	if int64(len(p)) > l.n {
		l.w.Write(p[:l.n])
		l.n = 0
		return len(p), nil
	}
	l.n -= int64(len(p))
	return l.w.Write(p)
}

// IsTimeout reports whether the error was the wall-clock timeout.
func (r Result) IsTimeout() bool { return errors.Is(r.Err, os.ErrDeadlineExceeded) || r.Err != nil && r.Exit == -1 }
