// Package queryplan implements the ADR-013 route (isql subprocess): plan
// retrieval for P2.4. Statements are written to a temp file, isql runs with
// SET PLANONLY (statement text is echoed before the plan), credentials via
// env only.
package queryplan

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aleks/fbmcp/internal/adminexec"
	"github.com/aleks/fbmcp/internal/config"
)

// Explain returns the access plan (and, on FB 4+, optionally the EXPLAIN
// form) for one SELECT statement.
func Explain(ctx context.Context, inst config.FBInstance, db config.Database, pass, query string, explain bool) (string, error) {
	if len(query) > 1<<20 {
		return "", fmt.Errorf("query too large")
	}
	tmp, err := os.CreateTemp("", "fbmcp-plan-*.sql")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	set := "SET PLANONLY;"
	if explain {
		set = "SET EXPLAIN;" // FB 4.0+; falls back gracefully on 3.0 (isql error captured)
	}
	body := fmt.Sprintf("SET HEADING OFF;\n%s\n%s;\n", set, query)
	if err := os.WriteFile(tmp.Name(), []byte(body), 0o600); err != nil {
		return "", err
	}
	bin := filepath.Join(inst.BinDir, "isql.exe")
	if _, err := os.Stat(bin); err != nil {
		// non-Windows layout
		bin = filepath.Join(inst.BinDir, "isql")
	}
	res := adminexec.Run(ctx, bin,
		[]string{"-i", tmp.Name(), "-user", db.AdminUser, "-q", fmt.Sprintf("localhost/%s:%s", portOf(inst.Addr), db.Path)},
		60*time.Second, 4<<20, map[string]string{"ISC_PASSWORD": pass})
	if res.Err != nil && res.Output == "" {
		return "", fmt.Errorf("isql: %v", res.Err)
	}
	return extractPlan(res.Output), nil
}

var planRe = regexp.MustCompile(`(?is)PLAN\s*\(.*`) // from first PLAN to end (PLANONLY prints only it)

func extractPlan(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "PLAN ") {
			return strings.Join(strings.SplitN(out, strings.TrimSpace(line), 2), "")[:0] + firstPlans(out)
		}
	}
	return strings.TrimSpace(out)
}

func firstPlans(out string) string {
	var b strings.Builder
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "PLAN") || b.Len() > 0 && t != "" && !strings.HasPrefix(t, "SQL>") {
			b.WriteString(t + "\n")
		}
	}
	return b.String()
}

func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i+1:]
	}
	return "3050"
}
