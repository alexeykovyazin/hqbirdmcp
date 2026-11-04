// Package hqbirdsetup is the pure part of "fbmcp ctl hqbird-install"
// (docs/hqbird.md): the HQbird layout, the facts of each Firebird line and
// the reconcile of fbmcp.yaml. It never touches the host; the service, the
// ACLs and the firewall live in internal/ctl.
package hqbirdsetup

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aleks/fbmcp/internal/config"
)

// DefaultPort is the remote-mode port (8443 and 9443 belong to goafts).
const DefaultPort = 8650

// ServiceName is the one fbmcp service of the server.
const ServiceName = "fbmcp"

// Paths are the files of the fbmcp of this server.
type Paths struct {
	DataDir     string
	Config      string
	StateDir    string
	SecretsDir  string
	TLSDir      string
	ConnectFile string
}

// ManagedFile lists the entries this command owns.
func (p Paths) ManagedFile() string { return filepath.Join(p.StateDir, "hqbird-managed.json") }

// DefaultPaths returns the HQbird layout. dataDir puts every file under
// one folder (Windows --data-dir; tests on Linux).
func DefaultPaths(goos, dataDir string) Paths {
	if dataDir == "" && goos == "linux" {
		return Paths{Config: "/etc/fbmcp/fbmcp.yaml", StateDir: "/var/lib/fbmcp", SecretsDir: "/etc/fbmcp/secrets",
			TLSDir: "/etc/fbmcp/tls", ConnectFile: "/etc/fbmcp/connect.txt"}
	}
	if dataDir == "" {
		dataDir = `C:\HQbirdData\fbmcp`
	}
	j := func(s string) string { return joinFor(goos, dataDir, s) }
	return Paths{DataDir: dataDir, Config: j("fbmcp.yaml"), StateDir: j("state"), SecretsDir: j("secrets"),
		TLSDir: j("tls"), ConnectFile: j("connect.txt")}
}

// joinFor joins with the separator of goos, so the Windows layout is right
// when the tests run on Linux and the other way round.
func joinFor(goos, dir, name string) string {
	if goos == "windows" {
		return strings.TrimRight(dir, `\/`) + `\` + name
	}
	return strings.TrimRight(dir, "/") + "/" + name
}

// Line is one HQbird line: one Firebird and its fbagent. It is also the
// shape of one --lines-stdin entry; a field given there wins over files.
type Line struct {
	AgentConfig     string `json:"agent_config"`
	Line            string `json:"line,omitempty"`
	FirebirdPort    int    `json:"firebird_port,omitempty"`
	FirebirdService string `json:"firebird_service,omitempty"`
	FirebirdBin     string `json:"firebird_bin,omitempty"`
	FirebirdVersion string `json:"firebird_version,omitempty"`
	LocalAPIURL     string `json:"local_api_url,omitempty"`
	LocalAPIToken   string `json:"local_api_token,omitempty"`
	InstanceID      string `json:"instance_id,omitempty"`
	SysdbaUser      string `json:"sysdba_user,omitempty"`
	SysdbaPassword  string `json:"sysdba_password,omitempty"`
	// LocalAPIOff: local_api.enabled is false in agent_config.json.
	LocalAPIOff bool `json:"-"`
}

// StdinDoc is the --lines-stdin document.
type StdinDoc struct {
	Lines []Line `json:"lines"`
}

// Key identifies a line by its agent config file.
func (l Line) Key() string { return pathKey(l.AgentConfig) }

func pathKey(p string) string {
	return strings.ToLower(filepath.ToSlash(filepath.Clean(strings.TrimSpace(p))))
}

// Merge fills the empty fields of l from o.
func (l *Line) Merge(o Line) {
	str := func(dst *string, v string) {
		if *dst == "" {
			*dst = v
		}
	}
	str(&l.AgentConfig, o.AgentConfig)
	str(&l.Line, o.Line)
	if l.FirebirdPort == 0 {
		l.FirebirdPort = o.FirebirdPort
	}
	str(&l.FirebirdService, o.FirebirdService)
	str(&l.FirebirdBin, o.FirebirdBin)
	str(&l.FirebirdVersion, o.FirebirdVersion)
	str(&l.LocalAPIURL, o.LocalAPIURL)
	str(&l.LocalAPIToken, o.LocalAPIToken)
	str(&l.InstanceID, o.InstanceID)
	str(&l.SysdbaUser, o.SysdbaUser)
	str(&l.SysdbaPassword, o.SysdbaPassword)
	l.LocalAPIOff = l.LocalAPIOff || o.LocalAPIOff
}

// agentFile is the slice of agent_config.json this command reads.
type agentFile struct {
	Agent struct {
		ID string `json:"id"`
	} `json:"agent"`
	Firebird struct {
		Port        int    `json:"port"`
		InstallPath string `json:"install_path"`
		Version     string `json:"version"`
	} `json:"firebird"`
	LocalAPI struct {
		Enabled    *bool  `json:"enabled"`
		Listen     string `json:"listen"`
		Port       int    `json:"port"`
		Token      string `json:"token"`
		InstanceID string `json:"instance_id"`
	} `json:"local_api"`
}

// firebirdRecord is hqbird2027-firebird.json in the agent folder (the
// Firebird a HQbird 2027 setup installed).
type firebirdRecord struct {
	Root    string `json:"root"`
	Service string `json:"service"`
	Port    int    `json:"port"`
	Version string `json:"version"`
}

// FillFromFiles reads the agent folder and fills the empty fields of l.
// findService finds the Firebird service of an install folder when no
// record names it (nil: skip). It returns notes, never an error: a line
// with too few facts is reported by Problems.
func (l *Line) FillFromFiles(findService func(installPath string) string) []string {
	var notes []string
	dir := filepath.Dir(l.AgentConfig)
	if l.Line == "" {
		l.Line = LineName(dir)
	}
	var af agentFile
	if b, err := os.ReadFile(l.AgentConfig); err != nil {
		notes = append(notes, fmt.Sprintf("%s: %v", l.AgentConfig, err))
	} else if err := json.Unmarshal(b, &af); err != nil {
		notes = append(notes, fmt.Sprintf("%s: %v", l.AgentConfig, err))
	}
	var rec firebirdRecord
	if b, err := os.ReadFile(filepath.Join(dir, "hqbird2027-firebird.json")); err == nil {
		_ = json.Unmarshal(b, &rec)
	}
	if l.FirebirdPort == 0 {
		l.FirebirdPort = af.Firebird.Port
	}
	if l.FirebirdPort == 0 {
		l.FirebirdPort = rec.Port
	}
	root := af.Firebird.InstallPath
	if root == "" {
		root = rec.Root
	}
	if l.FirebirdBin == "" && root != "" {
		l.FirebirdBin = BinDir(root)
	}
	if l.FirebirdService == "" {
		l.FirebirdService = rec.Service
	}
	if l.FirebirdService == "" && root != "" && findService != nil {
		l.FirebirdService = findService(root)
	}
	if l.FirebirdVersion == "" {
		l.FirebirdVersion = rec.Version
	}
	if af.LocalAPI.Enabled != nil && !*af.LocalAPI.Enabled {
		l.LocalAPIOff = true
	}
	if l.LocalAPIURL == "" {
		l.LocalAPIURL = "http://" + effectiveListen(af.LocalAPI.Listen, af.LocalAPI.Port, l.FirebirdPort)
	}
	if l.LocalAPIToken == "" {
		if t := strings.TrimSpace(af.LocalAPI.Token); strings.HasPrefix(t, "enc:") {
			notes = append(notes, fmt.Sprintf("line %s: local_api.token is sealed; the setup passes it with --lines-stdin", l.Line))
		} else {
			l.LocalAPIToken = t
		}
	}
	if l.InstanceID == "" {
		l.InstanceID = firstNonEmpty(af.LocalAPI.InstanceID, af.Agent.ID)
	}
	return notes
}

// effectiveListen is fbagent's Local API address rule: listen, else
// 127.0.0.1:(10000 + local_api.port or firebird.port), else :13055.
func effectiveListen(listen string, apiPort, fbPort int) string {
	if s := strings.TrimSpace(listen); s != "" {
		return s
	}
	port := apiPort
	if port <= 0 {
		port = fbPort
	}
	if port > 0 {
		return fmt.Sprintf("127.0.0.1:%d", 10000+port)
	}
	return "127.0.0.1:13055"
}

// LoopbackURL is the Local API URL fbmcp calls: a listen on all addresses
// (0.0.0.0, ::) is reached on 127.0.0.1. ok is false for a URL that is not
// on loopback: fbmcp refuses such an agent entry (allow_non_loopback).
func LoopbackURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return raw, false
	}
	h := u.Hostname()
	ip := net.ParseIP(h)
	switch {
	case h == "localhost" || (ip != nil && ip.IsLoopback()):
		return strings.TrimRight(u.String(), "/"), true
	case h == "" || (ip != nil && ip.IsUnspecified()):
		u.Host = net.JoinHostPort("127.0.0.1", u.Port())
		return strings.TrimRight(u.String(), "/"), true
	}
	return raw, false
}

// BinDir is the folder with gbak: <root>/bin when it holds gbak (Linux,
// Firebird 2.5), else the root (Firebird 3+ on Windows).
func BinDir(root string) string {
	for _, n := range []string{"gbak", "gbak.exe"} {
		if _, err := os.Stat(filepath.Join(root, "bin", n)); err == nil {
			return filepath.Join(root, "bin")
		}
	}
	return root
}

// LineName is the line of an agent folder: fbagent-fb50 → fb50,
// fbagent-fb50-2 → fb50-2. Another folder name gives "".
func LineName(agentDir string) string {
	base := strings.ToLower(filepath.Base(filepath.Clean(agentDir)))
	if n, ok := strings.CutPrefix(base, "fbagent-"); ok {
		return n
	}
	return ""
}

var idRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// InstanceID is the fbmcp instance id of a line: the line name in lower
// case with "-" → "_" (fb50, fb50_2, p3050); else p<port>.
func InstanceID(l Line) string {
	id := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(l.Line)), "-", "_")
	if idRe.MatchString(id) {
		return id
	}
	return fmt.Sprintf("p%d", l.FirebirdPort)
}

// AgentID, TokenEnv and SysdbaEnv follow the fb_agent_discover rule.
func AgentID(port int) string   { return fmt.Sprintf("agent_p%d", port) }
func TokenEnv(port int) string  { return fmt.Sprintf("FBMCP_AGENT_TOKEN_P%d", port) }
func SysdbaEnv(port int) string { return fmt.Sprintf("FBMCP_FB_P%d_SYSDBA", port) }

// Problem says why a line cannot be managed ("" = it can).
func (l Line) Problem() string {
	switch {
	case l.FirebirdPort <= 0:
		return "no Firebird port"
	case l.FirebirdBin == "":
		return "no Firebird install folder"
	}
	return ""
}

// Managed is hqbird-managed.json: what this command added, so a later run
// changes only its own entries and never the operator's.
type Managed struct {
	Instances []string `json:"instances,omitempty"`
	Agents    []string `json:"agents,omitempty"`
	Secrets   []string `json:"secrets,omitempty"`
	Remote    Remote   `json:"remote,omitempty"`
	// Firewall are the Linux firewall rules added (removed with the last
	// line). Windows uses the rule name fbmcp-remote.
	Firewall  []string `json:"firewall,omitempty"`
	UpdatedAt string   `json:"updated_at,omitempty"`
}

// Remote is the remote mode asked for with --allow-from. It stays here, so
// a later run with no options keeps it, and turns it on once a key exists.
type Remote struct {
	AllowFrom []string `json:"allow_from,omitempty"`
	Port      int      `json:"port,omitempty"`
	DNS       []string `json:"dns,omitempty"`
	IP        string   `json:"ip,omitempty"`
}

// On reports remote mode asked for.
func (r Remote) On() bool { return len(r.AllowFrom) > 0 }

// PortOrDefault is the remote port.
func (r Remote) PortOrDefault() int {
	if r.Port > 0 {
		return r.Port
	}
	return DefaultPort
}

// AddAllowFrom adds addresses (IP or CIDR, comma lists too), keeps the
// order and drops duplicates. It returns an error for a bad address.
func (r *Remote) AddAllowFrom(list []string) error {
	seen := map[string]bool{}
	for _, a := range r.AllowFrom {
		seen[a] = true
	}
	for _, item := range list {
		for _, a := range strings.Split(item, ",") {
			a = strings.TrimSpace(a)
			if a == "" {
				continue
			}
			n, err := config.ParseNet(a)
			if err != nil {
				return err
			}
			s := n.String()
			if !strings.Contains(a, "/") {
				s = a // a bare IP stays as typed
			}
			if !seen[s] {
				seen[s] = true
				r.AllowFrom = append(r.AllowFrom, s)
			}
		}
	}
	return nil
}

// ReadManaged reads hqbird-managed.json; a missing file is an empty list.
func ReadManaged(path string) (Managed, error) {
	var m Managed
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// WriteManaged writes hqbird-managed.json.
func WriteManaged(path string, m Managed, now time.Time) error {
	m.UpdatedAt = now.UTC().Format(time.RFC3339)
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Plan is the outcome of Reconcile.
type Plan struct {
	Config  *config.Config
	Managed Managed
	// Lines are the managed lines, by Firebird port.
	Lines []PlanLine
	// Secrets are the secret files to write (name → value).
	Secrets map[string]string
	Changes []string
	Notes   []string
}

// PlanLine is one line in the result.
type PlanLine struct {
	Line         string `json:"line"`
	Instance     string `json:"instance"`
	Agent        string `json:"agent,omitempty"`
	FirebirdPort int    `json:"firebird_port"`
	// FirebirdService is the OS service the fbmcp service may start/stop.
	FirebirdService string `json:"firebird_service,omitempty"`
	AgentConfig     string `json:"-"`
}

// Reconcile computes the new fbmcp.yaml from the old one (nil: none yet),
// the managed list and the lines of the server. haveSecret reports a secret
// file that already exists. Entries not in the managed list are the
// operator's: they are never changed or removed, and a line whose id or
// address an operator entry already holds is left to that entry.
func Reconcile(old *config.Config, m Managed, lines []Line, p Paths, haveSecret func(string) bool) Plan {
	pl := Plan{Managed: Managed{Remote: m.Remote, Firewall: m.Firewall}, Secrets: map[string]string{}}
	var cfg config.Config
	if old != nil {
		cfg = *old
		cfg.Instances = append([]config.FBInstance(nil), old.Instances...)
		cfg.Agents = append([]config.Agent(nil), old.Agents...)
	}
	if cfg.State.Dir == "" {
		cfg.State.Dir = p.StateDir
		pl.Changes = append(pl.Changes, "set state.dir "+p.StateDir)
	}
	if cfg.Secrets.Dir == "" {
		cfg.Secrets.Dir = p.SecretsDir
		pl.Changes = append(pl.Changes, "set secrets.dir "+p.SecretsDir)
	}
	managedInst := set(m.Instances)
	managedAgent := set(m.Agents)

	sort.Slice(lines, func(i, j int) bool { return lines[i].FirebirdPort < lines[j].FirebirdPort })
	wantInst := map[string]config.FBInstance{}
	wantAgent := map[string]config.Agent{}
	seenPort := map[int]bool{}
	for _, l := range lines {
		if pr := l.Problem(); pr != "" {
			pl.Notes = append(pl.Notes, fmt.Sprintf("line %s (%s) skipped: %s", firstNonEmpty(l.Line, "?"), l.AgentConfig, pr))
			continue
		}
		if seenPort[l.FirebirdPort] {
			pl.Notes = append(pl.Notes, fmt.Sprintf("line %s (%s) skipped: Firebird port %d is already used by another line", l.Line, l.AgentConfig, l.FirebirdPort))
			continue
		}
		seenPort[l.FirebirdPort] = true
		id := InstanceID(l)
		if _, dup := wantInst[id]; dup {
			pl.Notes = append(pl.Notes, fmt.Sprintf("line %s (%s) skipped: instance id %s is already used by another line", l.Line, l.AgentConfig, id))
			continue
		}
		if owner := operatorInstance(cfg.Instances, managedInst, id, l.FirebirdPort); owner != "" {
			pl.Notes = append(pl.Notes, fmt.Sprintf("line %s: the operator's instance %s already covers it; not managed", l.Line, owner))
			continue
		}
		in := config.FBInstance{ID: id, Addr: fmt.Sprintf("localhost:%d", l.FirebirdPort), BinDir: l.FirebirdBin,
			Service: l.FirebirdService, Version: l.FirebirdVersion}
		if env := SysdbaEnv(l.FirebirdPort); l.SysdbaPassword != "" || haveSecret(env) {
			user := firstNonEmpty(l.SysdbaUser, "SYSDBA")
			in.ServiceUser, in.ServiceSecretEnv = user, env
			in.DefaultAdminUser, in.DefaultAdminSecretEnv = user, env
			if l.SysdbaPassword != "" {
				pl.Secrets[env] = l.SysdbaPassword
			}
		} else {
			pl.Notes = append(pl.Notes, fmt.Sprintf("line %s: no SYSDBA password; Services API tools need: fbmcp ctl secret set --dir %s %s", l.Line, cfg.Secrets.Dir, env))
		}
		wantInst[id] = in
		pline := PlanLine{Line: l.Line, Instance: id, FirebirdPort: l.FirebirdPort, FirebirdService: l.FirebirdService, AgentConfig: l.AgentConfig}

		aid := AgentID(l.FirebirdPort)
		env := TokenEnv(l.FirebirdPort)
		apiURL, loopback := LoopbackURL(l.LocalAPIURL)
		switch {
		case l.LocalAPIOff:
			pl.Notes = append(pl.Notes, fmt.Sprintf("line %s: local_api.enabled is false; no agent entry", l.Line))
		case !loopback:
			pl.Notes = append(pl.Notes, fmt.Sprintf("line %s: the Local API %s is not on loopback; no agent entry", l.Line, l.LocalAPIURL))
		case l.LocalAPIToken == "" && !haveSecret(env):
			pl.Notes = append(pl.Notes, fmt.Sprintf("line %s: no Local API token; no agent entry", l.Line))
		default:
			if owner := operatorAgent(cfg.Agents, managedAgent, aid, apiURL); owner != "" {
				pl.Notes = append(pl.Notes, fmt.Sprintf("line %s: the operator's agent %s already covers it", l.Line, owner))
				break
			}
			if l.LocalAPIToken != "" {
				pl.Secrets[env] = l.LocalAPIToken
			}
			wantAgent[aid] = config.Agent{ID: aid, BaseURL: apiURL, TokenEnv: env, InstanceID: l.InstanceID, ConfigPath: l.AgentConfig}
			pline.Agent = aid
		}
		pl.Lines = append(pl.Lines, pline)
	}

	// Instances: update or drop the managed ones, keep the operator's, add new.
	used := map[string]bool{}
	for _, d := range cfg.Databases {
		used[d.Instance] = true
	}
	var insts []config.FBInstance
	for _, in := range cfg.Instances {
		if !managedInst[in.ID] {
			insts = append(insts, in)
			continue
		}
		want, ok := wantInst[in.ID]
		if !ok {
			if used[in.ID] {
				insts = append(insts, in)
				pl.Managed.Instances = append(pl.Managed.Instances, in.ID)
				pl.Notes = append(pl.Notes, fmt.Sprintf("instance %s: its line is gone, but databases use it; kept", in.ID))
				continue
			}
			pl.Changes = append(pl.Changes, "remove instance "+in.ID)
			continue
		}
		merged := mergeInstance(in, want)
		if merged != in {
			pl.Changes = append(pl.Changes, "update instance "+in.ID)
		}
		insts = append(insts, merged)
		delete(wantInst, in.ID)
		pl.Managed.Instances = append(pl.Managed.Instances, in.ID)
	}
	for _, l := range pl.Lines {
		if want, ok := wantInst[l.Instance]; ok {
			insts = append(insts, want)
			pl.Managed.Instances = append(pl.Managed.Instances, want.ID)
			pl.Changes = append(pl.Changes, "add instance "+want.ID)
		}
	}
	cfg.Instances = insts

	var agents []config.Agent
	for _, ag := range cfg.Agents {
		if !managedAgent[ag.ID] {
			agents = append(agents, ag)
			continue
		}
		want, ok := wantAgent[ag.ID]
		if !ok {
			pl.Changes = append(pl.Changes, "remove agent "+ag.ID)
			continue
		}
		merged := ag
		merged.BaseURL, merged.TokenEnv, merged.InstanceID, merged.ConfigPath = want.BaseURL, want.TokenEnv, want.InstanceID, want.ConfigPath
		if !sameAgent(merged, ag) {
			pl.Changes = append(pl.Changes, "update agent "+ag.ID)
		}
		agents = append(agents, merged)
		delete(wantAgent, ag.ID)
		pl.Managed.Agents = append(pl.Managed.Agents, ag.ID)
	}
	for _, l := range pl.Lines {
		if want, ok := wantAgent[l.Agent]; ok {
			agents = append(agents, want)
			pl.Managed.Agents = append(pl.Managed.Agents, want.ID)
			pl.Changes = append(pl.Changes, "add agent "+want.ID)
		}
	}
	cfg.Agents = agents

	// Secrets this command wrote and still needs.
	keep := map[string]bool{}
	for name := range pl.Secrets {
		keep[name] = true
	}
	for _, in := range cfg.Instances {
		if contains(pl.Managed.Instances, in.ID) && in.ServiceSecretEnv != "" {
			keep[in.ServiceSecretEnv] = true
		}
	}
	for _, ag := range cfg.Agents {
		if contains(pl.Managed.Agents, ag.ID) {
			keep[ag.TokenEnv] = true
		}
	}
	for name := range keep {
		pl.Managed.Secrets = append(pl.Managed.Secrets, name)
	}
	sort.Strings(pl.Managed.Secrets)
	pl.Config = &cfg
	return pl
}

// StaleSecrets are secret files this command wrote that no entry uses now.
func StaleSecrets(old, now Managed) []string {
	keep := set(now.Secrets)
	var out []string
	for _, s := range old.Secrets {
		if !keep[s] {
			out = append(out, s)
		}
	}
	return out
}

// mergeInstance updates the fields this command owns and keeps the rest
// (backup and work folders, RO defaults the operator set).
func mergeInstance(have, want config.FBInstance) config.FBInstance {
	out := have
	out.Addr, out.BinDir = want.Addr, want.BinDir
	if want.Service != "" {
		out.Service = want.Service
	}
	if want.Version != "" {
		out.Version = want.Version
	}
	if want.ServiceSecretEnv != "" {
		out.ServiceUser, out.ServiceSecretEnv = want.ServiceUser, want.ServiceSecretEnv
		if out.DefaultAdminUser == "" || out.DefaultAdminSecretEnv == want.ServiceSecretEnv {
			out.DefaultAdminUser, out.DefaultAdminSecretEnv = want.DefaultAdminUser, want.DefaultAdminSecretEnv
		}
	}
	return out
}

func sameAgent(a, b config.Agent) bool {
	return a.ID == b.ID && a.BaseURL == b.BaseURL && a.TokenEnv == b.TokenEnv && a.InstanceID == b.InstanceID && a.ConfigPath == b.ConfigPath
}

// operatorInstance returns an operator entry that holds the id or the port.
func operatorInstance(insts []config.FBInstance, managed map[string]bool, id string, port int) string {
	for _, in := range insts {
		if managed[in.ID] {
			continue
		}
		if in.ID == id || addrPort(in.Addr) == port {
			return in.ID
		}
	}
	return ""
}

// operatorAgent returns an operator entry that holds the id or the URL.
func operatorAgent(agents []config.Agent, managed map[string]bool, id, url string) string {
	norm := func(s string) string { return strings.TrimRight(strings.ToLower(s), "/") }
	for _, ag := range agents {
		if managed[ag.ID] {
			continue
		}
		if ag.ID == id || norm(ag.BaseURL) == norm(url) {
			return ag.ID
		}
	}
	return ""
}

func addrPort(addr string) int {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return 3050
	}
	var p int
	fmt.Sscanf(addr[i+1:], "%d", &p)
	return p
}

func set(list []string) map[string]bool {
	m := map[string]bool{}
	for _, s := range list {
		m[s] = true
	}
	return m
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
