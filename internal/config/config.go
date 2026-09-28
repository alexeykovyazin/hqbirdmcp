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
	SourcePath   string    `yaml:"-"`
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
	// identity (streamable POSTs + held SSE streams).
	MaxSessions int `yaml:"max_sessions"`
	// RatePerMinute is the sustained request budget per identity.
	RatePerMinute int `yaml:"rate_per_minute"`
	// RateBurst is the token-bucket burst size.
	RateBurst int `yaml:"rate_burst"`
}

// OrDefault fills the E.1 defaults: 8 sessions, 30/min sustained, burst 60.
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
	Name    string `yaml:"name"`
	KeyEnv  string `yaml:"key_env"`
	MaxTier int    `yaml:"max_tier"`
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
