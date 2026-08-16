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
