package main

import (
	"strings"
	"testing"
)

func TestPreviewRegisterDBUsesSuggestedIDAndDefaults(t *testing.T) {
	cfg := testCfg(t)
	cfg.Instances[0].DefaultROUser = "ro"
	cfg.Instances[0].DefaultROSecretEnv = "RO_PW"
	cfg.Instances[0].DefaultAdminUser = "SYSDBA"
	cfg.Instances[0].DefaultAdminSecretEnv = "ADM_PW"
	cfg.Instances[0].DefaultBackupDir = `C:/backup`
	cfg.Instances[0].DefaultWorkDir = `C:/work`
	_, summary, err := previewRegisterDB(cfg, registerDBArg{
		Instance: "fb5",
		Path:     `D:\DATABASE\WMS_CIELO\SINCRONIZA2.FDB`,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"db: sincroniza2",
		"backup_dir: C:/backup",
		"work_dir: C:/work",
		"ro_secret_env: RO_PW",
		"admin_secret_env: ADM_PW",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, summary)
		}
	}
}
