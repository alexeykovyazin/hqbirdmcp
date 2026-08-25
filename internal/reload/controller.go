package reload

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/transport"
)

const DefaultDrainTimeout = 30 * time.Second

// Result is the audited outcome of Apply.
type Result struct {
	Class   string
	Reason  string
	Diff    config.Diff
	Hash    string
	Message string
}

// Hooks are kernel side-effects. Nil funcs are skipped.
type Hooks struct {
	HasLiveBackup     func(dbID string) bool
	HasLiveServiceJob func(instanceID string) bool
	SetDraining       func(ids []string, on bool)
	StopTraces        func(ids []string)
	DropPending       func(dbIDs, dropInstanceIDs []string)
	RemapState        func(oldID, newID string) // newID "" deletes
	WaitIdle          func(ctx context.Context, id, exceptJobID string) error
	CloseDB           func(ids []string)
	InvalidateFacts   func(ids []string)
	SetWebhook        func(url, secretEnv string) error
	ApplyAuth         func(cfg *config.Config) error
	ApplyListener     func(old, new *config.Config) error
	Audit             func(res Result, err error)
}

// Controller serializes reloads against one Handle.
type Controller struct {
	h       *config.Handle
	hooks   Hooks
	timeout time.Duration
	mu      sync.Mutex
}

func New(h *config.Handle, hooks Hooks) *Controller {
	return &Controller{h: h, hooks: hooks, timeout: DefaultDrainTimeout}
}

func (c *Controller) Handle() *config.Handle { return c.h }

func (c *Controller) WithTimeout(d time.Duration) *Controller {
	c.timeout = d
	return c
}

// Apply loads path (or Handle.Current().SourcePath), diffs, drains, swaps.
func (c *Controller) Apply(reason, exceptJobID string) (Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	res, err := c.applyLocked(reason, exceptJobID)
	if c.hooks.Audit != nil {
		c.hooks.Audit(res, err)
	}
	return res, err
}

func (c *Controller) applyLocked(reason, exceptJobID string) (Result, error) {
	live := c.h.Current()
	if live == nil || live.SourcePath == "" {
		return Result{Class: "refuse", Reason: "config source path unavailable"}, fmt.Errorf("config source path unavailable")
	}
	next, err := config.Load(live.SourcePath)
	if err != nil {
		return Result{Class: "refuse", Reason: err.Error()}, err
	}
	hash := config.SnapshotHash(next)
	if hash == c.h.Hash() {
		return Result{Class: "noop", Hash: hash, Message: "already applied"}, nil
	}
	diff := config.CompareSnapshots(live, next)
	if diff.Class == "refuse" {
		return Result{Class: "refuse", Reason: diff.Refuse, Diff: diff, Hash: hash}, fmt.Errorf("%s", diff.Refuse)
	}
	if diff.Class == "noop" {
		c.h.Swap(next)
		return Result{Class: "noop", Hash: hash, Diff: diff, Message: "already applied"}, nil
	}

	if strings.TrimSpace(next.Listen) != "" {
		if err := transport.CheckRemote(next.Listen, next.TLS.Cert, next.TLS.Key, len(next.Identities), len(next.AllowedOrigins)); err != nil {
			return Result{Class: "refuse", Reason: err.Error(), Diff: diff, Hash: hash}, err
		}
	}

	drain := append([]string{}, diff.DrainIDs...)
	if c.hooks.HasLiveBackup != nil {
		for _, id := range diff.DirOnlyIDs {
			if c.hooks.HasLiveBackup(id) {
				drain = append(drain, id)
			}
		}
	}
	if c.hooks.HasLiveServiceJob != nil {
		for _, instID := range diff.ServiceInst {
			if c.hooks.HasLiveServiceJob(instID) {
				drain = append(drain, instID)
				for _, db := range live.Databases {
					if db.Instance == instID {
						drain = append(drain, db.ID)
					}
				}
			}
		}
	}
	drain = uniq(drain)

	if len(drain) > 0 && c.hooks.SetDraining != nil {
		c.hooks.SetDraining(drain, true)
		defer c.hooks.SetDraining(drain, false)
	}

	if len(drain) > 0 && c.hooks.StopTraces != nil {
		c.hooks.StopTraces(drain)
	}

	var dropInst []string
	for id := range indexInst(live) {
		if _, ok := indexInst(next)[id]; !ok {
			dropInst = append(dropInst, id)
		}
	}
	if c.hooks.DropPending != nil {
		c.hooks.DropPending(drain, dropInst)
	}

	if c.hooks.RemapState != nil {
		for oldID, newID := range diff.Renames {
			c.hooks.RemapState(oldID, newID)
		}
		for _, id := range diff.RemovedDBs {
			c.hooks.RemapState(id, "")
		}
	}

	if len(drain) > 0 && c.hooks.WaitIdle != nil {
		ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
		defer cancel()
		for _, id := range drain {
			if err := c.hooks.WaitIdle(ctx, id, exceptJobID); err != nil {
				return Result{Class: "refuse", Reason: "drain timeout for " + id, Diff: diff, Hash: hash}, fmt.Errorf("reload drain timeout for %s: %w", id, err)
			}
		}
	}

	closeIDs := append([]string{}, diff.CloseIDs...)
	for _, id := range drain {
		closeIDs = append(closeIDs, id)
	}
	closeIDs = uniq(closeIDs)
	if c.hooks.CloseDB != nil && len(closeIDs) > 0 {
		c.hooks.CloseDB(closeIDs)
	}

	c.h.Swap(next)

	if c.hooks.InvalidateFacts != nil && len(closeIDs) > 0 {
		c.hooks.InvalidateFacts(closeIDs)
	}
	if diff.Notify && c.hooks.SetWebhook != nil {
		if err := c.hooks.SetWebhook(next.Notify.WebhookURL, next.Notify.WebhookSecretEnv); err != nil {
			// snapshot already swapped; report but don't roll back (fail-open on notify)
			return Result{Class: "apply", Diff: diff, Hash: hash, Reason: reason, Message: "applied; webhook update: " + err.Error()}, nil
		}
	}
	if diff.Auth && c.hooks.ApplyAuth != nil {
		if err := c.hooks.ApplyAuth(next); err != nil {
			return Result{Class: "apply", Diff: diff, Hash: hash, Reason: reason, Message: "applied; auth update: " + err.Error()}, err
		}
	}
	if diff.Listener && c.hooks.ApplyListener != nil {
		if err := c.hooks.ApplyListener(live, next); err != nil {
			return Result{Class: "apply", Diff: diff, Hash: hash, Reason: reason, Message: "applied; listener: " + err.Error()}, err
		}
	}
	return Result{
		Class:   "apply",
		Reason:  reason,
		Diff:    diff,
		Hash:    hash,
		Message: summarize(diff),
	}, nil
}

func summarize(d config.Diff) string {
	var b strings.Builder
	b.WriteString("reload applied")
	if len(d.AddedDBs) > 0 {
		fmt.Fprintf(&b, " added=%s", strings.Join(d.AddedDBs, ","))
	}
	if len(d.RemovedDBs) > 0 {
		fmt.Fprintf(&b, " removed=%s", strings.Join(d.RemovedDBs, ","))
	}
	if len(d.Renames) > 0 {
		fmt.Fprintf(&b, " renamed=%d", len(d.Renames))
	}
	if d.Listener {
		b.WriteString(" listener")
	}
	if d.Auth {
		b.WriteString(" auth")
	}
	return b.String()
}

func indexInst(c *config.Config) map[string]config.FBInstance {
	m := map[string]config.FBInstance{}
	if c == nil {
		return m
	}
	for _, in := range c.Instances {
		m[in.ID] = in
	}
	return m
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
