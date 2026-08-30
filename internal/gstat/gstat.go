// Package gstat implements the gstat subprocess route reserved by ADR-003
// (the "future use (gstat route)" placeholder in cmd/fbmcp/p2tools.go):
// header page dumps (-h, reads the file directly, no auth) and record /
// index statistics (-r, attaches via the embedded engine, needs an
// authenticated user holding USE_GSTAT_UTILITY on FB 5+).
package gstat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aleks/fbmcp/internal/adminexec"
	"github.com/aleks/fbmcp/internal/config"
)

// Modes.
const (
	ModeHeader  = "header"
	ModeRecords = "records"
)

const (
	defaultTimeout = 60 * time.Second
	defaultMaxOut  = 4 << 20 // 4 MiB, same cap as queryplan
	maxTables      = 64
)

// tableNameOK restricts -t values to identifier characters: gstat matching
// is exact-case and the name lands in argv, so spaces, quotes and leading
// dashes (which gstat would parse as its own switches) are rejected. $ is
// allowed for RDB$… system relations (records mode with System=true).
var tableNameOK = regexp.MustCompile(`^[A-Za-z0-9_$]+$`)

// Options selects the gstat run.
type Options struct {
	Mode      string   // ModeHeader (default) or ModeRecords
	Tables    []string // records mode: restrict analysis to these tables (exact case)
	System    bool     // records mode: include system relations (-s)
	Timeout   time.Duration
	MaxOutput int64
}

// Bin resolves the gstat executable inside the instance bin_dir:
// gstat.exe first, then the non-Windows layout (like lwmonitoring).
func Bin(inst config.FBInstance) (string, error) {
	bin := filepath.Join(inst.BinDir, "gstat.exe")
	if _, err := os.Stat(bin); err != nil {
		bin = filepath.Join(inst.BinDir, "gstat")
		if _, err := os.Stat(bin); err != nil {
			return "", fmt.Errorf("gstat: executable not found in instance bin_dir %q", inst.BinDir)
		}
	}
	return bin, nil
}

// BuildArgs constructs the gstat argv: switches first, database path last.
// Table selection uses repeated -t before the path (FB 2.5+ form).
func BuildArgs(user, dbPath string, o Options) ([]string, error) {
	mode, err := normalizeMode(o.Mode)
	if err != nil {
		return nil, err
	}
	if mode == ModeHeader && (len(o.Tables) > 0 || o.System) {
		return nil, fmt.Errorf("gstat: tables/system apply only to %q mode", ModeRecords)
	}
	args := []string{"-user", user}
	if mode == ModeRecords {
		args = append(args, "-r")
		if o.System {
			args = append(args, "-s")
		}
		if len(o.Tables) > maxTables {
			return nil, fmt.Errorf("gstat: at most %d tables per run, got %d", maxTables, len(o.Tables))
		}
		for _, t := range o.Tables {
			if !tableNameOK.MatchString(t) {
				return nil, fmt.Errorf("gstat: invalid table name %q (allowed: letters, digits, _, $)", t)
			}
			args = append(args, "-t", t)
		}
	} else {
		args = append(args, "-h")
	}
	return append(args, dbPath), nil
}

// runCmd is injectable for tests; production path is adminexec.Run.
var runCmd = adminexec.Run

// normalizeMode defaults to ModeHeader and rejects unknown values.
func normalizeMode(m string) (string, error) {
	if m == "" {
		return ModeHeader, nil
	}
	if m != ModeHeader && m != ModeRecords {
		return "", fmt.Errorf("gstat: mode must be %q or %q, got %q", ModeHeader, ModeRecords, m)
	}
	return m, nil
}

// Run executes gstat against dbPath and returns its raw output. The
// password goes only to the environment (ISC_PASSWORD), never argv.
//
// Records mode attaches to the database, and the embedded provider is
// fragile: while the CALLING process holds SQL connections to the same
// database (fbmcp's pools), the embedded attach silently skips the
// analysis step and exits 1 (observed live on HQbird FB5; also sensitive
// to plugin-DLL contention). So records mode prefers a wire attach to the
// instance address (host/port:path — same targeting as lwmonitoring) and
// falls back to the embedded file route only when no engine answers there.
// Header mode always reads the file directly (no attach, no auth).
func Run(ctx context.Context, inst config.FBInstance, user, pass, dbPath string, o Options) (string, error) {
	mode, err := normalizeMode(o.Mode)
	if err != nil {
		return "", err
	}
	bin, err := Bin(inst)
	if err != nil {
		return "", err
	}
	timeout, maxOut := o.Timeout, o.MaxOutput
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if maxOut <= 0 {
		maxOut = defaultMaxOut
	}
	env := map[string]string{"ISC_PASSWORD": pass}
	attempt := func(target string) adminexec.Result {
		args, err := BuildArgs(user, target, o)
		if err != nil {
			return adminexec.Result{Err: err, Exit: -1}
		}
		return runCmd(ctx, bin, args, timeout, maxOut, env)
	}
	failure := func(res adminexec.Result, out string) error {
		if res.Err != nil && res.Exit < 0 {
			return fmt.Errorf("gstat: %v%s", res.Err, detail(out))
		}
		return fmt.Errorf("gstat: exit %d%s", res.Exit, detail(out))
	}

	if mode == ModeRecords && inst.Addr != "" {
		if wire := wireTarget(inst.Addr, dbPath); wire != dbPath {
			wres := attempt(wire)
			wout := strings.TrimSpace(wres.Output)
			if completed(wout, mode) || (wres.Exit == 0 && wres.Err == nil && wout != "") {
				return wout, nil
			}
			if !transportUnreachable(wout) {
				// A non-transport wire failure (auth, privileges) is the
				// truthful result — the file route would fail the same way
				// but with worse diagnostics.
				return "", failure(wres, wout)
			}
			// no engine on the instance address — continue to the file
		}
	}

	res := attempt(dbPath)
	out := strings.TrimSpace(res.Output)
	if res.Exit != 0 {
		// HQbird plugin-DLL contention ("Error loading plugin …") can be
		// transient: one retry, then the failure stands.
		if !completed(out, mode) && strings.Contains(out, "Error loading plugin") && ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-time.After(500 * time.Millisecond):
			}
			if ctx.Err() == nil {
				res = attempt(dbPath)
				out = strings.TrimSpace(res.Output)
			}
		}
		// completed(out, mode) also rescues the benign variant: gstat
		// exits 1 on plugin-load warnings while the report itself is
		// complete (noise stays visible in the collected output).
		if res.Exit != 0 && !completed(out, mode) {
			return "", failure(res, out)
		}
		if out == "" {
			return "", fmt.Errorf("gstat: empty output")
		}
		return out, nil
	}
	if res.Err != nil {
		return "", fmt.Errorf("gstat: %v%s", res.Err, detail(out))
	}
	if out == "" {
		return "", fmt.Errorf("gstat: empty output")
	}
	return out, nil
}

// wireTarget converts an instance "host:port" address plus the database
// file path into gstat's remote attach form "host/port:path".
func wireTarget(addr, dbPath string) string {
	host, port := addr, ""
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host, port = addr[:i], addr[i+1:]
	}
	if host == "" {
		host = "localhost"
	}
	if port == "" {
		return host + ":" + dbPath
	}
	return host + "/" + port + ":" + dbPath
}

// transportUnreachable matches Firebird's connection-failure wording for
// "nothing is listening on the instance address".
func transportUnreachable(out string) bool {
	l := strings.ToLower(out)
	for _, sig := range []string{"unable to complete network request", "connection refus", "no route to host", "timed out", "network unreachable", "failed to establish a connection"} {
		if strings.Contains(l, sig) {
			return true
		}
	}
	return false
}

// completed reports whether gstat finished its report despite a nonzero
// exit: HQbird plugin load failures (MySQLEngine observed) make gstat exit
// 1 while the statistics themselves are complete. The noise stays in the
// collected output; only a finished report is accepted.
func completed(out, mode string) bool {
	if !strings.Contains(out, "Gstat completion time") {
		return false
	}
	if mode == ModeRecords {
		return strings.Contains(out, "Analyzing database pages")
	}
	return strings.Contains(out, "Database header page information:")
}

// detail attaches the head of gstat's own output (its error lines come
// first) plus a remediation hint where the failure is a known one.
func detail(out string) string {
	s := ""
	if out != "" {
		s = "\n" + head(out, 12)
	}
	switch {
	case strings.Contains(out, "USE_GSTAT_UTILITY"):
		s += "\nhint: statistics mode attaches via the embedded engine; the user needs the USE_GSTAT_UTILITY system privilege (FB 5+) and a valid password"
	case strings.Contains(out, "not defined"):
		s += "\nhint: check the database's admin_user / admin_secret_env — gstat rejected the credentials"
	}
	return s
}

func head(s string, lines int) string {
	ps := strings.Split(s, "\n")
	if len(ps) > lines {
		ps = append(ps[:lines], "…")
	}
	return strings.Join(ps, "\n")
}
