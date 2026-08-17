package queryplan

import (
	"context"
	"os"
	"testing"

	"github.com/aleks/fbmcp/internal/config"
)

func TestExplainLive(t *testing.T) {
	dbFile := os.Getenv("FBMCP_FUSE_DB")
	if dbFile == "" {
		dbFile = `C:/HQbirdData/output/fbmcp-spike/spike_FB5.0.fdb`
	}
	if _, err := os.Stat(dbFile); err != nil {
		t.Skip("spike DB not present")
	}
	os.Setenv("FBMCP_FUSE_PW", "masterkey")
	defer os.Unsetenv("FBMCP_FUSE_PW")
	inst := config.FBInstance{ID: "fb5", Addr: "localhost:3055", BinDir: `C:\HQbird\Firebird50`}
	db := config.Database{ID: "spike5", Path: dbFile, AdminUser: "SYSDBA", AdminSecretEnv: "FBMCP_FUSE_PW"}

	plan, err := Explain(context.Background(), inst, db, "masterkey", "SELECT * FROM RDB$RELATIONS WHERE RDB$RELATION_NAME = 'CUSTOMER'", false)
	if err != nil {
		t.Skipf("isql route unavailable: %v", err)
	}
	t.Logf("plan output:\n%s", plan)
	if plan == "" {
		t.Fatal("empty plan output")
	}
}
