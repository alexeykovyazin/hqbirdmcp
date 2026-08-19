// Package backupsvc implements P3.1/P3.2/P3.4 on the ADR-003 API-first
// route: backup / restore / validate / sweep via the driver's Services API,
// plus the backup-catalog facts provider (K2) that unlocks Tier-2
// preconditions (backup_freshness, verified_backup_exists).
package backupsvc

import (
	"context"
	"fmt"
	"time"

	fb "github.com/nakagami/firebirdsql"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/state"
)

// Client carries connection coordinates for one instance.
type Client struct {
	Addr, User, Pass string
	opts             fb.ServiceManagerOptions
}

func NewClient(inst config.FBInstance, user, pass string) *Client {
	return &Client{Addr: inst.Addr, User: user, Pass: pass, opts: fb.NewServiceManagerOptions()}
}

// Backup runs a full gbak backup (blocking; call from a job). Progress lines
// are delivered while running. parallelWorkers is HQBird/FB5's -par / -parallel
// thread count (P7.2, phase7_plan.md); 0 means the driver default (no
// parallelism) — mirrors the driver's own WithoutBackupParallelWorkers().
func (c *Client) Backup(dbFile, backupFile string, parallelWorkers int32, progress func(string)) error {
	bm, err := fb.NewBackupManager(c.Addr, c.User, c.Pass, c.opts)
	if err != nil {
		return err
	}
	opts := fb.GetDefaultBackupOptions()
	opts.ParallelWorkers = parallelWorkers
	ch := make(chan string, 256)
	done := make(chan error, 1)
	dctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go drainUntil(dctx, ch, progress)
	go func() { done <- bm.Backup(dbFile, backupFile, opts, ch) }()
	return <-done
}

// Restore restores a backup file into dbFile (blocking). replace allows
// overwriting an existing database file (gbak -REP equivalent).
// parallelWorkers is HQBird/FB5's -par / -parallel thread count for index
// creation during restore (P7.2); 0 means no parallelism.
func (c *Client) Restore(backupFile, dbFile string, replace bool, parallelWorkers int32, progress func(string)) error {
	bm, err := fb.NewBackupManager(c.Addr, c.User, c.Pass, c.opts)
	if err != nil {
		return err
	}
	opts := fb.GetDefaultRestoreOptions()
	opts.Replace = replace
	opts.ParallelWorkers = parallelWorkers
	ch := make(chan string, 256)
	done := make(chan error, 1)
	dctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go drainUntil(dctx, ch, progress)
	go func() { done <- bm.Restore(backupFile, dbFile, opts, ch) }()
	return <-done
}

// Validate runs gfix-style validation (options: 0 = default checks).
func (c *Client) Validate(dbFile string, options int) error {
	mm, err := fb.NewMaintenanceManager(c.Addr, c.User, c.Pass, c.opts)
	if err != nil {
		return err
	}
	return mm.Validate(dbFile, options)
}

// Sweep runs a manual sweep.
func (c *Client) Sweep(dbFile string) error {
	mm, err := fb.NewMaintenanceManager(c.Addr, c.User, c.Pass, c.opts)
	if err != nil {
		return err
	}
	return mm.Sweep(dbFile)
}

// SetWriteModeSync/Async implement the gfix -writes family (op 62).
func (c *Client) SetForceWrite(dbFile string, on bool) error {
	mm, err := fb.NewMaintenanceManager(c.Addr, c.User, c.Pass, c.opts)
	if err != nil {
		return err
	}
	if on {
		return mm.SetWriteModeSync(dbFile)
	}
	return mm.SetWriteModeAsync(dbFile)
}

// SetReadOnly implements gfix -mode read_only / read_write (op 61).
func (c *Client) SetReadOnly(dbFile string, ro bool) error {
	mm, err := fb.NewMaintenanceManager(c.Addr, c.User, c.Pass, c.opts)
	if err != nil {
		return err
	}
	if ro {
		return mm.SetAccessModeReadOnly(dbFile)
	}
	return mm.SetAccessModeReadWrite(dbFile)
}

// NBackup runs an incremental nbackup at the given level (0–2).
func (c *Client) NBackup(dbFile, backupFile string, level int, progress func(string)) error {
	nm, err := fb.NewNBackupManager(c.Addr, c.User, c.Pass, c.opts)
	if err != nil {
		return err
	}
	opts := fb.GetDefaultNBackupOptions()
	opts.Level = int32(level)
	ch := make(chan string, 256)
	done := make(chan error, 1)
	dctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go drainUntil(dctx, ch, progress)
	go func() { done <- nm.Backup(dbFile, backupFile, opts, ch) }()
	return <-done
}

// drainUntil reads the Services-API verbose channel until ctx is done or
// the channel closes. After cancel it non-blockingly drains the buffer so
// a late send cannot fill the channel, then returns (C8: goroutine bounded;
// the driver never closes the channel).
func drainUntil(ctx context.Context, ch <-chan string, progress func(string)) {
	for {
		select {
		case <-ctx.Done():
			for {
				select {
				case msg, ok := <-ch:
					if !ok {
						return
					}
					if progress != nil {
						progress(msg)
					}
				default:
					return
				}
			}
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if progress != nil {
				progress(msg)
			}
		}
	}
}

// Catalog implements the K2 facts: backup_freshness (hours since newest
// verified backup) and verified_backup_exists, fail-closed on empty catalog.
type Catalog struct {
	st *state.Store
}

func NewCatalog(st *state.Store) *Catalog { return &Catalog{st: st} }

// Register records a completed backup artifact (verified=true after a
// successful test-restore).
func (c *Catalog) Register(dbID, path string, verified bool) error {
	return c.RegisterKind(dbID, path, verified, "gbak", 0)
}

func (c *Catalog) RegisterKind(dbID, path string, verified bool, kind string, level int) error {
	return c.st.AddCatalogEntry(state.CatalogEntry{
		ID: fmt.Sprintf("b%d", time.Now().UnixNano()), Database: dbID, Path: path,
		CreatedAt: time.Now().UTC(), Verified: verified, Kind: kind, Level: level,
	})
}

func (c *Catalog) Fact(fc state.FactContext, name string, _ map[string]string) (any, error) {
	at, ok := c.st.LatestVerifiedBackup(fc.Database)
	switch name {
	case "backup_freshness":
		if !ok {
			return 1e9, nil // infinitely stale → precondition fails
		}
		return time.Since(at).Hours(), nil
	case "verified_backup_exists":
		return ok, nil
	}
	return nil, fmt.Errorf("catalog: unknown fact %q", name)
}
