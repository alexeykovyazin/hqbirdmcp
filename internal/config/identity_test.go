package config

import (
	"strings"
	"testing"
)

const identBase = `
state: {dir: /tmp}
instances:
  - id: i1
    addr: localhost:3050
    bin_dir: /opt/fb
databases: []
`

// Phase 12 B8: identity validation.
func TestIdentityValidation(t *testing.T) {
	bad := map[string]string{
		"no name":         "identities:\n  - key_env: K\n",
		"bad key_env":     "identities:\n  - name: a\n    key_env: K-1\n",
		"tier 3":          "identities:\n  - name: a\n    key_env: K\n    max_tier: 3\n",
		"tier -1":         "identities:\n  - name: a\n    key_env: K\n    max_tier: -1\n",
		"unknown profile": "identities:\n  - name: a\n    key_env: K\n    profile: robot\n",
		"bad allow_from":  "identities:\n  - name: a\n    key_env: K\n    allow_from: [\"10.0.0.0/33\"]\n",
		"negative limit":  "identities:\n  - name: a\n    key_env: K\n    limits: {rate_per_minute: -1}\n",
		"idle per id":     "identities:\n  - name: a\n    key_env: K\n    limits: {session_idle_minutes: 5}\n",
		"dup name":        "identities:\n  - name: a\n    key_env: K\n  - name: a\n    key_env: L\n",
	}
	for name, y := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeCfg(t, identBase+y))
			if err == nil || !strings.Contains(err.Error(), "identit") {
				t.Fatalf("%s: err=%v, want an identity error", name, err)
			}
		})
	}
	good := identBase + `identities:
  - name: op
    key_env: FBMCP_OP_KEY
    max_tier: 2
  - name: bot
    key_env: FBMCP_BOT_KEY
    profile: autonomous
    allow_from: ["192.168.1.21", "10.0.0.0/8"]
    limits: {rate_per_minute: 5}
  - name: legacy
    key_env: FBMCP_LEGACY_KEY
`
	c, err := Load(writeCfg(t, good))
	if err != nil {
		t.Fatal(err)
	}
	op, bot, legacy := c.Identities[0], c.Identities[1], c.Identities[2]
	if op.EffectiveMaxTier() != 2 || !op.InbandAllowed() {
		t.Errorf("op: tier %d inband %v", op.EffectiveMaxTier(), op.InbandAllowed())
	}
	if bot.EffectiveMaxTier() != 0 || bot.InbandAllowed() {
		t.Errorf("autonomous default: tier %d inband %v, want 0 / false", bot.EffectiveMaxTier(), bot.InbandAllowed())
	}
	if legacy.EffectiveMaxTier() != 1 {
		t.Errorf("max_tier unset must default to 1 (release note), got %d", legacy.EffectiveMaxTier())
	}
	l := bot.EffectiveLimits(c.Limits.OrDefault())
	if l.RatePerMinute != 5 || l.MaxSessions != 2 {
		t.Errorf("bot limits = %+v (own rate 5, autonomous sessions 2)", l)
	}
	if len(bot.AllowNets()) != 2 {
		t.Errorf("allow nets: %v", bot.AllowNets())
	}
}

func TestServerNameDefault(t *testing.T) {
	c := &Config{}
	if n := c.ServerNameOrDefault(); n == "" || strings.ToLower(n) != n {
		t.Fatalf("default server name %q", n)
	}
	c.ServerName = "prod-db1"
	if c.ServerNameOrDefault() != "prod-db1" {
		t.Fatal("explicit server_name ignored")
	}
}

// The shipped example must stay a valid config (F20).
func TestExampleConfigLoads(t *testing.T) {
	c, err := Load("../../packaging/fbmcp.yaml.example")
	if err != nil {
		t.Fatal(err)
	}
	in, _ := c.Instance("fb5")
	if !strings.HasSuffix(in.BinDir, "/bin") || in.ServiceUser == "" {
		t.Fatalf("example instance: bin_dir %q service_user %q", in.BinDir, in.ServiceUser)
	}
}
