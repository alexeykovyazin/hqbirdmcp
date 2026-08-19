package config

import (
	"fmt"
	"strings"
)

// Diff is the classification of a new snapshot versus the live one.
type Diff struct {
	Class  string // noop | apply | refuse
	Refuse string

	AddedDBs   []string
	RemovedDBs []string
	Renames    map[string]string // old id → new id

	// DrainIDs are job/pool keys that must go idle then CloseDB (DSN or remove/rename).
	DrainIDs []string
	// DirOnlyIDs changed backup_dir/work_dir only — drain if a backup/restore job is live.
	DirOnlyIDs []string
	// ServiceInst instance ids whose service/user/secret changed — drain if fb_service_* is live.
	ServiceInst []string
	// CloseIDs always get CloseDB (drain set plus removed/renamed-from).
	CloseIDs []string

	Notify   bool
	Listener bool
	Auth     bool
}

// CompareSnapshots diffs two Load()ed configs. Nil and empty slices are equal.
func CompareSnapshots(oldC, newC *Config) Diff {
	if oldC == nil {
		return Diff{Class: "refuse", Refuse: "no live config"}
	}
	if newC == nil {
		return Diff{Class: "refuse", Refuse: "new config is nil"}
	}
	if SnapshotHash(oldC) == SnapshotHash(newC) {
		return Diff{Class: "noop"}
	}
	if oldC.State.Dir != newC.State.Dir {
		return Diff{Class: "refuse", Refuse: "state.dir cannot be reloaded in-process (D8)"}
	}
	if dup := duplicatePaths(newC); dup != "" {
		return Diff{Class: "refuse", Refuse: dup}
	}

	d := Diff{Class: "apply", Renames: map[string]string{}}
	oldDB := indexDBs(oldC)
	newDB := indexDBs(newC)
	oldInst := indexInst(oldC)
	newInst := indexInst(newC)

	gone := []string{}
	added := []string{}
	for id := range oldDB {
		if _, ok := newDB[id]; !ok {
			gone = append(gone, id)
		}
	}
	for id := range newDB {
		if _, ok := oldDB[id]; !ok {
			added = append(added, id)
		}
	}

	// Strict rename: exactly one gone, exactly one added, unique shared path.
	if len(gone) == 1 && len(added) == 1 {
		g, a := gone[0], added[0]
		gp, ap := NormalizeDBPath(oldDB[g].Path), NormalizeDBPath(newDB[a].Path)
		if gp != "" && gp == ap && uniquePath(oldC, gp) && uniquePath(newC, ap) {
			d.Renames[g] = a
			d.DrainIDs = append(d.DrainIDs, g)
			d.CloseIDs = append(d.CloseIDs, g)
			gone, added = nil, nil
		}
	}
	d.RemovedDBs = gone
	d.AddedDBs = added
	for _, id := range gone {
		d.DrainIDs = append(d.DrainIDs, id)
		d.CloseIDs = append(d.CloseIDs, id)
	}

	for id, nd := range newDB {
		od, ok := oldDB[id]
		if !ok {
			continue
		}
		if dsnChanged(od, nd, oldInst, newInst) {
			d.DrainIDs = append(d.DrainIDs, id)
			d.CloseIDs = append(d.CloseIDs, id)
			continue
		}
		if od.BackupDir != nd.BackupDir || od.WorkDir != nd.WorkDir {
			d.DirOnlyIDs = append(d.DirOnlyIDs, id)
		}
	}

	for id, ni := range newInst {
		oi, ok := oldInst[id]
		if !ok {
			continue
		}
		if oi.Addr != ni.Addr || oi.BinDir != ni.BinDir {
			for _, db := range newC.Databases {
				if db.Instance == id {
					d.DrainIDs = append(d.DrainIDs, db.ID)
					d.CloseIDs = append(d.CloseIDs, db.ID)
				}
			}
			d.DrainIDs = append(d.DrainIDs, id) // jobs keyed by instance id
			continue
		}
		if oi.Service != ni.Service || oi.ServiceUser != ni.ServiceUser || oi.ServiceSecretEnv != ni.ServiceSecretEnv {
			d.ServiceInst = append(d.ServiceInst, id)
		}
	}

	if oldC.Notify.WebhookURL != newC.Notify.WebhookURL || oldC.Notify.WebhookSecretEnv != newC.Notify.WebhookSecretEnv {
		d.Notify = true
	}
	if strings.TrimSpace(oldC.Listen) != strings.TrimSpace(newC.Listen) || oldC.TLS.Cert != newC.TLS.Cert || oldC.TLS.Key != newC.TLS.Key {
		d.Listener = true
	}
	if !identitiesEqual(oldC.Identities, newC.Identities) {
		d.Auth = true
	}

	d.DrainIDs = uniq(d.DrainIDs)
	d.CloseIDs = uniq(d.CloseIDs)
	d.DirOnlyIDs = uniq(d.DirOnlyIDs)
	d.ServiceInst = uniq(d.ServiceInst)
	return d
}

func dsnChanged(od, nd Database, oldInst, newInst map[string]FBInstance) bool {
	if NormalizeDBPath(od.Path) != NormalizeDBPath(nd.Path) {
		return true
	}
	if od.Instance != nd.Instance || od.ROUser != nd.ROUser || od.ROSecretEnv != nd.ROSecretEnv ||
		od.AdminUser != nd.AdminUser || od.AdminSecretEnv != nd.AdminSecretEnv {
		return true
	}
	return false
}

func indexDBs(c *Config) map[string]Database {
	m := map[string]Database{}
	for _, db := range c.Databases {
		m[db.ID] = db
	}
	return m
}

func indexInst(c *Config) map[string]FBInstance {
	m := map[string]FBInstance{}
	for _, in := range c.Instances {
		m[in.ID] = in
	}
	return m
}

func uniquePath(c *Config, np string) bool {
	n := 0
	for _, db := range c.Databases {
		if NormalizeDBPath(db.Path) == np {
			n++
		}
	}
	return n == 1
}

func duplicatePaths(c *Config) string {
	seen := map[string]string{}
	for _, db := range c.Databases {
		np := NormalizeDBPath(db.Path)
		if np == "" {
			continue
		}
		if other, ok := seen[np]; ok {
			return fmt.Sprintf("duplicate database path %q (%s and %s)", db.Path, other, db.ID)
		}
		seen[np] = db.ID
	}
	return ""
}

func identitiesEqual(a, b []APIIdentity) bool {
	if len(a) != len(b) {
		return false
	}
	type key struct {
		n, e string
		t    int
	}
	ma := map[key]int{}
	for _, id := range a {
		ma[key{id.Name, id.KeyEnv, id.MaxTier}]++
	}
	for _, id := range b {
		k := key{id.Name, id.KeyEnv, id.MaxTier}
		ma[k]--
		if ma[k] < 0 {
			return false
		}
	}
	return true
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
