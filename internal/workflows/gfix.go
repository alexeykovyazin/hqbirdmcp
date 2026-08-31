package workflows

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aleks/fbmcp/internal/adminexec"
	"github.com/aleks/fbmcp/internal/config"
)

// runCmd is the subprocess seam (stubbed in tests).
var runCmd = adminexec.Run

// connString renders the TCP connection string for gfix:
//
//	localhost/port:/path/db.fdb        (Linux)
//	localhost/port:Disk\Path\db.fdb    (Windows)
//
// A bare path means a LOCAL (embedded) attach: the client then loads the
// embedded engine and scans config-declared plugins, which fails on
// HQBird installs (MySQLEngine dependency error) — and the operation
// silently does not happen while the exit code may still be 0. Always
// connect over TCP to the instance's server.
func connString(inst config.FBInstance, dbPath string) string {
	port := "3050"
	if i := strings.LastIndex(inst.Addr, ":"); i >= 0 {
		port = inst.Addr[i+1:]
	}
	return fmt.Sprintf("localhost/%s:%s", port, dbPath)
}

func gfixBin(inst config.FBInstance) string {
	name := "gfix"
	if runtime.GOOS == "windows" {
		name = "gfix.exe"
	}
	return filepath.Join(inst.BinDir, name)
}

// gfixEnv builds the subprocess environment for gfix: credentials plus a
// PATH that includes the instance bin_dir AND the server root above it.
// HQBird installs ship config-declared plugins (e.g. MySQLEngine) whose
// dependency DLLs (libmariadb.dll) live in the server root; without it on
// PATH every client attach from gfix fails with "Error loading plugin" and
// the operation silently does not happen (observed live 2026-08-31: a
// gfix -shut applied but reported failure, and -online never took effect,
// leaving a database stuck in maintenance mode).
func gfixEnv(inst config.FBInstance, user, pass string) map[string]string {
	root := filepath.Dir(inst.BinDir)
	env := map[string]string{
		"ISC_USER":     user,
		"ISC_PASSWORD": pass,
	}
	if prev := os.Getenv("PATH"); prev != "" {
		env["PATH"] = inst.BinDir + string(os.PathListSeparator) + root +
			string(os.PathListSeparator) + prev
	} else {
		env["PATH"] = inst.BinDir + string(os.PathListSeparator) + root
	}
	return env
}

// GfixShutdown puts the database into shutdown (exclusive) mode.
// mode is "force", "attach", or "tran" (v3 row 56).
func GfixShutdown(ctx context.Context, inst config.FBInstance, dbPath, user, pass, mode string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	flag := "-force"
	switch mode {
	case "attach":
		flag = "-attach"
	case "tran":
		flag = "-tran"
	}
	res := runCmd(ctx, gfixBin(inst), []string{"-shut", flag, "0", connString(inst, dbPath)}, timeout, 64<<10,
		gfixEnv(inst, user, pass))
	if res.Err != nil {
		return fmt.Errorf("gfix -shut: %w (%s)", res.Err, res.Output)
	}
	return nil
}

// GfixOnline brings the database back online. Called from compensation and
// the workflow tail (AutoReopen).
func GfixOnline(ctx context.Context, inst config.FBInstance, dbPath, user, pass string) error {
	res := runCmd(ctx, gfixBin(inst), []string{"-online", connString(inst, dbPath)}, 30*time.Second, 64<<10,
		gfixEnv(inst, user, pass))
	if res.Err != nil {
		return fmt.Errorf("gfix -online: %w (%s)", res.Err, res.Output)
	}
	return nil
}

// GfixBuffers sets page buffers (P3.5 leftover).
func GfixBuffers(ctx context.Context, inst config.FBInstance, dbPath, user, pass string, n int) error {
	res := runCmd(ctx, gfixBin(inst), []string{"-buffers", fmt.Sprintf("%d", n), connString(inst, dbPath)}, 30*time.Second, 64<<10,
		gfixEnv(inst, user, pass))
	if res.Err != nil {
		return fmt.Errorf("gfix -buffers: %w (%s)", res.Err, res.Output)
	}
	return nil
}
