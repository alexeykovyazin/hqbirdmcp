package dbpool

import (
	"context"
	"os"
	"testing"

	"github.com/aleks/fbmcp/internal/config"
)

// Safety fuse #1 (main plan §8): a write attempted on the read pool must be
// refused by the ENGINE. Runs against the local spike DB when available;
// skips (with a loud message) otherwise — CI provides the DB.
//
// Env: FBMCP_FUSE_DB (default: the P0 spike FB5 database + masterkey creds).
func TestFuse1ReadPoolRefusesWrites(t *testing.T) {
	dbFile := os.Getenv("FBMCP_FUSE_DB")
	if dbFile == "" {
		dbFile = `C:/HQbirdData/output/fbmcp-spike/spike_FB5.0.fdb`
	}
	if _, err := os.Stat(dbFile); err != nil {
		t.Skipf("spike DB not present (%v) — fuse test needs a Firebird instance", err)
	}

	cfg := &config.Config{
		State: config.State{Dir: t.TempDir()},
		Instances: []config.FBInstance{{ID: "fb5", Addr: "localhost:3055", BinDir: "C:/HQbird/Firebird50"}},
		Databases: []config.Database{{
			ID: "fuse", Instance: "fb5", Path: dbFile,
			ROUser: "SYSDBA", ROSecretEnv: "FBMCP_FUSE_PW",
			AdminUser: "SYSDBA", AdminSecretEnv: "FBMCP_FUSE_PW",
		}},
	}
	os.Setenv("FBMCP_FUSE_PW", "masterkey")
	defer os.Unsetenv("FBMCP_FUSE_PW")

	m := NewManager(cfg)
	defer m.Close()

	ctx := context.Background()
	if err := m.Health(ctx, "fuse"); err != nil {
		t.Skipf("Firebird not reachable: %v", err)
	}

	tx, err := m.ReadOnly(ctx, "fuse")
	if err != nil {
		t.Fatalf("open read-only tx: %v", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec("INSERT INTO RDB$DATABASE (RDB$DESCRIPTION) VALUES ('fuse')"); err == nil {
		t.Fatal("FUSE FAILURE: write succeeded on read-only transaction")
	} else {
		t.Logf("engine refused DML as expected: %v", err)
	}
	if _, err := tx.Exec("CREATE TABLE FUSE_SHOULD_NOT_EXIST (ID INT)"); err == nil {
		t.Fatal("FUSE FAILURE: DDL succeeded on read-only transaction")
	} else {
		t.Logf("engine refused DDL as expected: %v", err)
	}

	// read path still works
	var n int
	if err := tx.QueryRow("SELECT COUNT(*) FROM RDB$RELATIONS").Scan(&n); err != nil {
		t.Fatalf("read on RO tx failed: %v", err)
	}
}
