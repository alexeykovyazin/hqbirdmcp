package config

import (
	"fmt"
	"strings"

	"github.com/aleks/fbmcp/internal/configedit"
	"gopkg.in/yaml.v3"
)

// AgentRegisterOptions carries the fb_agent_register inputs (phase 11 D2).
type AgentRegisterOptions struct {
	ID         string
	BaseURL    string
	TokenEnv   string
	InstanceID string
	ConfigPath string
}

// MaterializeAgent builds a validated Agent entry. The base_url/instance
// facts come from local discovery (internal/agentdiscover), never from a
// free-form tool argument at call time (ADR-031 SSRF stance holds: after
// registration the URL is config, and registration itself is Tier-2 gated).
func MaterializeAgent(opt AgentRegisterOptions) (Agent, error) {
	ag := Agent{
		ID:         strings.TrimSpace(opt.ID),
		BaseURL:    strings.TrimSpace(opt.BaseURL),
		TokenEnv:   strings.TrimSpace(opt.TokenEnv),
		InstanceID: strings.TrimSpace(opt.InstanceID),
		ConfigPath: strings.TrimSpace(opt.ConfigPath),
	}
	if !idRe.MatchString(ag.ID) {
		return Agent{}, fmt.Errorf("agent id %q: must match %s", ag.ID, idRe)
	}
	if ag.BaseURL == "" || ag.TokenEnv == "" {
		return Agent{}, fmt.Errorf("base_url and token_env are required")
	}
	if strings.Contains(ag.ConfigPath, "..") {
		return Agent{}, fmt.Errorf("config_path must not contain ..")
	}
	return ag, nil
}

// RegisterAgent appends one agents: entry to fbmcp.yaml (atomic write).
// Full Validate() runs on the appended document, so every ADR-031 rule
// (scheme, loopback, token_env) applies before anything reaches disk.
func RegisterAgent(cfgPath string, ag Agent) error {
	cfg, err := Load(cfgPath)
	if err != nil {
		return err
	}
	normURL := strings.TrimRight(strings.ToLower(ag.BaseURL), "/")
	for _, existing := range cfg.Agents {
		if strings.EqualFold(existing.ID, ag.ID) {
			return fmt.Errorf("agent id %q already exists", ag.ID)
		}
		if strings.TrimRight(strings.ToLower(existing.BaseURL), "/") == normURL {
			return fmt.Errorf("agent base_url %q is already registered as %q", ag.BaseURL, existing.ID)
		}
	}
	cfg.Agents = append(cfg.Agents, ag)
	if err := cfg.Validate(); err != nil {
		return err
	}
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return configedit.AtomicWrite(cfgPath, string(body))
}

// RegisterIdentity appends one identities: entry to fbmcp.yaml (atomic
// write, full Validate first). A running kernel picks it up on hot reload.
func RegisterIdentity(cfgPath string, id APIIdentity) error {
	cfg, err := Load(cfgPath)
	if err != nil {
		return err
	}
	for _, existing := range cfg.Identities {
		if existing.Name == id.Name {
			return fmt.Errorf("identity %q already exists", id.Name)
		}
		if existing.KeyEnv == id.KeyEnv {
			return fmt.Errorf("key_env %q is already used by identity %q", id.KeyEnv, existing.Name)
		}
	}
	cfg.Identities = append(cfg.Identities, id)
	if err := cfg.Validate(); err != nil {
		return err
	}
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return configedit.AtomicWrite(cfgPath, string(body))
}
