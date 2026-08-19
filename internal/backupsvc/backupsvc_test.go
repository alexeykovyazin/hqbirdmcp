package backupsvc

import (
	"os"
	"testing"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/state"
)

func liveClient(t *testing.T) *Client {
	t.Helper()
	dbFile := `C:/HQbirdData/output/fbmcp-spike/spike_FB5.0.fdb`
	if _, err := os.Stat(dbFile); err != nil {
		t.Skip("spike DB not present")
	}
	return NewClient(config.FBInstance{ID: "fb5", Addr: "localhost:3055"}, "SYSDBA", "masterkey")
}

func TestBackupRestoreValidateLive(t *testing.T) {
	c := liveClient(t)
	src := `C:/HQbirdData/output/fbmcp-spike/spike_FB5.0.fdb`
	fbk := `C:/HQbirdData/output/fbmcp-spike/test_p3.fbk`
	restored := `C:/HQbirdData/output/fbmcp-spike/test_p3_restored.fdb`
	os.Remove(fbk)
	os.Remove(restored)

	if err := c.Backup(src, fbk, 0, func(m string) {}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if _, err := os.Stat(fbk); err != nil {
		t.Fatalf("backup file missing: %v", err)
	}
	if err := c.Restore(fbk, restored, false, 0, func(m string) {}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := os.Stat(restored); err != nil {
		t.Fatalf("restored file missing: %v", err)
	}
	if err := c.Validate(restored, 0); err != nil {
		t.Fatalf("validate: %v", err)
	}
	os.Remove(fbk)
	os.Remove(restored)
}

// TestBackupRestoreParallelLive is P7.2 (phase7_plan.md): confirms a
// non-zero ParallelWorkers round-trips through the driver without error.
// Real speedup validation needs a much larger database than the spike DBs;
// this only proves the option is wired correctly end to end.
func TestBackupRestoreParallelLive(t *testing.T) {
	c := liveClient(t)
	src := `C:/HQbirdData/output/fbmcp-spike/spike_FB5.0.fdb`
	fbk := `C:/HQbirdData/output/fbmcp-spike/test_p7_2_parallel.fbk`
	restored := `C:/HQbirdData/output/fbmcp-spike/test_p7_2_parallel_restored.fdb`
	os.Remove(fbk)
	os.Remove(restored)
	defer os.Remove(fbk)
	defer os.Remove(restored)

	if err := c.Backup(src, fbk, 4, func(m string) {}); err != nil {
		t.Fatalf("parallel backup: %v", err)
	}
	if _, err := os.Stat(fbk); err != nil {
		t.Fatalf("backup file missing: %v", err)
	}
	if err := c.Restore(fbk, restored, false, 4, func(m string) {}); err != nil {
		t.Fatalf("parallel restore: %v", err)
	}
	if _, err := os.Stat(restored); err != nil {
		t.Fatalf("restored file missing: %v", err)
	}
}

func TestCatalogFacts(t *testing.T) {
	st, _ := state.Open(t.TempDir())
	cat := NewCatalog(st)
	// fail-closed on empty catalog
	fresh, err := cat.Fact(state.FactContext{Database: "db1"}, "backup_freshness", nil)
	if err != nil || fresh.(float64) < 1e8 {
		t.Fatalf("empty catalog freshness = %v err=%v (want huge)", fresh, err)
	}
	if v, _ := cat.Fact(state.FactContext{Database: "db1"}, "verified_backup_exists", nil); v != false {
		t.Fatal("empty catalog claims verified backup")
	}
	if err := cat.Register("db1", "/x.fbk", true); err != nil {
		t.Fatal(err)
	}
	if v, _ := cat.Fact(state.FactContext{Database: "db1"}, "verified_backup_exists", nil); v != true {
		t.Fatal("registered backup not visible")
	}
	if _, err := cat.Fact(state.FactContext{Database: "db1"}, "nope", nil); err == nil {
		t.Fatal("unknown fact accepted")
	}
}
