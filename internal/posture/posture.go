package posture

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/aleks/fbmcp/internal/config"
)

func Marker(stateDir string) string { return filepath.Join(stateDir, "posture.verified") }

func Verified(stateDir string) bool {
	_, err := os.Stat(Marker(stateDir))
	return err == nil
}

func WriteMarker(stateDir, note string) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(Marker(stateDir), []byte(note+"\n"), 0o640)
}

// Report is verify-mode: check + text, no mutation.
func Report(cfg *config.Config) (ok bool, text string) {
	var b strings.Builder
	ok = true
	fmt.Fprintf(&b, "os: %s\nstate: %s\n", runtime.GOOS, cfg.State.Dir)
	if cfg.State.Dir == "" {
		ok = false
		b.WriteString("FAIL: state.dir empty\n")
	} else if st, err := os.Stat(cfg.State.Dir); err != nil || !st.IsDir() {
		ok = false
		fmt.Fprintf(&b, "FAIL: state.dir not a directory: %v\n", err)
	} else {
		b.WriteString("ok: state.dir exists\n")
	}
	for _, db := range cfg.Databases {
		for _, dir := range []string{db.BackupDir, db.WorkDir} {
			if dir == "" {
				continue
			}
			if st, err := os.Stat(dir); err != nil || !st.IsDir() {
				fmt.Fprintf(&b, "WARN: %s dir %s: %v\n", db.ID, dir, err)
			} else {
				fmt.Fprintf(&b, "ok: %s dir %s\n", db.ID, dir)
			}
		}
	}
	if Verified(cfg.State.Dir) {
		b.WriteString("ok: posture.verified marker present\n")
	} else {
		b.WriteString("note: posture.verified marker absent — service start/stop will refuse (ADR-017)\n")
	}
	return ok, b.String()
}

func RefuseMessage() string {
	return "host posture not verified (ADR-017). Run: fbmcpctl doctor  OR  packaging/posture/verify, then fbmcpctl setup --write-posture"
}
