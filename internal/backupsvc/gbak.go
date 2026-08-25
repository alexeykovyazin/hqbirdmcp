package backupsvc

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"time"

	"github.com/aleks/fbmcp/internal/adminexec"
	"github.com/aleks/fbmcp/internal/config"
)

// RestoreNoMatviews restores via the gbak CLI with -NO_MATVIEWS (phase8_plan
// D2.2 / ADR-027 follow-up): the pinned driver has no isc_spb_res_no_matviews
// field, so the Services-API path cannot express the option. HQBird's FB 5.0
// gbak accepts it; older engines have no materialized views to skip, so the
// caller gates on version 5.0. Credentials travel via ISC_* env only.
func RestoreNoMatviews(ctx context.Context, inst config.FBInstance, user, pass, backupFile, dbFile string, replace bool, prog func(string)) error {
	name := "gbak"
	if runtime.GOOS == "windows" {
		name = "gbak.exe"
	}
	mode := "-C"
	if replace {
		mode = "-REP"
	}
	// -SE routes through the service manager (same channel the driver's
	// Services API uses); a direct remote target instead triggers the
	// client-side engine-plugin scan, which is broken on some HQBird hosts
	// (verified live: "module could not be found" from MySQLEngine).
	args := []string{"-SE", inst.Addr + ":service_mgr", mode, "-NO_MATVIEWS", backupFile, filepath.ToSlash(dbFile)}
	if prog != nil {
		prog(fmt.Sprintf("gbak %s -NO_MATVIEWS (no-matviews falls back to the CLI: ADR-028 pattern)", mode))
	}
	res := adminexec.Run(ctx, filepath.Join(inst.BinDir, name), args, 30*time.Minute, 4<<20,
		map[string]string{"ISC_USER": user, "ISC_PASSWORD": pass})
	if res.Err != nil {
		return fmt.Errorf("gbak -NO_MATVIEWS: %w (%s)", res.Err, res.Output)
	}
	return nil
}
