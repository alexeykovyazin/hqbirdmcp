package config

import (
	"os"
	"path/filepath"
	"testing"
)

const validYAML = `
state:
  dir: /var/lib/fbmcp
instances:
  - id: fb5
    addr: localhost:3055
    bin_dir: C:/HQbird/Firebird50
    version: "5.0"
    service_user: SYSDBA
    service_secret_env: FBMCP_FB5_SERVICE_PW
    default_ro_user: FBMCP_RO
    default_ro_secret_env: FBMCP_SPIKE5_RO_PW
    default_admin_user: SYSDBA
    default_admin_secret_env: FBMCP_SPIKE5_ADMIN_PW
    default_backup_dir: C:/HQbirdData/output/fbmcp-spike
    default_work_dir: C:/HQbirdData/output/fbmcp-spike
databases:
  - id: spike5
    instance: fb5
    path: C:/HQbirdData/output/fbmcp-spike/spike_FB5.0.fdb
    backup_dir: C:/HQbirdData/output/fbmcp-spike
    work_dir: C:/HQbirdData/output/fbmcp-spike
    ro_user: FBMCP_RO
    ro_secret_env: FBMCP_SPIKE5_RO_PW
    admin_user: SYSDBA
    admin_secret_env: FBMCP_SPIKE5_ADMIN_PW
`

func writeCfg(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fbmcp.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	c, err := Load(writeCfg(t, validYAML))
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if c.SourcePath == "" {
		t.Fatal("SourcePath not set")
	}
	if _, err := c.DB("spike5"); err != nil {
		t.Fatalf("known id rejected: %v", err)
	}
	if _, err := c.DB("nope"); err == nil {
		t.Fatal("unknown id accepted")
	}
}

func TestLoadRejects(t *testing.T) {
	cases := map[string]string{
		"no state dir": "instances:\n- id: a\n  addr: x\n  bin_dir: b\ndatabases: []\n",
		"unknown instance": `
state: {dir: /tmp}
instances: []
databases:
  - id: d1
    instance: ghost
    path: /x.fdb
    ro_user: u
    ro_secret_env: E
    admin_user: a
    admin_secret_env: F
`,
		"missing ro creds": `
state: {dir: /tmp}
instances:
  - id: i1
    addr: localhost:3050
    bin_dir: /opt/fb
databases:
  - id: d1
    instance: i1
    path: /x.fdb
    admin_user: a
    admin_secret_env: F
`,
		"dup db id": `
state: {dir: /tmp}
instances:
  - id: i1
    addr: localhost:3050
    bin_dir: /opt/fb
databases:
  - id: d1
    instance: i1
    path: /x.fdb
    ro_user: u
    ro_secret_env: E
    admin_user: a
    admin_secret_env: F
  - id: d1
    instance: i1
    path: /y.fdb
    ro_user: u
    ro_secret_env: E
    admin_user: a
    admin_secret_env: F
`,
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeCfg(t, y)); err == nil {
				t.Fatalf("invalid config accepted: %s", name)
			}
		})
	}
}

func TestLoadAllowsInstanceOnlyBootstrap(t *testing.T) {
	y := `
state: {dir: /tmp}
instances:
  - id: fb5
    addr: localhost:3055
    bin_dir: /opt/fb
    service_user: SYSDBA
    service_secret_env: FBMCP_FB5_SERVICE_PW
`
	if _, err := Load(writeCfg(t, y)); err != nil {
		t.Fatalf("instance-only bootstrap rejected: %v", err)
	}
}

func TestInstanceDiscoveryAndRegistrationDefaultsValidation(t *testing.T) {
	in := FBInstance{ID: "fb5"}
	if err := in.ValidateDiscoveryDefaults(); err == nil {
		t.Fatal("missing discovery defaults accepted")
	}
	if err := in.ValidateRegistrationDefaults(); err == nil {
		t.Fatal("missing registration defaults accepted")
	}
	in.ServiceUser, in.ServiceSecretEnv = "SYSDBA", "FBMCP_SERVICE_PW"
	if err := in.ValidateDiscoveryDefaults(); err != nil {
		t.Fatalf("discovery defaults rejected: %v", err)
	}
	in.DefaultROUser, in.DefaultROSecretEnv = "ro", "RO_PW"
	in.DefaultAdminUser, in.DefaultAdminSecretEnv = "SYSDBA", "ADM_PW"
	if err := in.ValidateRegistrationDefaults(); err != nil {
		t.Fatalf("registration defaults rejected: %v", err)
	}
}

func TestRejectDotDotAndUNC(t *testing.T) {
	y := `
state: {dir: /tmp/fbmcp}
instances:
  - id: i1
    addr: localhost:3050
    bin_dir: /opt/fb
databases:
  - id: d1
    instance: i1
    path: /opt/fb/../../../etc/passwd
    ro_user: u
    ro_secret_env: E
    admin_user: a
    admin_secret_env: F
`
	if _, err := Load(writeCfg(t, y)); err == nil {
		t.Fatal(".. in path accepted")
	}
	y2 := `
state: {dir: /tmp/fbmcp}
instances:
  - id: i1
    addr: localhost:3050
    bin_dir: /opt/fb
databases:
  - id: d1
    instance: i1
    path: "//evil/share/db.fdb"
    ro_user: u
    ro_secret_env: E
    admin_user: a
    admin_secret_env: F
`
	if _, err := Load(writeCfg(t, y2)); err == nil {
		t.Fatal("UNC path accepted")
	}
}
