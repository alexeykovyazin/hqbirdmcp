// Package backupsvc implements P3.1/P3.2/P3.4 on the ADR-003 API-first
// route: backup / restore / validate / sweep via the driver's Services API,
// plus the backup-catalog facts provider (K2) that unlocks Tier-2
// preconditions (backup_freshness, verified_backup_exists).
package backupsvc

import (
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
// are delivered while running.
func (c *Client) Backup(dbFile, backupFile string, progress func(string)) error {
	bm, err := fb.NewBackupManager(c.Addr, c.User, c.Pass, c.opts)
	if err != nil {
		return err
	}
	ch := make(chan string, 256)
	done := make(chan error, 1)
	go func() { done <- bm.Backup(dbFile, backupFile, fb.GetDefaultBackupOptions(), ch) }()
	// NOTE: the driver never closes the verbose channel — drain in background
	go func() {
		for msg := range ch {
			if progress != nil {
				progress(msg)
			}
		}
	}()
	return <-done
}

// Restore restores a backup file into dbFile (blocking). replace allows
// overwriting an existing database file (gbak -REP equivalent).
func (c *Client) Restore(backupFile, dbFile string, replace bool, progress func(string)) error {
	bm, err := fb.NewBackupManager(c.Addr, c.User, c.Pass, c.opts)
	if err != nil {
		return err
	}
	opts := fb.GetDefaultRestoreOptions()
	opts.Replace = replace
	ch := make(chan string, 256)
	done := make(chan error, 1)
	go func() { done <- bm.Restore(backupFile, dbFile, opts, ch) }()
	go func() {
		for msg := range ch {
			if progress != nil {
				progress(msg)
			}
		}
	}()
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

// Catalog implements the K2 facts: backup_freshness (hours since newest
// verified backup) and verified_backup_exists, fail-closed on empty catalog.
type Catalog struct {
	st *state.Store
}

func NewCatalog(st *state.Store) *Catalog { return &Catalog{st: st} }

// Register records a completed backup artifact (verified=true after a
// successful test-restore).
func (c *Catalog) Register(dbID, path string, verified bool) error {
	return c.st.AddCatalogEntry(state.CatalogEntry{
		ID: fmt.Sprintf("b%d", time.Now().UnixNano()), Database: dbID, Path: path,
		CreatedAt: time.Now().UTC(), Verified: verified,
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
