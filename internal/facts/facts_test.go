package facts

import (
	"context"
	"os"
	"testing"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/dbpool"
	"github.com/aleks/fbmcp/internal/state"
)

func TestEngineFactsSnapshot(t *testing.T) {
	dbFile := os.Getenv("FBMCP_FUSE_DB")
	if dbFile == "" {
		dbFile = `C:/HQbirdData/output/fbmcp-spike/spike_FB5.0.fdb`
	}
	if _, err := os.Stat(dbFile); err != nil {
		t.Skip("spike DB not present")
	}
	os.Setenv("FBMCP_FUSE_PW", "masterkey")
	defer os.Unsetenv("FBMCP_FUSE_PW")

	cfg := &config.Config{
		State:     config.State{Dir: t.TempDir()},
		Instances: []config.FBInstance{{ID: "fb5", Addr: "localhost:3055", BinDir: "C:/HQbird/Firebird50"}},
		Databases: []config.Database{{ID: "spike5", Instance: "fb5", Path: dbFile,
			ROUser: "SYSDBA", ROSecretEnv: "FBMCP_FUSE_PW", AdminUser: "SYSDBA", AdminSecretEnv: "FBMCP_FUSE_PW"}},
	}
	pools := dbpool.NewManager(cfg)
	defer pools.Close()
	if err := pools.Health(context.Background(), "spike5"); err != nil {
		t.Skip("Firebird not reachable")
	}

	ef := NewEngineFacts(cfg, pools)
	snap, err := ef.Snapshot(context.Background(), "spike5")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"engine_version", "ods", "page_size", "sql_dialect", "read_only", "forced_writes"} {
		if _, ok := snap[k]; !ok {
			t.Errorf("missing fact %s (have %v)", k, keysOf(snap))
		}
	}
	if v, _ := snap["engine_version"].(string); v != "5.0" {
		t.Errorf("engine_version = %q, want 5.0", v)
	}
	// facts-provider interface path
	if _, err := ef.Fact(factCtx("spike5"), "engine_version", nil); err != nil {
		t.Errorf("Fact(engine_version): %v", err)
	}
	if _, err := ef.Fact(factCtx("spike5"), "nope", nil); err == nil {
		t.Error("unknown fact accepted (must fail closed)")
	}
}

func factCtx(db string) state.FactContext { return state.FactContext{Database: db} }

func keysOf(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
