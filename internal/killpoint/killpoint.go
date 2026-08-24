// Package killpoint provides deterministic fault-injection checkpoints for
// the P6.2 chaos harness (improvement-plan WS1 A.1). Checkpoints are armed
// only via FBMCP_KILLPOINT (comma-separated names) plus FBMCP_KILLPOINT_DIR
// (coordination directory); with either unset, Hit is a no-op so normal and
// release operation is unaffected. An armed checkpoint writes <name>.ready
// into the coordination directory, then blocks until the harness kills the
// process or drops <name>.release (injection aborted, execution continues).
package killpoint

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	mu      sync.RWMutex
	enabled = parse(os.Getenv("FBMCP_KILLPOINT"))
)

func parse(v string) map[string]bool {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	m := map[string]bool{}
	for _, n := range strings.Split(v, ",") {
		if n = strings.TrimSpace(n); n != "" {
			m[n] = true
		}
	}
	return m
}

// Enabled reports whether the named checkpoint is armed.
func Enabled(name string) bool {
	mu.RLock()
	defer mu.RUnlock()
	return enabled != nil && enabled[name]
}

// SetEnabled overrides the armed set (testing only).
func SetEnabled(names map[string]bool) {
	mu.Lock()
	enabled = names
	mu.Unlock()
}

// Hit blocks at a named checkpoint when armed (see package comment). The
// wait is bounded (~5 minutes) so a dead harness cannot wedge the kernel
// forever; on timeout the injection is treated as aborted and execution
// continues.
func Hit(name string) {
	if !Enabled(name) {
		return
	}
	dir := os.Getenv("FBMCP_KILLPOINT_DIR")
	if dir == "" {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, name+".ready"), []byte("ready\n"), 0o644)
	rel := filepath.Join(dir, name+".release")
	for i := 0; i < 6000; i++ {
		if _, err := os.Stat(rel); err == nil {
			_ = os.Remove(rel)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
