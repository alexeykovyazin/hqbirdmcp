package hqbirdsetup

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/config"
)

func TestInstanceID(t *testing.T) {
	cases := []struct {
		line string
		port int
		want string
	}{
		{"fb50", 3050, "fb50"},
		{"fb50-2", 3051, "fb50_2"},
		{"p3050", 3050, "p3050"},
		{"FB40", 3040, "fb40"},
		{"", 3055, "p3055"},
		{"5x", 3056, "p3056"},
	}
	for _, c := range cases {
		if got := InstanceID(Line{Line: c.line, FirebirdPort: c.port}); got != c.want {
			t.Errorf("InstanceID(%q) = %q, want %q", c.line, got, c.want)
		}
	}
}

func TestLineName(t *testing.T) {
	for dir, want := range map[string]string{
		filepath.Join("opt", "hqbird", "fbagent-fb50"):   "fb50",
		filepath.Join("opt", "hqbird", "fbagent-fb50-2"): "fb50-2",
		filepath.Join("opt", "hqbird-fbagent"):           "",
	} {
		if got := LineName(dir); got != want {
			t.Errorf("LineName(%q) = %q, want %q", dir, got, want)
		}
	}
}

func TestLoopbackURL(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"http://127.0.0.1:13050", "http://127.0.0.1:13050", true},
		{"http://localhost:13050/", "http://localhost:13050", true},
		{"http://0.0.0.0:13050", "http://127.0.0.1:13050", true},
		{"http://:13050", "http://127.0.0.1:13050", true},
		{"http://[::]:13050", "http://127.0.0.1:13050", true},
		{"http://10.0.0.5:13050", "http://10.0.0.5:13050", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := LoopbackURL(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("LoopbackURL(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// agentDir writes an HQbird 2027 agent folder for a line and returns the
// path of its agent_config.json.
func agentDir(t *testing.T, root, line string, port int, token string) string {
	t.Helper()
	fb := filepath.Join(root, "firebird", line)
	if err := os.MkdirAll(fb, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "hqbird", "fbagent-"+line)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"agent":{"id":"srv-` + line + `"},"firebird":{"port":` + itoa(port) + `,"install_path":` + quote(fb) + `},` +
		`"local_api":{"enabled":true,"listen":"0.0.0.0:` + itoa(10000+port) + `","token":` + quote(token) + `}}`
	p := filepath.Join(dir, "agent_config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := `{"root":` + quote(fb) + `,"service":"FirebirdServer` + line + `","port":` + itoa(port) + `,"version":"5.0.3"}`
	if err := os.WriteFile(filepath.Join(dir, "hqbird2027-firebird.json"), []byte(rec), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func itoa(n int) string     { return strconv.Itoa(n) }
func quote(s string) string { return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"` }

func filled(t *testing.T, cfgPath string) Line {
	t.Helper()
	l := Line{AgentConfig: cfgPath}
	for _, n := range l.FillFromFiles(nil) {
		t.Logf("note: %s", n)
	}
	return l
}

func TestFillFromFiles(t *testing.T) {
	root := t.TempDir()
	l := filled(t, agentDir(t, root, "fb50", 3050, "tok50"))
	if l.Line != "fb50" || l.FirebirdPort != 3050 || l.FirebirdService != "FirebirdServerfb50" || l.FirebirdVersion != "5.0.3" {
		t.Fatalf("line facts: %+v", l)
	}
	if l.LocalAPIURL != "http://0.0.0.0:13050" || l.LocalAPIToken != "tok50" || l.InstanceID != "srv-fb50" {
		t.Fatalf("local api facts: %+v", l)
	}
	if l.FirebirdBin != filepath.Join(root, "firebird", "fb50") {
		t.Fatalf("bin dir %q", l.FirebirdBin)
	}

	sealed := agentDir(t, root, "fb40", 3040, "enc:v1:abc")
	l2 := Line{AgentConfig: sealed}
	notes := l2.FillFromFiles(nil)
	if l2.LocalAPIToken != "" || len(notes) != 1 || !strings.Contains(notes[0], "sealed") {
		t.Fatalf("sealed token: token %q notes %v", l2.LocalAPIToken, notes)
	}
}

func testPaths(root string) Paths {
	return DefaultPaths("windows", filepath.Join(root, "data"))
}

func noSecret(string) bool { return false }

var testNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func ids(insts []config.FBInstance) string {
	var out []string
	for _, in := range insts {
		out = append(out, in.ID)
	}
	return strings.Join(out, ",")
}

func TestReconcileGrowAndShrink(t *testing.T) {
	root := t.TempDir()
	p := testPaths(root)
	l50 := filled(t, agentDir(t, root, "fb50", 3050, "t50"))
	l50.SysdbaPassword = "masterkey"
	l40 := filled(t, agentDir(t, root, "fb40", 3040, "t40"))
	l50b := filled(t, agentDir(t, root, "fb50-2", 3051, "t51"))

	plan := Reconcile(nil, Managed{}, []Line{l50}, p, noSecret)
	if ids(plan.Config.Instances) != "fb50" || len(plan.Config.Agents) != 1 {
		t.Fatalf("1 line: %s, %d agents", ids(plan.Config.Instances), len(plan.Config.Agents))
	}
	in := plan.Config.Instances[0]
	if in.Addr != "localhost:3050" || in.ServiceSecretEnv != "FBMCP_FB_P3050_SYSDBA" || in.Service != "FirebirdServerfb50" {
		t.Fatalf("instance: %+v", in)
	}
	ag := plan.Config.Agents[0]
	if ag.ID != "agent_p3050" || ag.BaseURL != "http://127.0.0.1:13050" || ag.TokenEnv != "FBMCP_AGENT_TOKEN_P3050" {
		t.Fatalf("agent: %+v", ag)
	}
	if plan.Secrets["FBMCP_AGENT_TOKEN_P3050"] != "t50" || plan.Secrets["FBMCP_FB_P3050_SYSDBA"] != "masterkey" {
		t.Fatalf("secrets: %v", plan.Secrets)
	}
	if err := plan.Config.Validate(); err != nil {
		t.Fatal(err)
	}

	// A second and a third line join; the first entry stays as it is.
	have := map[string]bool{"FBMCP_AGENT_TOKEN_P3050": true, "FBMCP_FB_P3050_SYSDBA": true}
	haveSecret := func(n string) bool { return have[n] }
	l50.SysdbaPassword = "" // a later run: the password is only in the secret file
	plan2 := Reconcile(plan.Config, plan.Managed, []Line{l50, l40, l50b}, p, haveSecret)
	if ids(plan2.Config.Instances) != "fb50,fb40,fb50_2" {
		t.Fatalf("3 lines: %s", ids(plan2.Config.Instances))
	}
	if plan2.Config.Instances[0] != in {
		t.Fatalf("fb50 changed: %+v", plan2.Config.Instances[0])
	}
	for _, c := range plan2.Changes {
		if strings.Contains(c, "fb50 ") || strings.HasSuffix(c, "fb50") {
			t.Fatalf("unexpected change %q", c)
		}
	}

	// The same lines again: nothing changes.
	for n := range plan2.Secrets {
		have[n] = true
	}
	plan3 := Reconcile(plan2.Config, plan2.Managed, []Line{l50, l40, l50b}, p, haveSecret)
	if len(plan3.Changes) != 0 {
		t.Fatalf("second run changes: %v", plan3.Changes)
	}

	// fb40 is removed; its entries and secrets go.
	plan4 := Reconcile(plan3.Config, plan3.Managed, []Line{l50, l50b}, p, haveSecret)
	if ids(plan4.Config.Instances) != "fb50,fb50_2" || len(plan4.Config.Agents) != 2 {
		t.Fatalf("after removal: %s, %d agents", ids(plan4.Config.Instances), len(plan4.Config.Agents))
	}
	stale := StaleSecrets(plan3.Managed, plan4.Managed)
	if strings.Join(stale, ",") != "FBMCP_AGENT_TOKEN_P3040" {
		t.Fatalf("stale secrets: %v", stale)
	}
}

func TestReconcileKeepsOperatorEntries(t *testing.T) {
	root := t.TempDir()
	p := testPaths(root)
	old := &config.Config{
		State:     config.State{Dir: filepath.Join(root, "st")},
		Instances: []config.FBInstance{{ID: "main", Addr: "localhost:3050", BinDir: `C:\fb`, DefaultBackupDir: `D:\bk`}},
		Agents:    []config.Agent{{ID: "mine", BaseURL: "http://127.0.0.1:13040", TokenEnv: "MY_TOKEN"}},
	}
	l50 := filled(t, agentDir(t, root, "fb50", 3050, "t50"))
	l40 := filled(t, agentDir(t, root, "fb40", 3040, "t40"))
	plan := Reconcile(old, Managed{}, []Line{l50, l40}, p, noSecret)
	if ids(plan.Config.Instances) != "main,fb40" {
		t.Fatalf("instances: %s", ids(plan.Config.Instances))
	}
	if plan.Config.Instances[0] != old.Instances[0] {
		t.Fatalf("operator instance changed: %+v", plan.Config.Instances[0])
	}
	if len(plan.Config.Agents) != 1 || plan.Config.Agents[0].ID != "mine" {
		t.Fatalf("agents: %+v", plan.Config.Agents)
	}
	if strings.Join(plan.Managed.Instances, ",") != "fb40" || len(plan.Managed.Agents) != 0 {
		t.Fatalf("managed: %+v", plan.Managed)
	}
	if plan.Config.State.Dir != old.State.Dir {
		t.Fatalf("state.dir changed to %s", plan.Config.State.Dir)
	}

	// The last managed line goes: the operator entries stay.
	plan2 := Reconcile(plan.Config, plan.Managed, nil, p, noSecret)
	if ids(plan2.Config.Instances) != "main" || len(plan2.Config.Agents) != 1 {
		t.Fatalf("after removal: %s", ids(plan2.Config.Instances))
	}
}

func TestReconcileKeepsUsedInstanceAndSkipsBadLines(t *testing.T) {
	root := t.TempDir()
	p := testPaths(root)
	l50 := filled(t, agentDir(t, root, "fb50", 3050, "t50"))
	plan := Reconcile(nil, Managed{}, []Line{l50}, p, noSecret)
	plan.Config.Databases = []config.Database{{ID: "erp", Instance: "fb50", Path: `D:\erp.fdb`}}

	dupPort := l50
	dupPort.AgentConfig = filepath.Join(root, "other", "agent_config.json")
	noPort := Line{AgentConfig: "x", Line: "fb30"}
	plan2 := Reconcile(plan.Config, plan.Managed, []Line{noPort}, p, noSecret)
	if ids(plan2.Config.Instances) != "fb50" {
		t.Fatalf("used instance dropped: %s", ids(plan2.Config.Instances))
	}
	if len(plan2.Notes) < 2 {
		t.Fatalf("notes: %v", plan2.Notes)
	}
	plan3 := Reconcile(nil, Managed{}, []Line{l50, dupPort}, p, noSecret)
	if len(plan3.Config.Instances) != 1 {
		t.Fatalf("duplicate port: %s", ids(plan3.Config.Instances))
	}
}

func TestApplyRemote(t *testing.T) {
	p := DefaultPaths("linux", "")
	cfg := &config.Config{Listen: ""}
	if st := ApplyRemote(cfg, Remote{}, "10.0.0.5", p); st.Listen != "" || cfg.TLS.Cert != "" {
		t.Fatalf("remote off changed the config: %+v %+v", st, cfg.TLS)
	}

	r := Remote{AllowFrom: []string{"10.0.0.0/24"}}
	st := ApplyRemote(cfg, r, "10.0.0.5", p)
	if !st.Pending || st.Listen != "" || cfg.Listen != "" {
		t.Fatalf("no identity: %+v listen %q", st, cfg.Listen)
	}
	if cfg.TLS.Cert != "/etc/fbmcp/tls/tls.crt" || len(cfg.AllowedOrigins) != 1 {
		t.Fatalf("tls/origins: %+v %v", cfg.TLS, cfg.AllowedOrigins)
	}

	cfg.Identities = []config.APIIdentity{{Name: "alice", KeyEnv: "FBMCP_KEY_ALICE"}}
	st = ApplyRemote(cfg, r, "10.0.0.5", p)
	if st.Pending || st.Listen != "10.0.0.5:8650" || cfg.Listen != "10.0.0.5:8650" {
		t.Fatalf("with identity: %+v", st)
	}
	if u := URL(st.Listen, "srv1.example.com"); u != "https://srv1.example.com:8650/mcp" {
		t.Fatalf("URL %s", u)
	}
	// A port the operator set stays when --port is not given.
	cfg.Listen = "10.0.0.5:9650"
	if st := ApplyRemote(cfg, r, "10.0.0.5", p); st.Listen != "10.0.0.5:9650" {
		t.Fatalf("kept port: %+v", st)
	}
}

func TestAddAllowFrom(t *testing.T) {
	var r Remote
	if err := r.AddAllowFrom([]string{"10.0.0.0/24, 192.168.1.7", "10.0.0.0/24"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.AllowFrom, ",") != "10.0.0.0/24,192.168.1.7" {
		t.Fatalf("allow: %v", r.AllowFrom)
	}
	if err := r.AddAllowFrom([]string{"not-an-ip"}); err == nil {
		t.Fatal("bad address accepted")
	}
}

func TestRenderConnect(t *testing.T) {
	on := RenderConnect(ConnectInfo{Server: "srv1", URL: "https://srv1:8650/mcp", Pin: "sha256:abc", AllowFrom: []string{"10.0.0.0/24"}, StateDir: "/var/lib/fbmcp"})
	for _, want := range []string{"https://srv1:8650/mcp", "sha256:abc", "FBMCP_KEY_SRV1", `C:\\fbmcp\\fbmcp.exe`, "approve /var/lib/fbmcp"} {
		if !strings.Contains(on, want) {
			t.Errorf("connect.txt has no %q:\n%s", want, on)
		}
	}
	if !strings.Contains(RenderConnect(ConnectInfo{Server: "srv1", Pending: true, Config: "/etc/fbmcp/fbmcp.yaml"}), `apikey new <name> --max-tier 2 --config "/etc/fbmcp/fbmcp.yaml"`) {
		t.Error("pending text has no apikey step")
	}
	if !strings.Contains(RenderConnect(ConnectInfo{Server: "srv1"}), "--allow-from") {
		t.Error("off text has no --allow-from step")
	}
}

func TestManagedRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "st", "hqbird-managed.json")
	m, err := ReadManaged(p)
	if err != nil || len(m.Instances) != 0 {
		t.Fatalf("missing file: %+v %v", m, err)
	}
	m = Managed{Instances: []string{"fb50"}, Remote: Remote{AllowFrom: []string{"10.0.0.0/24"}}}
	if err := WriteManaged(p, m, testNow); err != nil {
		t.Fatal(err)
	}
	got, err := ReadManaged(p)
	if err != nil || got.Instances[0] != "fb50" || !got.Remote.On() || got.UpdatedAt == "" {
		t.Fatalf("read back: %+v %v", got, err)
	}
}
