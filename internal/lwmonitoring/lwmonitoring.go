// Package lwmonitoring implements P7.1 (phase7_plan.md): the HQBird
// isc_action_svc_lwmonitoring (32) Services-API action, reached via the
// fbsvcmgr subprocess (ADR-028) since the pinned firebirdsql driver has no
// primitive for it — ServiceManager's wire dispatch is unexported and only
// the typed wrappers (Backup/Restore/Sweep/Validate/…) are reachable from
// outside the driver.
package lwmonitoring

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aleks/fbmcp/internal/adminexec"
	"github.com/aleks/fbmcp/internal/config"
)

// MinQuery/MaxQuery are the documented isc_spb_lwm_query levels (1: DB/attachment
// counts, 2: per-database, 3: per-database + transaction/request stats,
// 4: per-attachment within one database — README.lwmonitoring).
const (
	MinQuery = 1
	MaxQuery = 4
)

// Query runs one lwmonitoring service call and returns its raw JSON output.
// dbPath is required for levels 2–4 (isc_spb_dbname scopes the query to one
// database); level 1 ignores it.
func Query(ctx context.Context, inst config.FBInstance, user, pass string, level int, dbPath string) (string, error) {
	if level < MinQuery || level > MaxQuery {
		return "", fmt.Errorf("lwmonitoring: query level must be %d-%d, got %d", MinQuery, MaxQuery, level)
	}
	bin := filepath.Join(inst.BinDir, "fbsvcmgr.exe")
	if _, err := os.Stat(bin); err != nil {
		bin = filepath.Join(inst.BinDir, "fbsvcmgr") // non-Windows layout
	}
	target := "service_mgr"
	if host := hostOf(inst.Addr); host != "" {
		target = host + ":service_mgr"
	}
	args := []string{target, "-user", user, "-action_lwmonitoring", "-lwm_query", strconv.Itoa(level)}
	if dbPath != "" {
		args = append(args, "-dbname", dbPath)
	}
	res := adminexec.Run(ctx, bin, args, 15*time.Second, 64<<10, map[string]string{"ISC_PASSWORD": pass})
	if res.Err != nil && strings.TrimSpace(res.Output) == "" {
		return "", fmt.Errorf("fbsvcmgr: %v", res.Err)
	}
	return strings.TrimSpace(res.Output), nil
}

// hostOf returns the host part of an "addr:port" instance address, or ""
// for local connections (fbsvcmgr's plain "service_mgr" target).
func hostOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host := addr[:i]
		if host != "" && host != "localhost" && host != "127.0.0.1" {
			return host
		}
	}
	return ""
}
