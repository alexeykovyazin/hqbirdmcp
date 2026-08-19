// Package housekeep rotates server/trace logs and removes aged work-dir
// orphans (P5.3 T5). Never deletes cataloged backup artifacts.
package housekeep

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aleks/fbmcp/internal/confine"
)

// Rotate moves path to path.1 (and path.1 → path.2) if it exceeds maxBytes.
func Rotate(path string, maxBytes int64, generations int) error {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Size() <= maxBytes {
		return nil
	}
	if generations < 1 {
		generations = 1
	}
	_ = os.Remove(path + "." + strconv.Itoa(generations))
	for i := generations - 1; i >= 1; i-- {
		from, to := path+"."+strconv.Itoa(i), path+"."+strconv.Itoa(i+1)
		_ = os.Rename(from, to)
	}
	if err := os.Rename(path, path+".1"); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	return f.Close()
}

// CleanOrphans removes files in dir older than age whose name matches suffixes.
// dir must pass confine.Dir (no symlink / .. / UNC). Symlink files are skipped.
func CleanOrphans(dir string, age time.Duration, suffixes []string) (int, error) {
	if err := confine.Dir(dir); err != nil {
		return 0, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-age)
	n := 0
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		match := false
		name := e.Name()
		for _, s := range suffixes {
			if strings.HasSuffix(name, s) {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		full := filepath.Join(dir, name)
		if err := confine.FileIn(dir, full); err != nil {
			continue
		}
		if err := os.Remove(full); err == nil {
			n++
		}
	}
	return n, nil
}
