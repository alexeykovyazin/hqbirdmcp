// Package config implements the P1.1 database registry & server policy
// configuration (phase1_plan.md). YAML on disk, validated on load.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the root of fbmcp.yaml.
type Config struct {
	State     State     `yaml:"state"`
	Instances []FBInstance `yaml:"instances"`
	Databases []Database `yaml:"databases"`
}

// State locates kernel state (audit log, job store, pending actions).
type State struct {
	Dir string `yaml:"dir"`
}

// FBInstance is one local Firebird server (service install) we can reach.
type FBInstance struct {
	ID      string `yaml:"id"`
	Addr    string `yaml:"addr"` // host:port
	BinDir  string `yaml:"bin_dir"`
	Version string `yaml:"version"` // informational, e.g. "5.0"
}

// Database is one managed database. Tool args reference it by ID only —
// never by path or connection string (main plan §4.2).
type Database struct {
	ID         string `yaml:"id"`
	Instance   string `yaml:"instance"`
	Path       string `yaml:"path"`
	BackupDir  string `yaml:"backup_dir"`
	WorkDir    string `yaml:"work_dir"`
	ROUser     string `yaml:"ro_user"`
	ROSecretEnv string `yaml:"ro_secret_env"` // env var name holding the RO password
	AdminUser  string `yaml:"admin_user"`
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
	return &c, nil
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
	if len(c.Databases) == 0 {
		return fmt.Errorf("at least one database is required")
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
func SecretFromEnv(envName string) (string, error) {
	if envName == "" {
		return "", fmt.Errorf("empty secret env name")
	}
	v := os.Getenv(envName)
	if v == "" {
		return "", fmt.Errorf("secret env %s is not set", envName)
	}
	return v, nil
}
