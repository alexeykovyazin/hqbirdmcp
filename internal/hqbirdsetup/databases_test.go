package hqbirdsetup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aleks/fbmcp/internal/config"
)

func TestDatabasesConfPaths(t *testing.T) {
	src := "# comment\r\n" +
		"employee.fdb = $(dir_sampleDb)/employee.fdb\n" +
		"employee = $(dir_sampleDb)/employee.fdb\n" +
		"security.db = /opt/firebird/security5.fdb\n" +
		"{\n\tRemoteAccess = false\n}\n" +
		"sales = /data/sales.fdb   # main\n" +
		"stock = /data/stock.fdb\n" +
		"{\n  DefaultDbCachePages = 64K\n  # x = /no.fdb\n}\n" +
		"erp = C:\\db\\erp.fdb {\n  LockMemSize = 1M\n}\n" +
		"remote = srv2:/data/r.fdb\n" +
		"unc = \\\\srv2\\share\\u.fdb\n" +
		"quoted = \"/data/q.fdb\"\n"
	got := strings.Join(DatabasesConfPaths(src), ",")
	if want := `/data/sales.fdb,/data/stock.fdb,C:\db\erp.fdb,/data/q.fdb`; got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestRecentDatabases(t *testing.T) {
	data := t.TempDir()
	dir := filepath.Join(data, "server_level", "GetActiveDatabases")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := `{"databases":[{"path":"/data/old.fdb"}]}` + "\n"
	cur := `{"databases":[{"path":"/data/b.fdb","attachments":2},{"path":"/opt/fb/security5.fdb"}]}` + "\n" +
		"not json\n" +
		`{"databases":[{"path":"/data/a.fdb"},{"path":"/data/b.fdb"}]}` + "\n"
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	o := write("getactdb_20261104.jsonl", old)
	write("getactdb_20261105.jsonl", cur)
	past := testNow.Add(-24 * 3600 * 1e9)
	_ = os.Chtimes(o, past, past)
	if got := strings.Join(RecentDatabases(data), ","); got != "/data/a.fdb,/data/b.fdb" {
		t.Fatalf("recent: %s", got)
	}
	if RecentDatabases(t.TempDir()) != nil {
		t.Fatal("no files: want nil")
	}
}

func TestFillFromFilesDatabases(t *testing.T) {
	root := t.TempDir()
	cfgPath := agentDir(t, root, "fb50", 3050, "t50")
	b, _ := os.ReadFile(cfgPath)
	body := `{"storage":{"data_path":"logs"},` + strings.TrimPrefix(string(b), "{")
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	dbDir := filepath.Join(root, "db")
	_ = os.MkdirAll(dbDir, 0o755)
	sales, crm, gone := filepath.Join(dbDir, "sales.fdb"), filepath.Join(dbDir, "crm.fdb"), filepath.Join(dbDir, "gone.fdb")
	for _, p := range []string{sales, crm} {
		_ = os.WriteFile(p, []byte("x"), 0o644)
	}
	fb := filepath.Join(root, "firebird", "fb50")
	conf := "sales = " + sales + "\ngone = " + gone + "\n"
	_ = os.WriteFile(filepath.Join(fb, "databases.conf"), []byte(conf), 0o644)
	act := filepath.Join(filepath.Dir(cfgPath), "logs", "server_level", "GetActiveDatabases")
	_ = os.MkdirAll(act, 0o755)
	line := `{"databases":[{"path":` + quote(crm) + `},{"path":` + quote(sales) + `}]}` + "\n"
	_ = os.WriteFile(filepath.Join(act, "getactdb_20261105.jsonl"), []byte(line), 0o644)

	l := filled(t, cfgPath)
	if got, want := strings.Join(l.Databases, ","), sales+","+crm; got != want {
		t.Fatalf("databases: %s, want %s", got, want)
	}
}

func TestReconcileDatabases(t *testing.T) {
	root := t.TempDir()
	p := testPaths(root)
	l50 := filled(t, agentDir(t, root, "fb50", 3050, "t50"))
	l50.SysdbaPassword = "masterkey"
	l50.Databases = []string{`C:\db\sales.fdb`, `C:\db\Main.FDB`, `c:\DB\op.fdb`}
	l40 := filled(t, agentDir(t, root, "fb40", 3040, "t40"))
	l40.SysdbaPassword = "masterkey"
	l40.Databases = []string{`D:\db40\sales.fdb`}
	l30 := filled(t, agentDir(t, root, "fb30", 3030, "t30"))
	l30.Databases = []string{`E:\x.fdb`} // no SYSDBA password

	// The operator registered one file already, on an instance of their own.
	old := &config.Config{
		Instances: []config.FBInstance{{ID: "op", Addr: "localhost:3999", BinDir: `C:\op`}},
		Databases: []config.Database{{ID: "op", Instance: "op", Path: `C:\db\op.fdb`, ROUser: "R", ROSecretEnv: "R", AdminUser: "A", AdminSecretEnv: "A"}},
	}
	plan := Reconcile(old, Managed{}, []Line{l50, l40, l30}, p, noSecret)
	if err := plan.Config.Validate(); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range plan.Config.Databases {
		got = append(got, d.ID+"@"+d.Instance)
	}
	if strings.Join(got, ",") != "op@op,sales@fb40,sales_fb50@fb50,main@fb50" {
		t.Fatalf("databases: %v", got)
	}
	in, _ := plan.Config.Instance("fb50")
	if in.DefaultROUser != "SYSDBA" || in.DefaultROSecretEnv != "FBMCP_FB_P3050_SYSDBA" ||
		in.DefaultBackupDir != p.BackupRoot+`\fb50` || in.DefaultWorkDir != p.WorkRoot+`\fb50` {
		t.Fatalf("instance defaults: %+v", in)
	}
	db, _ := plan.Config.DB("main")
	if db.ROUser != "SYSDBA" || db.AdminSecretEnv != "FBMCP_FB_P3050_SYSDBA" || db.BackupDir != in.DefaultBackupDir {
		t.Fatalf("db: %+v", db)
	}
	if strings.Join(plan.Managed.Databases, ",") != "sales,sales_fb50,main" {
		t.Fatalf("managed: %v", plan.Managed.Databases)
	}
	if !strings.Contains(strings.Join(plan.Notes, "|"), "line fb30: 1 database(s) not registered: no SYSDBA password") {
		t.Fatalf("notes: %v", plan.Notes)
	}

	// The same lines again: nothing changes.
	have := func(n string) bool { return n != "FBMCP_FB_P3030_SYSDBA" }
	l50.SysdbaPassword, l40.SysdbaPassword = "", ""
	plan2 := Reconcile(plan.Config, plan.Managed, []Line{l50, l40, l30}, p, have)
	if len(plan2.Changes) != 0 {
		t.Fatalf("second run: %v", plan2.Changes)
	}

	// fb40 goes: its database goes with it, and so does the instance.
	plan3 := Reconcile(plan2.Config, plan2.Managed, []Line{l50, l30}, p, have)
	if _, err := plan3.Config.DB("sales"); err == nil {
		t.Fatal("sales of fb40 stays")
	}
	if _, err := plan3.Config.Instance("fb40"); err == nil {
		t.Fatal("instance fb40 stays")
	}
	if _, err := plan3.Config.DB("op"); err != nil {
		t.Fatal("the operator's database went")
	}
}

func TestUniqueDBID(t *testing.T) {
	taken := map[string]bool{"sales": true, "sales_fb50": true}
	if got := uniqueDBID("sales", "fb50", taken); got != "sales_fb50_2" {
		t.Fatalf("got %s", got)
	}
	long := strings.Repeat("a", 70)
	if got := uniqueDBID(long, "fb50", map[string]bool{}); len(got) != 64 {
		t.Fatalf("len %d", len(got))
	}
}
