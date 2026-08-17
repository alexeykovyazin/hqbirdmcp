// Package facts implements real facts providers (P2.1+) on top of the P1.8
// interface. EngineFacts supplies engine version / ODS / page size etc. from
// MON$DATABASE (read pool) and the Services API (version string), and feeds
// the policy engine's version gating.
package facts

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/dbpool"
	"github.com/aleks/fbmcp/internal/state"
)

// EngineFacts provides `engine_version`, `ods`, `page_size`, `read_only`,
// `forced_writes`, `sql_dialect` per database, cached briefly.
type EngineFacts struct {
	cfg   *config.Config
	pools *dbpool.Manager
	mu    sync.Mutex
	cache map[string]cached
	ttl   time.Duration
}

type cached struct {
	at    time.Time
	facts map[string]any
}

func NewEngineFacts(cfg *config.Config, pools *dbpool.Manager) *EngineFacts {
	return &EngineFacts{cfg: cfg, pools: pools, cache: map[string]cached{}, ttl: 30 * time.Second}
}

// Fact implements state.FactsProvider (fail-closed on unknown names).
func (e *EngineFacts) Fact(fc state.FactContext, name string, _ map[string]string) (any, error) {
	m, err := e.snapshot(context.Background(), fc.Database)
	if err != nil {
		return nil, err
	}
	v, ok := m[name]
	if !ok {
		return nil, fmt.Errorf("unknown fact %q for database %q", name, fc.Database)
	}
	return v, nil
}

// Snapshot returns all engine facts for a database (fb_info's data source).
func (e *EngineFacts) Snapshot(ctx context.Context, dbID string) (map[string]any, error) {
	return e.snapshot(ctx, dbID)
}

func (e *EngineFacts) snapshot(ctx context.Context, dbID string) (map[string]any, error) {
	e.mu.Lock()
	if c, ok := e.cache[dbID]; ok && time.Since(c.at) < e.ttl {
		e.mu.Unlock()
		return c.facts, nil
	}
	e.mu.Unlock()

	db, err := e.cfg.DB(dbID)
	if err != nil {
		return nil, err
	}
	inst, err := e.cfg.Instance(db.Instance)
	if err != nil {
		return nil, err
	}

	out := map[string]any{}
	// engine version via Services API (P0.1 finding: no SQL column exists)
	if v, err := serviceVersion(inst.Addr, db.AdminUser, db.AdminSecretEnv); err == nil {
		out["engine_version_full"] = v
		out["engine_version"] = majorMinor(v)
	} else {
		out["engine_version_error"] = err.Error() // degraded, not fatal: MON$ below still works
	}

	// MON$DATABASE core facts via the READ pool (Tier-0 discipline)
	pool, err := e.pools.ReadPool(ctx, dbID)
	if err != nil {
		return nil, err
	}
	var (
		pageSize, odsMajor, odsMinor, dialect   int
		readOnly, forcedWrites                  bool
		sweepInterval                           int
	)
	err = pool.QueryRowContext(ctx, `SELECT MON$PAGE_SIZE, MON$ODS_MAJOR, MON$ODS_MINOR,
		MON$SQL_DIALECT, MON$READ_ONLY, MON$FORCED_WRITES, MON$SWEEP_INTERVAL FROM MON$DATABASE`).
		Scan(&pageSize, &odsMajor, &odsMinor, &dialect, &readOnly, &forcedWrites, &sweepInterval)
	if err != nil {
		return nil, fmt.Errorf("MON$DATABASE: %w", err)
	}
	out["page_size"] = pageSize
	out["ods"] = fmt.Sprintf("%d.%d", odsMajor, odsMinor)
	out["ods_major"], out["ods_minor"] = odsMajor, odsMinor
	out["sql_dialect"] = dialect
	out["read_only"] = readOnly
	out["forced_writes"] = forcedWrites
	out["sweep_interval"] = sweepInterval

	e.mu.Lock()
	e.cache[dbID] = cached{at: time.Now(), facts: out}
	e.mu.Unlock()
	return out, nil
}

func serviceVersion(addr, user, secretEnv string) (string, error) {
	pass, err := config.SecretFromEnv(secretEnv)
	if err != nil {
		return "", err
	}
	// Local import cycle avoidance: use the driver directly.
	svc, err := newSvcMgr(addr, user, pass)
	if err != nil {
		return "", err
	}
	defer svc.Close()
	return svc.Version()
}

// majorMinor extracts "5.0" from "WI-V5.0.5.1876 Firebird 5.0 HQbird".
func majorMinor(v string) string {
	low := strings.ToLower(v)
	i := strings.Index(low, "v")
	if i < 0 {
		i = 0
	}
	rest := v[i:]
	end := strings.IndexAny(rest, " ")
	if end < 0 {
		end = len(rest)
	}
	ver := strings.TrimLeft(rest[:end], "vV")
	parts := strings.SplitN(ver, ".", 3)
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return ver
}
