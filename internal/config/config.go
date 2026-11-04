// Package config implements the P1.1 database registry & server policy
// configuration (phase1_plan.md). YAML on disk, validated on load.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/aleks/fbmcp/internal/secrets"

	"gopkg.in/yaml.v3"
)

// Config is the root of fbmcp.yaml.
type Config struct {
	State      State         `yaml:"state"`
	Instances  []FBInstance  `yaml:"instances"`
	Databases  []Database    `yaml:"databases"`
	Notify     Notify        `yaml:"notify"`
	Listen     string        `yaml:"listen"` // empty = stdio only (P5.1)
	TLS        TLS           `yaml:"tls"`
	Identities []APIIdentity `yaml:"identities"`
	// AllowedOrigins is the browser-CSRF Origin allowlist for /mcp and /sse.
	// E.1 default-deny: requests carrying an Origin header not in the list
	// get 403 (an empty list rejects every Origin); requests with NO Origin
	// header are still allowed (non-browser clients send none — the threat
	// is a browser being used as a confused deputy). Remote mode refuses to
	// START with an empty allowlist (CheckRemote).
	AllowedOrigins []string `yaml:"allowed_origins"`
	// LocalMaxTier caps the stdio-local fallback identity (WS2.3). 0 in
	// YAML means "not set" — use LocalMaxTierOrDefault (default 2).
	LocalMaxTier int       `yaml:"local_max_tier"`
	Limits       Limits    `yaml:"limits"`
	Retention    Retention `yaml:"retention"`
	Trends       Trends    `yaml:"trends"`
	Agents       []Agent   `yaml:"agents"`
	Grid         Grid      `yaml:"grid"`
	// ServerName names this server in fb_ping, fb_db_list and webhook
	// events (phase 12 B5), so one client or channel can serve many
	// servers. Empty → the OS host name (ServerNameOrDefault).
	ServerName string `yaml:"server_name"`
	// Secrets configures the file secret source (phase 12 B0.9).
	Secrets    Secrets `yaml:"secrets"`
	SourcePath string  `yaml:"-"`
}

// Secrets is the optional file secret source (phase 12 B0.9): one file per
// secret name (<dir>/FBMCP_KEY_ALICE), readable only by the service
// account. Lookup order: env → file → OS keyring. The config watcher also
// watches this folder, so a changed file applies with no restart.
type Secrets struct {
	Dir string `yaml:"dir"`
}

// ServerNameOrDefault returns server_name, or the OS host name.
func (c *Config) ServerNameOrDefault() string {
	if n := strings.TrimSpace(c.ServerName); n != "" {
		return n
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return strings.ToLower(h)
	}
	return "fbmcp"
}

// Agent is one co-located fbagent Local API endpoint (phase9_plan.md /
// ADR-031): the loopback HTTP surface of the HQBird agent service. The
// bridge tools proxy it; base_url only ever comes from this config, never
// from tool arguments (SSRF stance).
type Agent struct {
	ID         string `yaml:"id"`
	BaseURL    string `yaml:"base_url"`    // e.g. http://127.0.0.1:13050
	TokenEnv   string `yaml:"token_env"`   // env var name (keyring fallback) holding the Bearer token
	InstanceID string `yaml:"instance_id"` // used in /v1/instances/{id} paths; default "fb1"
	// TimeoutSeconds bounds bridge polling of async agent operations; it must
	// exceed the agent's restart_timeout_seconds (default 120) or long
	// restarts surface as client timeouts. 0 → 130.
	TimeoutSeconds int `yaml:"timeout_seconds"`
	// Tier2Jobs extends the hard-coded hot list of job names classified
	// Tier 2 by fb_agent_job_run (ADR-032).
	Tier2Jobs []string `yaml:"tier2_jobs"`
	// AllowNonLoopback lifts the loopback-only base_url rule. Not
	// recommended: the agent itself rejects non-loopback binds.
	AllowNonLoopback bool `yaml:"allow_non_loopback"`
	// ConfigPath is the agent's own agent_config.json on this host
	// (phase 11: recorded by fb_agent_register from discovery; used by the
	// candidate-file config write path). Optional.
	ConfigPath string `yaml:"config_path,omitempty"`
}

// TimeoutOrDefault fills the ADR-031 default (130s: agent restart timeout
// 120s + headroom).
func (a Agent) TimeoutOrDefault() int {
	if a.TimeoutSeconds <= 0 {
		return 130
	}
	return a.TimeoutSeconds
}

// InstanceIDOrDefault fills the agent's own default instance id.
func (a Agent) InstanceIDOrDefault() string {
	if a.InstanceID == "" {
		return "fb1"
	}
	return a.InstanceID
}

// Trends configures the C.4 capacity sampler (periodic per-DB metrics
// under <state.dir>/trends/). Enabled unless explicitly disabled; zero
// intervals/days mean defaults.
type Trends struct {
	Disabled        bool `yaml:"disabled"`
	IntervalSeconds int  `yaml:"interval_seconds"` // 0 → 300 (5 min)
	RetentionDays   int  `yaml:"retention_days"`   // 0 → 30
}

// OrDefault fills the C.4 defaults.
func (t Trends) OrDefault() Trends {
	if t.IntervalSeconds <= 0 {
		t.IntervalSeconds = 300
	}
	if t.RetentionDays <= 0 {
		t.RetentionDays = 30
	}
	return t
}

// Limits bounds remote-mode resource use per identity (phase8_plan D4.1 /
// E.1, closing the C11 residual). Zero values mean defaults.
type Limits struct {
	// MaxSessions caps concurrent in-flight authenticated requests per
	// identity (streamable POSTs and SSE message POSTs). Held GET streams
	// do not count here (phase 12 B0.7); MaxStreams caps them.
	MaxSessions int `yaml:"max_sessions"`
	// RatePerMinute is the sustained request budget per identity.
	RatePerMinute int `yaml:"rate_per_minute"`
	// RateBurst is the token-bucket burst size.
	RateBurst int `yaml:"rate_burst"`
	// MaxStreams caps held GET streams (/mcp GET, /sse) per identity.
	MaxStreams int `yaml:"max_streams"`
	// MaxLiveSessions caps live MCP sessions per identity (phase 12 B0.5).
	MaxLiveSessions int `yaml:"max_live_sessions"`
	// SessionIdleMinutes closes a session with no request for this long
	// (phase 12 B0.5). Global only: the SDK has one timeout per handler.
	SessionIdleMinutes int `yaml:"session_idle_minutes"`
}

// OrDefault fills the E.1 defaults: 8 in-flight calls, 30/min sustained,
// burst 60, 2 streams, 16 live sessions, 30 min idle timeout.
func (l Limits) OrDefault() Limits {
	if l.MaxSessions <= 0 {
		l.MaxSessions = 8
	}
	if l.RatePerMinute <= 0 {
		l.RatePerMinute = 30
	}
	if l.RateBurst <= 0 {
		l.RateBurst = 60
	}
	if l.MaxStreams <= 0 {
		l.MaxStreams = 2
	}
	if l.MaxLiveSessions <= 0 {
		l.MaxLiveSessions = 16
	}
	if l.SessionIdleMinutes <= 0 {
		l.SessionIdleMinutes = 30
	}
	return l
}

// Over returns l with every non-zero field of o applied on top.
func (l Limits) Over(o *Limits) Limits {
	if o == nil {
		return l
	}
	if o.MaxSessions > 0 {
		l.MaxSessions = o.MaxSessions
	}
	if o.RatePerMinute > 0 {
		l.RatePerMinute = o.RatePerMinute
	}
	if o.RateBurst > 0 {
		l.RateBurst = o.RateBurst
	}
	if o.MaxStreams > 0 {
		l.MaxStreams = o.MaxStreams
	}
	if o.MaxLiveSessions > 0 {
		l.MaxLiveSessions = o.MaxLiveSessions
	}
	return l
}

// Grid configures the result-set grid resources (phase10_plan.md /
// ADR-035): fb_query results additionally served as ui://fbmcp/results/{id}
// HTML resources rendered by MCP-Apps clients in the chat. The grid adds no
// authority (Tier-0 rendering of data the client already holds), so it is
// enabled by default; `enabled: false` removes the resource template and
// the _meta.ui references.
type Grid struct {
	// Enabled is a *bool so an absent key means the default (on).
	Enabled *bool `yaml:"enabled"`
	// TTLMinutes is how long a result stays fetchable (0 → 30).
	TTLMinutes int `yaml:"ttl_minutes"`
	// MaxLive caps stored results per identity, oldest evicted (0 → 32).
	MaxLive int `yaml:"max_live"`
}

// OrDefault fills the grid defaults: enabled, 30 min TTL, 32 live results.
func (g Grid) OrDefault() Grid {
	if g.Enabled == nil {
		t := true
		g.Enabled = &t
	}
	if g.TTLMinutes <= 0 {
		g.TTLMinutes = 30
	}
	if g.MaxLive <= 0 {
		g.MaxLive = 32
	}
	return g
}

// Notify is K7 delivery (ADR-024). Empty webhook = local event log only.
type Notify struct {
	WebhookURL       string `yaml:"webhook_url"`
	WebhookSecretEnv string `yaml:"webhook_secret_env"`
}

// TLS files for remote mode (ADR-022).
type TLS struct {
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
}

// APIIdentity is a remote API-key identity (P5.1).
type APIIdentity struct {
	Name   string `yaml:"name"`
	KeyEnv string `yaml:"key_env"`
	// MaxTier is the identity ceiling (phase 12 B0.1): missing → 1
	// (0 for profile autonomous), 0 → read-only, 1, 2. Tier 3 is never
	// reachable by an API key; values outside 0..2 are a config error.
	MaxTier *int `yaml:"max_tier,omitempty"`
	// Profile "autonomous" (phase 12 B8) marks an agent that runs without a
	// human: max_tier defaults to 0, in-band confirmation is off, and the
	// profile limits apply. Empty = an interactive client.
	Profile string `yaml:"profile,omitempty"`
	// InbandConfirm false sends Tier-1 requests of this identity to the
	// out-of-band queue (tray / fbmcpctl approve) and refuses its
	// fb_confirm. Missing → true; profile autonomous forces false.
	InbandConfirm *bool `yaml:"inband_confirm,omitempty"`
	// AllowFrom accepts this key only from these TCP peer addresses
	// (IP or CIDR). Empty = any address. X-Forwarded-For is not trusted.
	AllowFrom []string `yaml:"allow_from,omitempty"`
	// Limits override the global limits: block for this identity.
	Limits *Limits `yaml:"limits,omitempty"`
}

// ProfileAutonomous is the B8 identity profile for unattended agents.
const ProfileAutonomous = "autonomous"

// Tier returns a *int for APIIdentity.MaxTier literals.
func Tier(n int) *int { return &n }

// Autonomous reports profile: autonomous.
func (id APIIdentity) Autonomous() bool { return id.Profile == ProfileAutonomous }

// EffectiveMaxTier applies the B0.1 / B8 defaults.
func (id APIIdentity) EffectiveMaxTier() int {
	if id.MaxTier != nil {
		return *id.MaxTier
	}
	if id.Autonomous() {
		return 0
	}
	return 1
}

// InbandAllowed reports whether fb_confirm (in-band token / elicitation)
// may confirm this identity's Tier-1 requests.
func (id APIIdentity) InbandAllowed() bool {
	if id.Autonomous() {
		return false
	}
	return id.InbandConfirm == nil || *id.InbandConfirm
}

// autonomousLimits are the B8 profile defaults: 10/min, burst 20, 2 calls.
var autonomousLimits = Limits{MaxSessions: 2, RatePerMinute: 10, RateBurst: 20, MaxStreams: 2, MaxLiveSessions: 4}

// EffectiveLimits returns the limits for this identity: the global block
// (with defaults), then the profile defaults, then the identity override.
func (id APIIdentity) EffectiveLimits(global Limits) Limits {
	l := global.OrDefault()
	if id.Autonomous() {
		l = l.Over(&autonomousLimits)
	}
	return l.Over(id.Limits)
}

// AllowNets parses AllowFrom (validated by Config.Validate).
func (id APIIdentity) AllowNets() []*net.IPNet {
	var out []*net.IPNet
	for _, s := range id.AllowFrom {
		if n, err := ParseNet(s); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// ParseNet accepts an IP or a CIDR; a bare IP becomes a /32 or /128.
func ParseNet(s string) (*net.IPNet, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		_, n, err := net.ParseCIDR(s)
		return n, err
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return nil, fmt.Errorf("not an IP or CIDR: %q", s)
	}
	bits := 128
	if ip.To4() != nil {
		ip, bits = ip.To4(), 32
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}, nil
}

// SecretNameRe is the shape of a secret name (env var, file, keyring key).
var SecretNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// Validate checks one identity entry (also used by fbmcpctl apikey new).
func (id APIIdentity) Validate() error { return id.validate() }

func (id APIIdentity) validate() error {
	if strings.TrimSpace(id.Name) == "" {
		return fmt.Errorf("identity: name is required")
	}
	if !SecretNameRe.MatchString(id.KeyEnv) {
		return fmt.Errorf("identity %q: key_env must be a secret name like FBMCP_KEY_ALICE", id.Name)
	}
	if id.MaxTier != nil && (*id.MaxTier < 0 || *id.MaxTier > 2) {
		return fmt.Errorf("identity %q: max_tier must be 0, 1 or 2 (got %d); Tier 3 is never reachable by an API key", id.Name, *id.MaxTier)
	}
	if id.Profile != "" && id.Profile != ProfileAutonomous {
		return fmt.Errorf("identity %q: unknown profile %q (known: %s)", id.Name, id.Profile, ProfileAutonomous)
	}
	for _, a := range id.AllowFrom {
		if _, err := ParseNet(a); err != nil {
			return fmt.Errorf("identity %q: allow_from: %v", id.Name, err)
		}
	}
	if l := id.Limits; l != nil {
		if l.MaxSessions < 0 || l.RatePerMinute < 0 || l.RateBurst < 0 || l.MaxStreams < 0 || l.MaxLiveSessions < 0 {
			return fmt.Errorf("identity %q: limits must not be negative", id.Name)
		}
		if l.SessionIdleMinutes != 0 {
			return fmt.Errorf("identity %q: limits.session_idle_minutes is global only", id.Name)
		}
	}
	return nil
}

// Retention is ADR-016 keep_days (0 = keep-everything).
type Retention struct {
	KeepDays int `yaml:"keep_days"`
}

// State locates kernel state (audit log, job store, pending actions).
type State struct {
	Dir string `yaml:"dir"`
}

// FBInstance is one local Firebird server (service install) we can reach.
type FBInstance struct {
	ID                    string `yaml:"id"`
	Addr                  string `yaml:"addr"` // host:port
	BinDir                string `yaml:"bin_dir"`
	Service               string `yaml:"service"` // OS service name (P3.7); derived fallback if empty
	Version               string `yaml:"version"` // informational, e.g. "5.0"
	ServiceUser           string `yaml:"service_user"`
	ServiceSecretEnv      string `yaml:"service_secret_env"`
	DefaultROUser         string `yaml:"default_ro_user"`
	DefaultROSecretEnv    string `yaml:"default_ro_secret_env"`
	DefaultAdminUser      string `yaml:"default_admin_user"`
	DefaultAdminSecretEnv string `yaml:"default_admin_secret_env"`
	DefaultBackupDir      string `yaml:"default_backup_dir"`
	DefaultWorkDir        string `yaml:"default_work_dir"`
}

// Database is one managed database. Tool args reference it by ID only —
// never by path or connection string (main plan §4.2).
type Database struct {
	ID             string `yaml:"id"`
	Instance       string `yaml:"instance"`
	Path           string `yaml:"path"`
	BackupDir      string `yaml:"backup_dir"`
	WorkDir        string `yaml:"work_dir"`
	MigrationsDir  string `yaml:"migrations_dir"` // C.1: ordered .sql files; confine-checked at use
	ROUser         string `yaml:"ro_user"`
	ROSecretEnv    string `yaml:"ro_secret_env"` // env var name holding the RO password
	AdminUser      string `yaml:"admin_user"`
	AdminSecretEnv string `yaml:"admin_secret_env"`
}

var idRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config: invalid %s: %w", path, err)
	}
	c.SourcePath = path
	return &c, nil
}

// LocalMaxTierOrDefault returns the local-identity ceiling (default 2;
// YAML zero-value means unset, and values are clamped by identity.SetLocalMaxTier).
func (c *Config) LocalMaxTierOrDefault() int {
	if c.LocalMaxTier <= 0 {
		return 2
	}
	return c.LocalMaxTier
}

// Validate enforces structural rules; every violation is an error (fail-closed).
func (c *Config) Validate() error {
	if c.State.Dir == "" {
		return fmt.Errorf("state.dir is required (kernel state location, D8)")
	}
	if len(c.Instances) == 0 {
		return fmt.Errorf("at least one instance is required")
	}
	seenInst := map[string]bool{}
	for _, in := range c.Instances {
		if !idRe.MatchString(in.ID) {
			return fmt.Errorf("instance id %q: must match %s", in.ID, idRe)
		}
		if seenInst[in.ID] {
			return fmt.Errorf("duplicate instance id %q", in.ID)
		}
		seenInst[in.ID] = true
		if in.Addr == "" {
			return fmt.Errorf("instance %q: addr is required", in.ID)
		}
		if in.BinDir == "" {
			return fmt.Errorf("instance %q: bin_dir is required (fixed absolute utility paths, §4.1)", in.ID)
		}
	}
	seenDB := map[string]bool{}
	for _, db := range c.Databases {
		if !idRe.MatchString(db.ID) {
			return fmt.Errorf("database id %q: must match %s", db.ID, idRe)
		}
		if seenDB[db.ID] {
			return fmt.Errorf("duplicate database id %q", db.ID)
		}
		seenDB[db.ID] = true
		if !seenInst[db.Instance] {
			return fmt.Errorf("database %q: unknown instance %q", db.ID, db.Instance)
		}
		if db.Path == "" {
			return fmt.Errorf("database %q: path is required", db.ID)
		}
		if db.ROUser == "" || db.ROSecretEnv == "" {
			return fmt.Errorf("database %q: ro_user and ro_secret_env are required (read pool credentials)", db.ID)
		}
		if db.AdminUser == "" || db.AdminSecretEnv == "" {
			return fmt.Errorf("database %q: admin_user and admin_secret_env are required (admin pool credentials)", db.ID)
		}
		if strings.ContainsAny(db.Path, "$`") {
			return fmt.Errorf("database %q: suspicious characters in path", db.ID)
		}
		for _, p := range []string{db.Path, db.BackupDir, db.WorkDir} {
			if p == "" {
				continue
			}
			if strings.Contains(p, "..") {
				return fmt.Errorf("database %q: path must not contain ..", db.ID)
			}
			if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
				return fmt.Errorf("database %q: UNC paths are refused", db.ID)
			}
		}
	}
	seenIdent := map[string]bool{}
	for _, id := range c.Identities {
		if err := id.validate(); err != nil {
			return err
		}
		if seenIdent[id.Name] {
			return fmt.Errorf("duplicate identity %q", id.Name)
		}
		seenIdent[id.Name] = true
	}
	seenAgent := map[string]bool{}
	for _, ag := range c.Agents {
		if !idRe.MatchString(ag.ID) {
			return fmt.Errorf("agent id %q: must match %s", ag.ID, idRe)
		}
		if seenAgent[ag.ID] {
			return fmt.Errorf("duplicate agent id %q", ag.ID)
		}
		seenAgent[ag.ID] = true
		if ag.BaseURL == "" {
			return fmt.Errorf("agent %q: base_url is required", ag.ID)
		}
		u, err := url.Parse(ag.BaseURL)
		if err != nil {
			return fmt.Errorf("agent %q: base_url: %v", ag.ID, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("agent %q: base_url scheme must be http(s), got %q", ag.ID, u.Scheme)
		}
		if !ag.AllowNonLoopback {
			host := u.Hostname()
			loopback := host == "localhost"
			if !loopback {
				if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
					return fmt.Errorf("agent %q: base_url must be loopback (the agent API is loopback-only); use allow_non_loopback: true to override", ag.ID)
				}
			}
		}
		if ag.TokenEnv == "" {
			return fmt.Errorf("agent %q: token_env is required (the agent API denies every request without a Bearer token)", ag.ID)
		}
		for _, j := range ag.Tier2Jobs {
			if j == "" {
				return fmt.Errorf("agent %q: tier2_jobs entries must be non-empty", ag.ID)
			}
		}
	}
	return nil
}

func (in FBInstance) ValidateDiscoveryDefaults() error {
	if in.ServiceUser == "" || in.ServiceSecretEnv == "" {
		return fmt.Errorf("instance %q: service_user and service_secret_env are required for instance-scoped discovery", in.ID)
	}
	return nil
}

func (in FBInstance) ValidateRegistrationDefaults() error {
	if in.DefaultROUser == "" || in.DefaultROSecretEnv == "" {
		return fmt.Errorf("instance %q: default_ro_user and default_ro_secret_env are required for registration defaults", in.ID)
	}
	if in.DefaultAdminUser == "" || in.DefaultAdminSecretEnv == "" {
		return fmt.Errorf("instance %q: default_admin_user and default_admin_secret_env are required for registration defaults", in.ID)
	}
	return nil
}

// DB looks up a database by registry ID. Unknown IDs are rejected everywhere.
func (c *Config) DB(id string) (Database, error) {
	for _, db := range c.Databases {
		if db.ID == id {
			return db, nil
		}
	}
	return Database{}, fmt.Errorf("unknown database id %q (registry has: %v)", id, c.dbIDs())
}

// Instance looks up an instance by ID.
func (c *Config) Instance(id string) (FBInstance, error) {
	for _, in := range c.Instances {
		if in.ID == id {
			return in, nil
		}
	}
	return FBInstance{}, fmt.Errorf("unknown instance id %q", id)
}

func (c *Config) dbIDs() []string {
	ids := make([]string, len(c.Databases))
	for i, d := range c.Databases {
		ids[i] = d.ID
	}
	return ids
}

// SecretFromEnv resolves a secret exclusively from the environment (ADR-009:
// never in config files, never in argv).
// SecretFromEnv resolves a secret: environment first (ADR-009 compat),
// OS keyring entry "fbmcp/<envName>" as fallback (D5, phase8_plan D4.3).
func SecretFromEnv(envName string) (string, error) {
	return secrets.Get(envName)
}
