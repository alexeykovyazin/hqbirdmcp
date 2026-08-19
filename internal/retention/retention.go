// Package retention executes ADR-016: delete only catalog-verified artifacts
// past keep_days. Uncataloged files are never touched. keep_days=0 means
// keep-everything (plan is empty).
package retention

import (
	"fmt"
	"os"
	"time"

	"github.com/aleks/fbmcp/internal/state"
)

// Action is one proposed deletion.
type Action struct {
	CatalogID string
	Path      string
	AgeHours  float64
}

// Plan lists catalog-verified artifacts older than keepDays. keepDays<=0
// yields an empty plan (keep-everything default).
func Plan(st *state.Store, keepDays int, now time.Time) []Action {
	if keepDays <= 0 {
		return nil
	}
	cutoff := now.Add(-time.Duration(keepDays) * 24 * time.Hour)
	var out []Action
	for _, e := range st.Catalog() {
		if !e.Verified || e.Path == "" {
			continue
		}
		if !e.CreatedAt.Before(cutoff) {
			continue
		}
		out = append(out, Action{
			CatalogID: e.ID,
			Path:      e.Path,
			AgeHours:  now.Sub(e.CreatedAt).Hours(),
		})
	}
	return out
}

// Execute deletes planned files and drops catalog rows. dryRun lists only.
// A missing file still drops the catalog row (artifact already gone).
func Execute(st *state.Store, plan []Action, dryRun bool) (report string, err error) {
	if dryRun {
		if len(plan) == 0 {
			return "dry-run: nothing to delete (keep-everything or no aged verified artifacts)", nil
		}
		r := fmt.Sprintf("dry-run: would delete %d catalog-verified artifact(s):\n", len(plan))
		for _, a := range plan {
			r += fmt.Sprintf("- %s (%s, age %.0fh)\n", a.Path, a.CatalogID, a.AgeHours)
		}
		return r, nil
	}
	deleted := 0
	for _, a := range plan {
		_ = os.Remove(a.Path) // ignore not-exist
		if err := st.RemoveCatalogEntry(a.CatalogID); err != nil {
			return "", err
		}
		deleted++
	}
	return fmt.Sprintf("deleted %d catalog-verified artifact(s)", deleted), nil
}
