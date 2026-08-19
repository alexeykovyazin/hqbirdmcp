package facts

import (
	"context"
	"errors"
	fb "github.com/nakagami/firebirdsql"
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

type stubSvc struct {
	version string
	info    *fb.SrvDbInfo
	err     error
}

func (s *stubSvc) Version() (string, error)                { return s.version, nil }
func (s *stubSvc) ConnectedDBInfo() (*fb.SrvDbInfo, error) { return s.info, s.err }
func (s *stubSvc) Close() error                            { return nil }

func TestConnectedDatabasesUsesDBInstanceAndMapsPaths(t *testing.T) {
	t.Setenv("FBMCP_SERVICE_PW", "masterkey")
	cfg := &config.Config{
		State: config.State{Dir: t.TempDir()},
		Instances: []config.FBInstance{
			{ID: "fb5", Addr: "localhost:3055", BinDir: "C:/HQbird/Firebird50", ServiceUser: "SYSDBA", ServiceSecretEnv: "FBMCP_SERVICE_PW"},
			{ID: "fb4", Addr: "localhost:3054", BinDir: "C:/HQbird/Firebird40", ServiceUser: "SYSDBA", ServiceSecretEnv: "FBMCP_SERVICE_PW"},
		},
		Databases: []config.Database{
			{ID: "spike5", Instance: "fb5", Path: `D:/Database/WMS_CIELO/DADOS.FDB`, ROUser: "SYSDBA", ROSecretEnv: "FBMCP_FUSE_PW", AdminUser: "SYSDBA", AdminSecretEnv: "FBMCP_FUSE_PW"},
			{ID: "sameinst_dup_a", Instance: "fb5", Path: `D:/Database/WMS_CIELO/OPLGAR.FDB`, ROUser: "SYSDBA", ROSecretEnv: "FBMCP_FUSE_PW", AdminUser: "SYSDBA", AdminSecretEnv: "FBMCP_FUSE_PW"},
			{ID: "sameinst_dup_b", Instance: "fb5", Path: `d:\database\wms_cielo\oplgar.fdb`, ROUser: "SYSDBA", ROSecretEnv: "FBMCP_FUSE_PW", AdminUser: "SYSDBA", AdminSecretEnv: "FBMCP_FUSE_PW"},
			{ID: "otherinst_samepath", Instance: "fb4", Path: `D:/Database/WMS_CIELO/SINCRONIZA.FDB`, ROUser: "SYSDBA", ROSecretEnv: "FBMCP_FUSE_PW", AdminUser: "SYSDBA", AdminSecretEnv: "FBMCP_FUSE_PW"},
			{ID: "spike5b", Instance: "fb5", Path: `D:/Database/WMS_CIELO/SINCRONIZA.FDB`, ROUser: "SYSDBA", ROSecretEnv: "FBMCP_FUSE_PW", AdminUser: "SYSDBA", AdminSecretEnv: "FBMCP_FUSE_PW"},
		},
	}
	old := openSvc
	defer func() { openSvc = old }()
	var gotAddr, gotUser, gotPass string
	openSvc = func(addr, user, pass string) (serviceClient, error) {
		gotAddr, gotUser, gotPass = addr, user, pass
		return &stubSvc{info: &fb.SrvDbInfo{
			AttachmentsCount: 182,
			DatabaseCount:    4,
			Databases: []string{
				`D:\DATABASE\WMS_CIELO\DADOS.FDB`,
				`D:\DATABASE\WMS_CIELO\OPLGAR.FDB`,
				`D:\DATABASE\WMS_CIELO\SINCRONIZA.FDB`,
				`D:\DATABASE\WMS_CIELO\SINCRONIZADOR2\SINCRONIZA2.FDB`,
			},
		}}, nil
	}

	info, err := ConnectedDatabases(cfg, "fb5")
	if err != nil {
		t.Fatal(err)
	}
	if gotAddr != "localhost:3055" || gotUser != "SYSDBA" || gotPass != "masterkey" {
		t.Fatalf("svc creds/addr = %q %q %q", gotAddr, gotUser, gotPass)
	}
	if info.Instance != "fb5" || info.AttachmentsCount != 182 || info.DatabaseCount != 4 {
		t.Fatalf("unexpected info: %+v", info)
	}
	if len(info.Databases) != 4 {
		t.Fatalf("databases = %d", len(info.Databases))
	}
	if info.Databases[0].DBID != "spike5" || info.Databases[0].MatchStatus != "managed" {
		t.Fatalf("row0 = %+v", info.Databases[0])
	}
	if info.Databases[1].DBID != "" || info.Databases[1].MatchStatus != "ambiguous" {
		t.Fatalf("row1 = %+v", info.Databases[1])
	}
	if info.Databases[2].DBID != "spike5b" || info.Databases[2].MatchStatus != "managed" {
		t.Fatalf("row2 = %+v", info.Databases[2])
	}
	if info.Databases[3].DBID != "" || info.Databases[3].MatchStatus != "unmanaged" {
		t.Fatalf("row3 = %+v", info.Databases[3])
	}
}

func TestConnectedDatabasesPropagatesSvcError(t *testing.T) {
	t.Setenv("FBMCP_SERVICE_PW", "masterkey")
	cfg := testFactsConfig(t)
	old := openSvc
	defer func() { openSvc = old }()
	openSvc = func(addr, user, pass string) (serviceClient, error) {
		return &stubSvc{err: errors.New("svc failed")}, nil
	}
	_, err := ConnectedDatabases(cfg, "fb5")
	if err == nil || err.Error() != "svc failed" {
		t.Fatalf("err = %v", err)
	}
}

func TestConnectedDatabasesEmptyResult(t *testing.T) {
	t.Setenv("FBMCP_SERVICE_PW", "masterkey")
	cfg := testFactsConfig(t)
	old := openSvc
	defer func() { openSvc = old }()
	openSvc = func(addr, user, pass string) (serviceClient, error) {
		return &stubSvc{info: &fb.SrvDbInfo{}}, nil
	}
	info, err := ConnectedDatabases(cfg, "fb5")
	if err != nil {
		t.Fatal(err)
	}
	if info.Instance != "fb5" || info.AttachmentsCount != 0 || info.DatabaseCount != 0 || len(info.Databases) != 0 {
		t.Fatalf("unexpected empty info: %+v", info)
	}
}

func testFactsConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		State:     config.State{Dir: t.TempDir()},
		Instances: []config.FBInstance{{ID: "fb5", Addr: "localhost:3055", BinDir: "C:/HQbird/Firebird50", ServiceUser: "SYSDBA", ServiceSecretEnv: "FBMCP_SERVICE_PW"}},
		Databases: []config.Database{{ID: "spike5", Instance: "fb5", Path: `D:/Database/WMS_CIELO/DADOS.FDB`,
			ROUser: "SYSDBA", ROSecretEnv: "FBMCP_FUSE_PW", AdminUser: "SYSDBA", AdminSecretEnv: "FBMCP_FUSE_PW"}},
	}
}
