package workflows

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"time"

	"github.com/aleks/fbmcp/internal/adminexec"
	"github.com/aleks/fbmcp/internal/config"
)

func gfixBin(inst config.FBInstance) string {
	name := "gfix"
	if runtime.GOOS == "windows" {
		name = "gfix.exe"
	}
	return filepath.Join(inst.BinDir, name)
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
	res := adminexec.Run(ctx, gfixBin(inst), []string{"-shut", flag, "0", dbPath}, timeout, 64<<10,
		map[string]string{"ISC_USER": user, "ISC_PASSWORD": pass})
	if res.Err != nil {
		return fmt.Errorf("gfix -shut: %w (%s)", res.Err, res.Output)
	}
	return nil
}

// GfixOnline brings the database back online. Called from compensation and
// the workflow tail (AutoReopen).
func GfixOnline(ctx context.Context, inst config.FBInstance, dbPath, user, pass string) error {
	res := adminexec.Run(ctx, gfixBin(inst), []string{"-online", dbPath}, 30*time.Second, 64<<10,
		map[string]string{"ISC_USER": user, "ISC_PASSWORD": pass})
	if res.Err != nil {
		return fmt.Errorf("gfix -online: %w (%s)", res.Err, res.Output)
	}
	return nil
}

// GfixBuffers sets page buffers (P3.5 leftover).
func GfixBuffers(ctx context.Context, inst config.FBInstance, dbPath, user, pass string, n int) error {
	res := adminexec.Run(ctx, gfixBin(inst), []string{"-buffers", fmt.Sprintf("%d", n), dbPath}, 30*time.Second, 64<<10,
		map[string]string{"ISC_USER": user, "ISC_PASSWORD": pass})
	if res.Err != nil {
		return fmt.Errorf("gfix -buffers: %w (%s)", res.Err, res.Output)
	}
	return nil
}
