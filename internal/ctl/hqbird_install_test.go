package ctl

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/agentdiscover"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/hqbirdsetup"
	"github.com/aleks/fbmcp/internal/secrets"
)

// fakeHost records what hqbird-install asks of the OS.
type fakeHost struct {
	calls    []string
	state    string
	firewall []string
}

func (f *fakeHost) note(s string) { f.calls = append(f.calls, s) }

func (f *fakeHost) FirebirdService(string) string { return "" }
func (f *fakeHost) Account() string               { return "fbmcp-test" }
func (f *fakeHost) PrepareFolders(p hqbirdsetup.Paths) error {
	f.note("folders")
	for _, d := range []string{filepath.Dir(p.Config), p.StateDir, p.SecretsDir, p.TLSDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}
func (f *fakeHost) AfterWrite(...string) error { return nil }
func (f *fakeHost) EnsureService(exe, cfg string) error {
	f.note("service")
	if f.state == "absent" {
		f.state = "stopped"
	}
	return nil
}
func (f *fakeHost) GrantFirebird(s []string) []string {
	f.note("firebird:" + strings.Join(s, ","))
	return nil
}
func (f *fakeHost) GrantAgentConfigs(p []string) []string { return nil }
func (f *fakeHost) GrantKeyRead(string) error             { f.note("keyread"); return nil }
func (f *fakeHost) Firewall(port int, allow []string, _ string, m *hqbirdsetup.Managed) error {
	f.note("firewall:" + strconv.Itoa(port) + ":" + strings.Join(allow, ","))
	m.Firewall = []string{"fbmcp-remote"}
	f.firewall = m.Firewall
	return nil
}
func (f *fakeHost) RemoveFirewall(m *hqbirdsetup.Managed) error {
	if len(m.Firewall) > 0 {
		f.note("firewall-off")
	}
	m.Firewall, f.firewall = nil, nil
	return nil
}
func (f *fakeHost) State() string { return f.state }
func (f *fakeHost) Start() error  { f.note("start"); f.state = "running"; return nil }
func (f *fakeHost) StopDisable() error {
	f.note("stop")
	f.state = "stopped"
	return nil
}

func (f *fakeHost) did(s string) bool {
	for _, c := range f.calls {
		if c == s || strings.HasPrefix(c, s+":") {
			return true
		}
	}
	return false
}

// hqLine writes an HQbird 2027 agent folder and returns its agent_config.json.
func hqLine(t *testing.T, root, line string, port int) string {
	t.Helper()
	fb := filepath.Join(root, "firebird", line)
	dir := filepath.Join(root, "hqbird", "fbagent-"+line)
	for _, d := range []string{fb, dir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	q := func(s string) string { return strconv.Quote(s) }
	body := `{"firebird":{"port":` + strconv.Itoa(port) + `,"install_path":` + q(fb) + `},` +
		`"local_api":{"enabled":true,"token":"tok` + strconv.Itoa(port) + `"}}`
	p := filepath.Join(dir, "agent_config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := `{"root":` + q(fb) + `,"service":"FB_` + line + `","port":` + strconv.Itoa(port) + `}`
	if err := os.WriteFile(filepath.Join(dir, "hqbird2027-firebird.json"), []byte(rec), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

type hqTest struct {
	t       *testing.T
	root    string
	host    *fakeHost
	cands   []agentdiscover.Candidate
	discErr error
	doctor  int
	stdin   string
}

func newHQTest(t *testing.T) *hqTest {
	return &hqTest{t: t, root: t.TempDir(), host: &fakeHost{state: "absent"}}
}

func (h *hqTest) dataDir() string { return filepath.Join(h.root, "data") }

func (h *hqTest) addLine(line string, port int) {
	p := hqLine(h.t, h.root, line, port)
	h.cands = append(h.cands, agentdiscover.Candidate{Service: "HQbirdFBAgent-" + line, ConfigPath: p})
}

func (h *hqTest) run(args ...string) (hqbirdResult, error) {
	h.t.Helper()
	o, err := parseHqbirdArgs(append([]string{"--data-dir", h.dataDir()}, args...))
	if err != nil {
		h.t.Fatal(err)
	}
	env := hqbirdEnv{
		host: h.host, stdin: strings.NewReader(h.stdin), log: io.Discard, goos: runtime.GOOS, exe: "fbmcp-test",
		discover: func() ([]agentdiscover.Candidate, []string, error) { return h.cands, nil, h.discErr },
		now:      func() time.Time { return time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC) },
		ping:     func(string, time.Duration) error { return nil },
		doctor:   func(string) int { return h.doctor },
		hostIP:   func() string { return "10.0.0.5" },
		hostName: func() string { return "srv1" },
	}
	return runHqbirdInstall(o, env)
}

func (h *hqTest) config() *config.Config {
	h.t.Helper()
	cfg, err := config.Load(filepath.Join(h.dataDir(), "fbmcp.yaml"))
	if err != nil {
		h.t.Fatal(err)
	}
	return cfg
}

func instIDs(cfg *config.Config) string {
	var out []string
	for _, in := range cfg.Instances {
		out = append(out, in.ID)
	}
	return strings.Join(out, ",")
}

func TestHqbirdInstallLines(t *testing.T) {
	h := newHQTest(t)
	h.addLine("fb50", 3050)
	res, err := h.run()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Lines) != 1 || res.ServiceState != "running" || res.Remote.Enabled {
		t.Fatalf("first run: %+v", res)
	}
	if !h.host.did("service") || !h.host.did("firebird:FB_fb50") || !h.host.did("start") || h.host.did("firewall") {
		t.Fatalf("host calls: %v", h.host.calls)
	}
	cfg := h.config()
	if instIDs(cfg) != "fb50" || len(cfg.Agents) != 1 || cfg.Agents[0].BaseURL != "http://127.0.0.1:13050" {
		t.Fatalf("config: %s %+v", instIDs(cfg), cfg.Agents)
	}
	if v, err := secrets.ReadFile(cfg.Secrets.Dir, "FBMCP_AGENT_TOKEN_P3050"); err != nil || v != "tok3050" {
		t.Fatalf("token secret: %q %v", v, err)
	}
	if b, err := os.ReadFile(res.ConnectFile); err != nil || !strings.Contains(string(b), "--allow-from") {
		t.Fatalf("connect.txt: %v", err)
	}

	// Two more lines; the SYSDBA password comes on stdin.
	h.addLine("fb40", 3040)
	h.addLine("fb50-2", 3051)
	h.stdin = `{"lines":[{"agent_config":` + strconv.Quote(h.cands[0].ConfigPath) + `,"sysdba_password":"masterkey"}]}`
	if _, err := h.run("--lines-stdin"); err != nil {
		t.Fatal(err)
	}
	cfg = h.config()
	if instIDs(cfg) != "fb50,fb40,fb50_2" {
		t.Fatalf("3 lines: %s", instIDs(cfg))
	}
	if cfg.Instances[0].ServiceSecretEnv != "FBMCP_FB_P3050_SYSDBA" {
		t.Fatalf("fb50: %+v", cfg.Instances[0])
	}

	// Again with no change: nothing to do.
	h.stdin = ""
	res, err = h.run()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 0 {
		t.Fatalf("idempotent run changed: %v", res.Changes)
	}

	// fb40 goes away.
	h.cands = []agentdiscover.Candidate{h.cands[0], h.cands[2]}
	res, err = h.run()
	if err != nil {
		t.Fatal(err)
	}
	if instIDs(h.config()) != "fb50,fb50_2" {
		t.Fatalf("after removal: %s", instIDs(h.config()))
	}
	if _, err := secrets.ReadFile(cfg.Secrets.Dir, "FBMCP_AGENT_TOKEN_P3040"); err == nil {
		t.Fatal("the token of the removed line is still there")
	}

	// No line left: the service stops, the config stays.
	h.cands = nil
	res, err = h.run()
	if err != nil {
		t.Fatal(err)
	}
	if res.ServiceState != "stopped" || !h.host.did("stop") {
		t.Fatalf("zero lines: %s %v", res.ServiceState, h.host.calls)
	}
	if instIDs(h.config()) != "fb50,fb50_2" {
		t.Fatalf("config after zero lines: %s", instIDs(h.config()))
	}
	// A line comes back: the old entries are replaced.
	h.addLine("fb30", 3030)
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	if instIDs(h.config()) != "fb30" {
		t.Fatalf("new line after zero: %s", instIDs(h.config()))
	}
}

func TestHqbirdInstallDryRun(t *testing.T) {
	h := newHQTest(t)
	h.addLine("fb50", 3050)
	res, err := h.run("--dry-run", "--allow-from", "10.0.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Lines) != 1 || len(h.host.calls) != 0 {
		t.Fatalf("dry run: %+v calls %v", res, h.host.calls)
	}
	if _, err := os.Stat(h.dataDir()); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote %s: %v", h.dataDir(), err)
	}
}

func TestHqbirdInstallDiscoveryError(t *testing.T) {
	h := newHQTest(t)
	h.addLine("fb50", 3050)
	h.discErr = errors.New("access denied")
	if _, err := h.run(); err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("err = %v", err)
	}
	if len(h.host.calls) != 0 {
		t.Fatalf("host calls after a failed discovery: %v", h.host.calls)
	}
}

func TestHqbirdInstallRemote(t *testing.T) {
	h := newHQTest(t)
	h.addLine("fb50", 3050)

	// No API key yet: remote mode waits.
	res, err := h.run("--allow-from", "10.0.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if res.Remote.Enabled || !res.Remote.Pending || h.host.did("firewall") {
		t.Fatalf("pending: %+v %v", res.Remote, h.host.calls)
	}
	if cfg := h.config(); cfg.Listen != "" || cfg.TLS.Cert == "" || len(cfg.AllowedOrigins) == 0 {
		t.Fatalf("pending config: listen %q tls %+v", cfg.Listen, cfg.TLS)
	}

	// The operator adds a key (apikey new); the next run turns remote on.
	cfgPath := filepath.Join(h.dataDir(), "fbmcp.yaml")
	addIdentity(t, cfgPath)
	res, err = h.run()
	if err != nil {
		t.Fatal(err)
	}
	if !res.Remote.Enabled || res.Remote.URL != "https://srv1:8650/mcp" || !strings.HasPrefix(res.Remote.Pin, "sha256:") {
		t.Fatalf("remote: %+v", res.Remote)
	}
	if !h.host.did("firewall:8650:10.0.0.0/24") || !h.host.did("keyread") {
		t.Fatalf("host calls: %v", h.host.calls)
	}
	if cfg := h.config(); cfg.Listen != "10.0.0.5:8650" {
		t.Fatalf("listen %q", cfg.Listen)
	}

	// doctor --remote fails: remote mode goes off again, so does the rule.
	h.doctor = 1
	res, err = h.run()
	if err != nil {
		t.Fatal(err)
	}
	if res.Remote.Enabled || h.config().Listen != "" || !h.host.did("firewall-off") {
		t.Fatalf("after doctor FAIL: %+v listen %q calls %v", res.Remote, h.config().Listen, h.host.calls)
	}
}

// addIdentity appends an identity as "fbmcp ctl apikey new" would.
func addIdentity(t *testing.T, cfgPath string) {
	t.Helper()
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "identities:") {
			continue
		}
		out = append(out, sc.Text())
	}
	out = append(out, "identities:", "  - name: alice", "    key_env: FBMCP_KEY_ALICE")
	if err := os.WriteFile(cfgPath, []byte(strings.Join(out, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(cfgPath); err != nil {
		t.Fatalf("config with identity: %v", err)
	}
}

func TestParseHqbirdArgs(t *testing.T) {
	o, err := parseHqbirdArgs([]string{"--allow-from=10.0.0.0/24,10.1.0.7", "--port", "9650", "--dns", "a.example,b.example", "--json", "--agent-config", "x.json"})
	if err != nil {
		t.Fatal(err)
	}
	if o.port != 9650 || len(o.dns) != 2 || !o.jsonOut || len(o.agentConfigs) != 1 || len(o.allowFrom) != 1 {
		t.Fatalf("opts: %+v", o)
	}
	for _, bad := range [][]string{
		{"--nope"}, {"--port", "0"}, {"--port"}, {"--allow-from", "x.y"}, {"--json=1"}, {"--ip", "host"},
	} {
		if _, err := parseHqbirdArgs(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// The unit hqbird-install writes keeps the directives of the packaged unit.
func TestHqbirdUnitMatchesPackaging(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "packaging", "linux", "fbmcp.service"))
	if err != nil {
		t.Fatal(err)
	}
	unit := renderUnit(unitData{User: "firebird", Exe: "/opt/hqbird/fbmcp/fbmcp", Config: "/etc/fbmcp/fbmcp.yaml", ConfigDir: "/etc/fbmcp",
		RW: []string{"/var/lib/fbmcp", "/var/lib/fbmcp-backups", "/var/lib/fbmcp-work", "/etc/fbmcp", "/opt/hqbird", "/opt/hqbird-fbagent"}})
	directives := func(s string) map[string]string {
		m := map[string]string{}
		for _, ln := range strings.Split(s, "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(ln), "="); ok && !strings.HasPrefix(ln, "#") {
				m[k] = v
			}
		}
		return m
	}
	pkg, got := directives(string(b)), directives(unit)
	for _, k := range []string{"Type", "User", "Group", "Restart", "RestartSec", "TimeoutStopSec", "ProtectSystem", "ProtectHome", "PrivateTmp", "WantedBy", "After", "Wants"} {
		if pkg[k] != got[k] {
			t.Errorf("%s: packaging %q, hqbird-install %q", k, pkg[k], got[k])
		}
	}
	if _, ok := got["NoNewPrivileges"]; ok {
		t.Error("NoNewPrivileges blocks sudo for fb_service_*")
	}
	for _, p := range strings.Fields(pkg["ReadWritePaths"]) {
		if !strings.Contains(" "+got["ReadWritePaths"]+" ", " "+p+" ") {
			t.Errorf("ReadWritePaths lacks %s: %s", p, got["ReadWritePaths"])
		}
	}
	if got["ExecStart"] != "/opt/hqbird/fbmcp/fbmcp -config /etc/fbmcp/fbmcp.yaml -serve" {
		t.Errorf("ExecStart %q", got["ExecStart"])
	}
}
