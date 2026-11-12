package ctl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/aleks/fbmcp/internal/agentdiscover"
	"github.com/aleks/fbmcp/internal/attach"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/configedit"
	"github.com/aleks/fbmcp/internal/hqbirdsetup"
	"github.com/aleks/fbmcp/internal/posture"
	"github.com/aleks/fbmcp/internal/secrets"
)

// Version is the fbmcp version; the binaries set it from main.version
// (ADR-038).
var Version = "dev"

// hqbirdOpts are the hqbird-install options (docs/hqbird.md).
type hqbirdOpts struct {
	dataDir, config, stateDir, secretsDir string
	agentConfigs                          []string
	linesStdin                            bool
	allowFrom                             []string
	port                                  int
	dns                                   []string
	ip                                    string
	noService, dryRun, jsonOut            bool
}

func parseHqbirdArgs(args []string) (hqbirdOpts, error) {
	var o hqbirdOpts
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(a, "=")
		switch name {
		case "--lines-stdin", "--no-service", "--dry-run", "--json":
			if hasVal {
				return o, fmt.Errorf("%s takes no value", name)
			}
			switch name {
			case "--lines-stdin":
				o.linesStdin = true
			case "--no-service":
				o.noService = true
			case "--dry-run":
				o.dryRun = true
			case "--json":
				o.jsonOut = true
			}
			continue
		case "--data-dir", "--config", "--state-dir", "--secrets-dir", "--agent-config", "--allow-from", "--port", "--dns", "--ip":
		default:
			return o, fmt.Errorf("unknown option %s", a)
		}
		v := val
		if !hasVal {
			if i+1 >= len(args) {
				return o, fmt.Errorf("%s needs a value", name)
			}
			i++
			v = args[i]
		}
		switch name {
		case "--data-dir":
			o.dataDir = v
		case "--config":
			o.config = v
		case "--state-dir":
			o.stateDir = v
		case "--secrets-dir":
			o.secretsDir = v
		case "--agent-config":
			o.agentConfigs = append(o.agentConfigs, v)
		case "--allow-from":
			o.allowFrom = append(o.allowFrom, v)
		case "--port":
			p, err := strconv.Atoi(v)
			if err != nil || p < 1 || p > 65535 {
				return o, fmt.Errorf("bad --port %q", v)
			}
			o.port = p
		case "--dns":
			o.dns = append(o.dns, splitList(v)...)
		case "--ip":
			if net.ParseIP(v) == nil {
				return o, fmt.Errorf("bad --ip %q", v)
			}
			o.ip = v
		}
	}
	var r hqbirdsetup.Remote
	if err := r.AddAllowFrom(o.allowFrom); err != nil {
		return o, fmt.Errorf("--allow-from: %v", err)
	}
	return o, nil
}

// hqbirdResult is the --json document (docs/hqbird.md).
type hqbirdResult struct {
	Version      string                 `json:"version"`
	Config       string                 `json:"config"`
	StateDir     string                 `json:"state_dir"`
	Service      string                 `json:"service"`
	ServiceState string                 `json:"service_state"`
	Lines        []hqbirdsetup.PlanLine `json:"lines"`
	Remote       hqbirdRemote           `json:"remote"`
	ConnectFile  string                 `json:"connect_file,omitempty"`
	Changes      []string               `json:"changes,omitempty"`
	Warnings     []string               `json:"warnings"`
	DryRun       bool                   `json:"dry_run,omitempty"`
}

type hqbirdRemote struct {
	Enabled   bool     `json:"enabled"`
	Pending   bool     `json:"pending,omitempty"`
	URL       string   `json:"url,omitempty"`
	Pin       string   `json:"pin,omitempty"`
	AllowFrom []string `json:"allow_from,omitempty"`
}

// hqbirdHost is the host side: the OS service, ACLs, firewall. The tests
// use a fake.
type hqbirdHost interface {
	// FirebirdService finds the service of a Firebird install folder.
	FirebirdService(installPath string) string
	// Account is the service account ("NT SERVICE\fbmcp", firebird).
	Account() string
	// PrepareFolders makes the fbmcp folders with their access rules.
	PrepareFolders(p hqbirdsetup.Paths) error
	// AfterWrite gives the service account its access to written files.
	AfterWrite(paths ...string) error
	// EnsureService creates or updates the service (not started).
	EnsureService(exe, cfgPath string) error
	// GrantFirebird lets the service start and stop these Firebird services.
	GrantFirebird(services []string) []string
	// GrantAgentConfigs lets the service read the agent configs.
	GrantAgentConfigs(paths []string) []string
	// GrantKeyRead lets the service read the TLS key.
	GrantKeyRead(keyPath string) error
	// Firewall opens port only for allow; m records what was added.
	Firewall(port int, allow []string, exe string, m *hqbirdsetup.Managed) error
	// RemoveFirewall removes the rules of m.
	RemoveFirewall(m *hqbirdsetup.Managed) error
	// State is running, stopped, failed or absent.
	State() string
	// Start starts the service, or restarts it when EnsureService changed
	// its definition while it ran.
	Start() error
	StopDisable() error
}

// hqbirdEnv is everything runHqbirdInstall uses from outside.
type hqbirdEnv struct {
	host     hqbirdHost
	discover func() ([]agentdiscover.Candidate, []string, error)
	stdin    io.Reader
	log      io.Writer
	goos     string
	exe      string
	now      func() time.Time
	ping     func(cfgPath string, wait time.Duration) error
	doctor   func(cfgPath string) int
	hostIP   func() string
	hostName func() string
	settle   time.Duration
}

func cmdHqbirdInstall(args []string) int {
	o, err := parseHqbirdArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hqbird-install:", err)
		fmt.Fprintln(os.Stderr, "usage: fbmcp ctl hqbird-install [--data-dir DIR] [--config FILE] [--state-dir DIR] [--secrets-dir DIR] [--agent-config FILE]... [--lines-stdin] [--allow-from CIDR]... [--port N] [--dns NAME]... [--ip ADDR] [--no-service] [--dry-run] [--json]")
		return 2
	}
	exe, _ := os.Executable()
	env := hqbirdEnv{
		host: newHqbirdHost(), discover: agentdiscover.Discover, stdin: os.Stdin, log: os.Stderr,
		goos: runtime.GOOS, exe: exe, now: time.Now, settle: 3 * time.Second,
		ping: func(cfgPath string, wait time.Duration) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			conn, err := attach.Dial(cfg.State.Dir, wait)
			if err != nil {
				return err
			}
			return conn.Close()
		},
		doctor: func(cfgPath string) int {
			// doctor prints its report on stdout; with --json stdout holds
			// only the result.
			old := os.Stdout
			os.Stdout = os.Stderr
			defer func() { os.Stdout = old }()
			return doctorRemote(cfgPath)
		},
		hostIP:   primaryIP,
		hostName: func() string { h, _ := os.Hostname(); return strings.ToLower(h) },
	}
	res, err := runHqbirdInstall(o, env)
	if o.jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hqbird-install:", err)
		return 1
	}
	if !o.jsonOut {
		fmt.Printf("fbmcp %s: %d line(s), service %s", res.Version, len(res.Lines), res.ServiceState)
		if res.Remote.Enabled {
			fmt.Printf(", remote %s (pin %s)", res.Remote.URL, res.Remote.Pin)
		}
		fmt.Println()
		if res.ConnectFile != "" {
			fmt.Println("client notes:", res.ConnectFile)
		}
	}
	return 0
}

// runHqbirdInstall is the command without the process around it.
func runHqbirdInstall(o hqbirdOpts, env hqbirdEnv) (hqbirdResult, error) {
	if o.dataDir != "" {
		o.dataDir = filepath.Clean(o.dataDir)
	}
	p := hqbirdsetup.DefaultPaths(env.goos, o.dataDir)
	if o.config != "" {
		p.Config = o.config
	}
	if o.stateDir != "" {
		p.StateDir = o.stateDir
	}
	if o.secretsDir != "" {
		p.SecretsDir = o.secretsDir
	}
	res := hqbirdResult{Version: Version, Config: p.Config, StateDir: p.StateDir, Service: hqbirdsetup.ServiceName,
		ServiceState: "absent", Lines: []hqbirdsetup.PlanLine{}, Warnings: []string{}, DryRun: o.dryRun}
	say := func(f string, a ...any) { fmt.Fprintf(env.log, "== "+f+"\n", a...) }
	warn := func(f string, a ...any) {
		s := fmt.Sprintf(f, a...)
		res.Warnings = append(res.Warnings, s)
		fmt.Fprintln(env.log, "warning:", s)
	}
	act := !o.dryRun
	host := act && !o.noService
	if o.dryRun {
		say("dry run: nothing is changed")
	}

	// 1. Lines: stdin first (it wins), then --agent-config, then discovery.
	var lines []*hqbirdsetup.Line
	byKey := map[string]*hqbirdsetup.Line{}
	add := func(l hqbirdsetup.Line) {
		if strings.TrimSpace(l.AgentConfig) == "" {
			return
		}
		if have := byKey[l.Key()]; have != nil {
			have.Merge(l)
			return
		}
		nl := l
		byKey[l.Key()] = &nl
		lines = append(lines, &nl)
	}
	if o.linesStdin {
		var doc hqbirdsetup.StdinDoc
		if err := json.NewDecoder(io.LimitReader(env.stdin, 1<<20)).Decode(&doc); err != nil {
			return res, fmt.Errorf("--lines-stdin: %v", err)
		}
		for _, l := range doc.Lines {
			add(l)
		}
	}
	for _, a := range o.agentConfigs {
		if abs, err := filepath.Abs(a); err == nil {
			a = abs
		}
		add(hqbirdsetup.Line{AgentConfig: a})
	}
	cands, _, err := env.discover()
	if err != nil {
		// Without the list of lines a reconcile would drop every line.
		return res, fmt.Errorf("find the fbagent services: %v", err)
	}
	for _, c := range cands {
		if c.ConfigPath == "" || strings.HasPrefix(c.Service, "(") { // registry-only row: no service
			continue
		}
		add(hqbirdsetup.Line{AgentConfig: c.ConfigPath, FirebirdPort: c.FirebirdPort, LocalAPIURL: c.BaseURL, InstanceID: c.InstanceID})
	}
	var found []hqbirdsetup.Line
	for _, l := range lines {
		for _, n := range l.FillFromFiles(env.host.FirebirdService) {
			fmt.Fprintln(env.log, "note:", n)
		}
		found = append(found, *l)
	}
	say("%d line(s) found", len(found))

	// 2. Config and the managed list.
	var old *config.Config
	var oldBody []byte
	if b, err := os.ReadFile(p.Config); err == nil {
		oldBody = b
		if old, err = config.Load(p.Config); err != nil {
			return res, fmt.Errorf("%v (fix it first; nothing was changed)", err)
		}
		if old.State.Dir != "" {
			p.StateDir = old.State.Dir
		}
		if old.Secrets.Dir != "" {
			p.SecretsDir = old.Secrets.Dir
		}
		res.StateDir = p.StateDir
	} else if !os.IsNotExist(err) {
		return res, err
	}
	m, err := hqbirdsetup.ReadManaged(p.ManagedFile())
	if err != nil {
		return res, err
	}
	if err := m.Remote.AddAllowFrom(o.allowFrom); err != nil {
		return res, err
	}
	if o.port > 0 {
		m.Remote.Port = o.port
	}
	for _, d := range o.dns {
		if !containsFold(m.Remote.DNS, d) {
			m.Remote.DNS = append(m.Remote.DNS, d)
		}
	}
	if o.ip != "" {
		m.Remote.IP = o.ip
	}
	haveSecret := func(name string) bool { _, err := secrets.ReadFile(p.SecretsDir, name); return err == nil }
	plan := hqbirdsetup.Reconcile(old, m, found, p, haveSecret)
	rcmHost, rcmFound, rcmNotes := hqbirdsetup.DiscoverRCM(p.RCMConfig, found)
	plan.Notes = append(plan.Notes, rcmNotes...)
	plan.ApplyRCM(m, rcmHost, rcmFound, haveSecret)
	for _, n := range plan.Notes {
		warn("%s", n)
	}
	res.Lines = append(res.Lines, plan.Lines...)
	cfg := plan.Config

	// No instance left: nothing to serve. The config keeps its last
	// entries (a config with no instance is invalid); the managed list
	// keeps them too, so a new line replaces them.
	if len(cfg.Instances) == 0 {
		say("no Firebird line: the fbmcp service is stopped")
		if host {
			if env.host.State() != "absent" {
				if err := env.host.StopDisable(); err != nil {
					warn("stop the fbmcp service: %v", err)
				}
			}
			if err := env.host.RemoveFirewall(&m); err != nil {
				warn("firewall: %v", err)
			}
			m.Firewall = nil
			if old != nil {
				if err := hqbirdsetup.WriteManaged(p.ManagedFile(), m, env.now()); err != nil {
					warn("%v", err)
				}
			}
		}
		res.ServiceState = hqbirdState(env.host, o)
		return res, nil
	}

	// 3. Remote mode.
	ip := firstNonEmptyStr(m.Remote.IP, env.hostIP())
	rs := hqbirdsetup.RemoteState{Listen: strings.TrimSpace(cfg.Listen)}
	if plan.Managed.Remote.On() && ip == "" {
		warn("remote mode: no IP address of this server found; pass --ip")
	} else {
		rs = hqbirdsetup.ApplyRemote(cfg, plan.Managed.Remote, ip, p)
	}
	changes := append(append([]string{}, plan.Changes...), rs.Changes...)
	if host {
		if err := env.host.PrepareFolders(p); err != nil {
			return res, fmt.Errorf("folders: %v", err)
		}
	} else if act {
		for _, d := range []string{filepath.Dir(p.Config), p.StateDir, p.SecretsDir} {
			if err := os.MkdirAll(d, 0o750); err != nil {
				return res, err
			}
		}
	}
	pin := ""
	if plan.Managed.Remote.On() && cfg.TLS.Cert != "" {
		pin, err = ensureTLS(cfg, plan.Managed.Remote, ip, env, act)
		if err != nil {
			warn("TLS: %v; remote mode stays off", err)
			cfg.Listen, rs.Listen = "", ""
		} else if host {
			if err := env.host.GrantKeyRead(cfg.TLS.Key); err != nil {
				warn("TLS key access for %s: %v", env.host.Account(), err)
			}
		}
	}

	// The allow-from list lives only in the firewall rule: the kernel does
	// not check source addresses. So remote mode needs a working firewall
	// (lab 2026-11: ufw inactive, every address reached the port). The rule
	// goes first; the config with listen is written only after it.
	if host && rs.Listen != "" {
		if err := env.host.Firewall(portOf(rs.Listen), plan.Managed.Remote.AllowFrom, env.exe, &plan.Managed); err != nil {
			if plan.Managed.Remote.On() {
				warn("remote mode stays off: firewall: %v; turn the firewall on, then run fbmcp ctl hqbird-install again", err)
				_ = env.host.RemoveFirewall(&plan.Managed)
				plan.Managed.Firewall = nil
				cfg.Listen, rs.Listen = "", ""
				changes = dropPrefix(changes, "set listen ")
			} else {
				// A listen the operator set: theirs to protect.
				warn("firewall: %v", err)
			}
		}
	}

	// 4. Secrets and config.
	if err := cfg.Validate(); err != nil {
		return res, fmt.Errorf("the new config is invalid (nothing was changed): %v", err)
	}
	for _, name := range sortedKeys(plan.Secrets) {
		if cur, err := secrets.ReadFile(p.SecretsDir, name); err == nil && cur == strings.TrimSpace(plan.Secrets[name]) {
			continue
		}
		changes = append(changes, "write secret "+name)
		if act {
			if err := secrets.WriteFile(p.SecretsDir, name, plan.Secrets[name]); err != nil {
				return res, fmt.Errorf("secret %s: %v", name, err)
			}
		}
	}
	for _, name := range hqbirdsetup.StaleSecrets(m, plan.Managed) {
		changes = append(changes, "remove secret "+name)
		if act {
			_ = secrets.DropFile(p.SecretsDir, name)
		}
	}
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return res, err
	}
	if !bytes.Equal(body, oldBody) {
		if oldBody == nil {
			changes = append(changes, "write "+p.Config)
		}
		if act {
			if err := configedit.AtomicWrite(p.Config, string(body)); err != nil {
				return res, err
			}
		}
	}
	res.Changes = changes
	for _, c := range changes {
		fmt.Fprintln(env.log, "+", c)
	}

	// 5. Service, rights, posture, start.
	if host {
		if err := env.host.AfterWrite(p.Config); err != nil {
			warn("config access: %v", err)
		}
		if err := env.host.EnsureService(env.exe, p.Config); err != nil {
			return res, fmt.Errorf("service: %v", err)
		}
		var svcs, agentCfgs []string
		for _, l := range plan.Lines {
			if l.FirebirdService != "" {
				svcs = append(svcs, l.FirebirdService)
			}
			if l.Agent != "" {
				agentCfgs = append(agentCfgs, l.AgentConfig)
			}
		}
		for _, w := range env.host.GrantFirebird(svcs) {
			warn("%s", w)
		}
		for _, w := range env.host.GrantAgentConfigs(agentCfgs) {
			warn("%s", w)
		}
		if ok, report := posture.Report(cfg); ok {
			if err := posture.WriteMarker(cfg.State.Dir, "fbmcp ctl hqbird-install"); err != nil {
				warn("posture marker: %v", err)
			} else if err := env.host.AfterWrite(posture.Marker(cfg.State.Dir)); err != nil {
				warn("posture marker access: %v", err)
			}
		} else {
			warn("posture check failed; fb_service_* will refuse:\n%s", report)
		}
		if err := env.host.Start(); err != nil {
			warn("start the fbmcp service: %v", err)
		}
		if err := env.ping(p.Config, 30*time.Second); err != nil {
			warn("the fbmcp kernel does not answer: %v", err)
		}
		if rs.Listen != "" {
			// The kernel applies a new listen within seconds (hot reload).
			time.Sleep(env.settle)
			if env.doctor(p.Config) != 0 {
				warn("doctor --remote found a FAIL: remote mode is turned off again")
				cfg.Listen, rs.Listen = "", ""
				if b, err := yaml.Marshal(cfg); err == nil {
					_ = configedit.AtomicWrite(p.Config, string(b))
				}
				_ = env.host.RemoveFirewall(&plan.Managed)
				plan.Managed.Firewall = nil
			}
		}
	}
	res.ServiceState = hqbirdState(env.host, o)

	// 6. Result, connect.txt, managed list.
	res.Remote = hqbirdRemote{Enabled: rs.Listen != "", Pending: rs.Pending, AllowFrom: plan.Managed.Remote.AllowFrom}
	if rs.Listen != "" {
		// A name from --dns, else the --ip the operator gave (the host name
		// often does not resolve on the admin PC), else the host name.
		res.Remote.URL = hqbirdsetup.URL(rs.Listen, firstNonEmptyStr(firstStr(plan.Managed.Remote.DNS), plan.Managed.Remote.IP, env.hostName()))
		res.Remote.Pin = pin
	}
	if rs.Pending {
		// apikey new writes fbmcp.yaml: the service account may not.
		asAdmin := "sudo fbmcp ctl"
		if env.goos == "windows" {
			asAdmin = "in an Administrator prompt: fbmcp ctl"
		}
		warn("remote mode needs one API key first: %s apikey new <name> --max-tier 2 --config %q, then the same way fbmcp ctl hqbird-install", asAdmin, p.Config)
	}
	text := hqbirdsetup.RenderConnect(hqbirdsetup.ConnectInfo{Server: cfg.ServerNameOrDefault(), URL: res.Remote.URL, Pin: pin,
		AllowFrom: plan.Managed.Remote.AllowFrom, Pending: rs.Pending, Config: p.Config, StateDir: cfg.State.Dir, Windows: env.goos == "windows"})
	if act {
		if err := os.WriteFile(p.ConnectFile, []byte(text), 0o644); err != nil {
			warn("connect.txt: %v", err)
		} else {
			res.ConnectFile = p.ConnectFile
		}
		if err := hqbirdsetup.WriteManaged(p.ManagedFile(), plan.Managed, env.now()); err != nil {
			warn("%v", err)
		}
		if host {
			if err := env.host.AfterWrite(p.ConnectFile, p.ManagedFile()); err != nil {
				warn("file access: %v", err)
			}
		}
	}
	return res, nil
}

// ensureTLS makes the self-signed pair when it is missing and returns the
// pin of the certificate.
func ensureTLS(cfg *config.Config, r hqbirdsetup.Remote, ip string, env hqbirdEnv, act bool) (string, error) {
	if c, err := readCert(cfg.TLS.Cert); err == nil {
		if _, kerr := os.Stat(cfg.TLS.Key); kerr == nil {
			return spkiPin(c), nil
		}
	}
	if !act {
		return "(new)", nil
	}
	dns := append([]string{}, r.DNS...)
	if h := env.hostName(); h != "" && !containsFold(dns, h) {
		dns = append(dns, h)
	}
	var ips []net.IP
	if p := net.ParseIP(ip); p != nil {
		ips = append(ips, p)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.TLS.Cert), 0o750); err != nil {
		return "", err
	}
	c, err := selfsign(cfg.TLS.Cert, cfg.TLS.Key, dns, ips, 825, true, false)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(env.log, "+ self-signed certificate %s (%s)\n", cfg.TLS.Cert, strings.Join(dns, ", "))
	return spkiPin(c), nil
}

func hqbirdState(h hqbirdHost, o hqbirdOpts) string {
	if o.noService {
		return "absent"
	}
	return h.State()
}

// primaryIP is the address of the default route (no packet is sent), else
// the first non-loopback IPv4 address.
func primaryIP() string {
	if c, err := net.Dial("udp", "192.0.2.1:9"); err == nil {
		defer c.Close()
		if a, ok := c.LocalAddr().(*net.UDPAddr); ok && !a.IP.IsLoopback() {
			return a.IP.String()
		}
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			return n.IP.String()
		}
	}
	return ""
}

func portOf(listen string) int {
	_, ps, _ := net.SplitHostPort(listen)
	p, _ := strconv.Atoi(ps)
	return p
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

func firstStr(list []string) string {
	if len(list) == 0 {
		return ""
	}
	return list[0]
}

// dropPrefix returns list without the items that start with prefix.
func dropPrefix(list []string, prefix string) []string {
	var out []string
	for _, s := range list {
		if !strings.HasPrefix(s, prefix) {
			out = append(out, s)
		}
	}
	return out
}

func firstNonEmptyStr(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
