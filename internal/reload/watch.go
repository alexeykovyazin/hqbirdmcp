package reload

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch directory of the live SourcePath; debounce then Apply("watch", "").
// Phase 12 B0.9: the secrets.dir folder is watched too; a changed secret
// file applies as Apply("secrets", ""), which re-resolves the keys.
func (c *Controller) Watch(ctx context.Context) error {
	live := c.h.Current()
	if live == nil || live.SourcePath == "" {
		return nil
	}
	dir := filepath.Dir(live.SourcePath)
	base := strings.ToLower(filepath.Base(live.SourcePath))
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.Add(dir); err != nil {
		w.Close()
		return err
	}
	secDir := ""
	watchSecrets := func(want string) {
		want = strings.TrimSpace(want)
		if want == secDir {
			return
		}
		if secDir != "" && !samePath(secDir, dir) {
			_ = w.Remove(secDir)
		}
		secDir = ""
		if want == "" {
			return
		}
		if samePath(want, dir) || w.Add(want) == nil {
			secDir = want
		}
	}
	watchSecrets(live.Secrets.Dir)
	go func() {
		defer w.Close()
		timer := time.NewTimer(time.Hour)
		timer.Stop()
		pending := ""
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				evDir := filepath.Dir(ev.Name)
				name := strings.ToLower(filepath.Base(ev.Name))
				switch {
				case samePath(evDir, dir) && (name == base || name == base+".new" || name == base+".prev"):
					if pending == "" {
						pending = "watch"
					}
				case secDir != "" && samePath(evDir, secDir) && !strings.HasPrefix(name, "."):
					pending = "secrets" // dot files are fbmcpctl temp files
				default:
					continue
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(400 * time.Millisecond)
			case <-timer.C:
				if pending == "" {
					continue
				}
				reason := pending
				pending = ""
				_, _ = c.Apply(reason, "")
				if cur := c.h.Current(); cur != nil {
					watchSecrets(cur.Secrets.Dir)
				}
			case <-w.Errors:
			}
		}
	}()
	return nil
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}
