package config

import (
	"os"
	"strings"
	"testing"
)

func TestMaterializeDatabaseUsesInstanceDefaults(t *testing.T) {
	cfg, err := Load(writeCfg(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	db, err := MaterializeDatabase(cfg, RegisterOptions{
		InstanceID: "fb5",
		DBID:       "dados",
		Path:       `D:\DATABASE\WMS\DADOS.FDB`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if db.ROUser != "FBMCP_RO" || db.AdminUser != "SYSDBA" {
		t.Fatalf("defaults not materialized: %+v", db)
	}
	if db.BackupDir == "" || db.WorkDir == "" {
		t.Fatalf("dirs missing: %+v", db)
	}
}

func TestMaterializeDatabaseFailsWithoutDefaults(t *testing.T) {
	y := `
state: {dir: /tmp}
instances:
  - id: fb5
    addr: localhost:3055
    bin_dir: /opt/fb
    service_user: SYSDBA
    service_secret_env: X
`
	cfg, err := Load(writeCfg(t, y))
	if err != nil {
		t.Fatal(err)
	}
	_, err = MaterializeDatabase(cfg, RegisterOptions{
		InstanceID: "fb5",
		DBID:       "dados",
		Path:       `/data/dados.fdb`,
	})
	if err == nil {
		t.Fatal("missing defaults accepted")
	}
}

func TestRegisterDatabaseAppendsAndRejectsDuplicates(t *testing.T) {
	p := writeCfg(t, validYAML)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	db, err := MaterializeDatabase(cfg, RegisterOptions{
		InstanceID: "fb5",
		DBID:       "dados",
		Path:       `D:\DATABASE\WMS\DADOS.FDB`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterDatabase(p, db); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "id: dados") {
		t.Fatalf("new db not written:\n%s", b)
	}
	if _, err := os.Stat(p + ".prev"); err != nil {
		t.Fatalf(".prev missing: %v", err)
	}
	if err := RegisterDatabase(p, db); err == nil {
		t.Fatal("duplicate id/path accepted")
	}
}

func TestSuggestedDBID(t *testing.T) {
	if got := SuggestedDBID(`D:\DATABASE\WMS_CIELO\SINCRONIZA2.FDB`); got != "sincroniza2" {
		t.Fatalf("SuggestedDBID = %q", got)
	}
}
