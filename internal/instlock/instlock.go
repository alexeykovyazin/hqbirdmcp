// Package instlock implements decision D8 / ADR-005: a single active fbmcp
// instance per kernel state directory, enforced by a lock file holding the
// owner PID. A second process fails fast with a clear message (never
// dual-writes kernel state — safety fuse #6).
package instlock

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Lock is the acquired single-instance lock.
type Lock struct {
	f *os.File
}

// Acquire takes the instance lock for dir. On Windows it uses a mandatory
// share-lock on the file (LockFileEx semantics via syscall); the PID file
// content additionally enables diagnostics. Stale locks from a dead process
// are reclaimed automatically.
func Acquire(dir string) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "instance.lock")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, fmt.Errorf("instance lock: open: %w", err)
	}
	// reclaim stale: if we can take the exclusive OS lock, any recorded PID
	// belonged to a dead process
	if err := lockExcl(f); err != nil {
		other, _ := os.ReadFile(p)
		f.Close()
		return nil, fmt.Errorf("another fbmcp instance is active (state dir %s, recorded owner: %s) — stop it or use a different state dir", dir, strings.TrimSpace(string(other)))
	}
	if err := f.Truncate(0); err == nil {
		f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0)
	}
	return &Lock{f: f}, nil
}

// Release drops the lock (process shutdown).
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	unlock(l.f)
	l.f.Close()
	l.f = nil
}

// OwnerPID reports the recorded owner of the lock file at dir (0 if none) —
// used by `fbmcp status` style diagnostics.
func OwnerPID(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "instance.lock"))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}
