// Package confine enforces C9: file I/O stays inside configured roots.
// Symlinks are refused (phase6_plan_v2.md §12).
package confine

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Dir refuses relative paths, `..`, UNC, Windows ADS / trailing-dot names,
// and existing symlinks. A path that does not exist yet is still checked
// syntactically (so a work dir cannot be created as an escape).
func Dir(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("confine: empty path")
	}
	if strings.Contains(path, "..") {
		return fmt.Errorf("confine: refusing path with ..")
	}
	if strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, "//") {
		return fmt.Errorf("confine: refusing UNC path")
	}
	base := filepath.Base(path)
	if runtime.GOOS == "windows" {
		if i := strings.Index(base, ":"); i >= 0 {
			return fmt.Errorf("confine: refusing ADS / stream name %q", base)
		}
		if strings.HasSuffix(base, ".") || strings.HasSuffix(base, " ") {
			return fmt.Errorf("confine: refusing trailing-dot/space name")
		}
	}
	if filepath.IsAbs(path) {
		// ok
	} else if runtime.GOOS == "windows" && len(path) >= 3 && path[1] == ':' {
		// C:\... parsed as relative on non-Windows; on Windows IsAbs handles it
	} else if !filepath.IsAbs(path) {
		return fmt.Errorf("confine: refusing relative path %q", path)
	}
	fi, err := os.Lstat(path)
	if err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("confine: refusing symlink %q", path)
	}
	return nil
}

// JoinUnder joins name onto root after Dir(root). name must be a single
// base name (no separators, no `..`).
func JoinUnder(root, name string) (string, error) {
	if err := Dir(root); err != nil {
		return "", err
	}
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
		return "", fmt.Errorf("confine: refusing name %q", name)
	}
	if strings.Contains(name, "..") {
		return "", fmt.Errorf("confine: refusing name %q", name)
	}
	if runtime.GOOS == "windows" && strings.Contains(name, ":") {
		return "", fmt.Errorf("confine: refusing ADS name %q", name)
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return "", fmt.Errorf("confine: refusing trailing-dot/space name")
	}
	p := filepath.Join(root, name)
	rel, err := filepath.Rel(root, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("confine: %q escapes %q", name, root)
	}
	return p, nil
}

// FileIn reports whether path (after Clean) stays under root.
func FileIn(root, path string) error {
	if err := Dir(root); err != nil {
		return err
	}
	if err := Dir(filepath.Dir(path)); err != nil {
		return err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("confine: %q not under %q", path, root)
	}
	fi, err := os.Lstat(path)
	if err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("confine: refusing symlink file %q", path)
	}
	return nil
}
