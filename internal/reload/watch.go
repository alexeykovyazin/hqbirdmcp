package reload

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch directory of the live SourcePath; debounce then Apply("watch", "").
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
	go func() {
		defer w.Close()
		timer := time.NewTimer(time.Hour)
		timer.Stop()
		pending := false
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				name := strings.ToLower(filepath.Base(ev.Name))
				if name != base && name != base+".new" && name != base+".prev" {
					continue
				}
				pending = true
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(400 * time.Millisecond)
			case <-timer.C:
				if !pending {
					continue
				}
				pending = false
				_, _ = c.Apply("watch", "")
			case <-w.Errors:
			}
		}
	}()
	return nil
}
