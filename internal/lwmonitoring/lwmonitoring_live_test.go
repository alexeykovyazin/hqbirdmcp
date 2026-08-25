package lwmonitoring

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/aleks/fbmcp/internal/config"
)

// TestQueryLive is P7.1's live verification (phase7_plan.md §7): a real
// fbsvcmgr call against the dev fb5 instance, query levels 1-4, matching how
// backupsvc's liveClient tests skip when the spike environment isn't present.
func TestQueryLive(t *testing.T) {
	const dbFile = `C:/HQbirdData/output/fbmcp-spike/spike_FB5.0.fdb`
	if _, err := os.Stat(dbFile); err != nil {
		t.Skip("spike DB not present")
	}
	inst := config.FBInstance{ID: "fb5", Addr: "localhost:3055", BinDir: "C:/HQbird/Firebird50"}
	if _, err := os.Stat(inst.BinDir + "/fbsvcmgr.exe"); err != nil {
		t.Skip("fbsvcmgr.exe not present")
	}

	for level := MinQuery; level <= MaxQuery; level++ {
		dbPath := ""
		if level >= 2 {
			dbPath = dbFile
		}
		out, err := Query(context.Background(), inst, "SYSDBA", "masterkey", level, dbPath)
		if err != nil {
			t.Fatalf("query level %d: %v", level, err)
		}
		if strings.TrimSpace(out) == "" {
			t.Fatalf("query level %d: empty output - installed fblwmon plugin does not serve this level (D2.1 distinct failure)", level)
		}
		if !strings.Contains(out, "idQuery") {
			t.Errorf("query level %d: output missing idQuery header, got: %s", level, out)
		}
		t.Logf("level %d output: %s", level, out)
	}
}
