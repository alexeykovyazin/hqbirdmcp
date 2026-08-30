// Package dbpool implements the P1.2 connection layer: two physically
// separate pools per database — a read pool (RO user, read-only TPB on every
// transaction) and an admin pool (gated credentials). Read-only is enforced
// by the Firebird engine, not by query filtering (main plan §2.1, §4.2;
// spike-verified on FB 2.5–5.0).
package dbpool

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "github.com/nakagami/firebirdsql"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/killpoint"
)

// Manager owns the pools for every registered database.
type Manager struct {
	cfg   registry
	mu    sync.Mutex
	read  map[string]*sql.DB // db id -> read pool
	admin map[string]*sql.DB // db id -> admin pool
}

type registry interface {
	DB(id string) (config.Database, error)
	Instance(id string) (config.FBInstance, error)
}

func NewManager(cfg registry) *Manager {
	return &Manager{
		cfg:   cfg,
		read:  map[string]*sql.DB{},
		admin: map[string]*sql.DB{},
	}
}

func dsn(addr, path, user, pass string) string {
	return fmt.Sprintf("%s:%s@%s/%s?charset=UTF8", user, pass, addr, path)
}

// ReadPool returns the read-only pool for a registry DB id. Every transaction
// opened through ReadOnly() carries the read-only TPB.
func (m *Manager) ReadPool(ctx context.Context, dbID string) (*sql.DB, error) {
	db, err := m.get(ctx, dbID, false)
	if err != nil {
		return nil, err
	}
	return db, nil
}

// ReadOnly opens a read-only, read-committed transaction on the read pool.
// The engine refuses any write attempted on it (CI fuse #1 proves it).
func (m *Manager) ReadOnly(ctx context.Context, dbID string) (*sql.Tx, error) {
	pool, err := m.ReadPool(ctx, dbID)
	if err != nil {
		return nil, err
	}
	return pool.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
}

// AdminPool returns the admin pool — only the mutation layer (post-policy,
// post-gate) may call this. Tier-0 tools must never reach it (import boundary
// tested in Phase 2).
func (m *Manager) AdminPool(ctx context.Context, dbID string) (*sql.DB, error) {
	return m.get(ctx, dbID, true)
}

func (m *Manager) get(ctx context.Context, dbID string, admin bool) (*sql.DB, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pools := m.read
	role := "read"
	if admin {
		pools = m.admin
		role = "admin"
	}
	if db, ok := pools[dbID]; ok {
		return db, nil
	}
	rcfg, err := m.cfg.DB(dbID)
	if err != nil {
		return nil, err
	}
	inst, err := m.cfg.Instance(rcfg.Instance)
	if err != nil {
		return nil, err
	}
	user, env := rcfg.ROUser, rcfg.ROSecretEnv
	if admin {
		user, env = rcfg.AdminUser, rcfg.AdminSecretEnv
	}
	pass, err := config.SecretFromEnv(env)
	if err != nil {
		return nil, fmt.Errorf("%s pool for %q: %w", role, dbID, err)
	}
	pool, err := sql.Open("firebirdsql", dsn(inst.Addr, rcfg.Path, user, pass))
	if err != nil {
		return nil, fmt.Errorf("%s pool open %q: %w", role, dbID, err)
	}
	pool.SetMaxOpenConns(4)
	pool.SetConnMaxIdleTime(5 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.PingContext(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("%s pool ping %q: %w", role, dbID, err) // degraded per-DB, never server-wide
	}
	pools[dbID] = pool
	return pool, nil
}

// Health probes a database; used for per-DB degraded mode (P1.1/P2.1).
func (m *Manager) Health(ctx context.Context, dbID string) error {
	pool, err := m.ReadPool(ctx, dbID)
	if err != nil {
		return err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return pool.PingContext(pingCtx)
}

// Close closes all pools.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.read {
		p.Close()
	}
	for _, p := range m.admin {
		p.Close()
	}
	m.read = map[string]*sql.DB{}
	m.admin = map[string]*sql.DB{}
}

// CloseDB closes both pools for one database (used by guarded restore before
// replacing the file).
func (m *Manager) CloseDB(dbID string) {
	killpoint.Hit("db.closedb") // chaos harness (P6.2 T6 / P3 finding #3): kill during CloseDB in restore/shutdown
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.read[dbID]; ok {
		p.Close()
		delete(m.read, dbID)
	}
	if p, ok := m.admin[dbID]; ok {
		p.Close()
		delete(m.admin, dbID)
	}
}

// CloseRead closes only the read pool for one database. Used by fb_query's
// fb_write fallback: a procedure the engine refused on the read-only
// transaction leaves its compiled statement pinning the procedure's target
// objects on that attachment (driver-level: the handle survives stmt.Close
// and rollback), blocking DDL on them until idle reaping. Draining the
// stateless read pool releases the pins immediately; read tools reopen on
// next use.
func (m *Manager) CloseRead(dbID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.read[dbID]; ok {
		p.Close()
		delete(m.read, dbID)
	}
}
