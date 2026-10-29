package config

import (
	"os"
	"strings"
	"testing"
)

func TestMaterializeAgentValidation(t *testing.T) {
	ok := AgentRegisterOptions{ID: "agent_p3055", BaseURL: "http://127.0.0.1:13055", TokenEnv: "FBMCP_AGENT_TOKEN_P3055", InstanceID: "honor-p3055", ConfigPath: `C:\HQbird\fbagent-p3055\agent_config.json`}
	if _, err := MaterializeAgent(ok); err != nil {
		t.Fatal(err)
	}
	bad := []AgentRegisterOptions{
		{ID: "Agent-P3055", BaseURL: ok.BaseURL, TokenEnv: ok.TokenEnv},                                            // id regex
		{ID: "agent_p3055", BaseURL: "", TokenEnv: ok.TokenEnv},                                                    // base_url required
		{ID: "agent_p3055", BaseURL: ok.BaseURL, TokenEnv: ""},                                                     // token_env required
		{ID: "agent_p3055", BaseURL: ok.BaseURL, TokenEnv: ok.TokenEnv, ConfigPath: `C:\x\..\y\agent_config.json`}, // ..
	}
	for i, opt := range bad {
		if _, err := MaterializeAgent(opt); err == nil {
			t.Errorf("case %d: invalid options accepted: %+v", i, opt)
		}
	}
}

func TestRegisterAgentAppendsAndRejectsDuplicates(t *testing.T) {
	p := writeCfg(t, validYAML)
	ag, err := MaterializeAgent(AgentRegisterOptions{ID: "agent_p3055", BaseURL: "http://127.0.0.1:13055", TokenEnv: "FBMCP_AGENT_TOKEN_P3055"})
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterAgent(p, ag); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "id: agent_p3055") || !strings.Contains(string(b), "http://127.0.0.1:13055") {
		t.Fatalf("agent not written:\n%s", b)
	}
	// duplicate id
	if err := RegisterAgent(p, ag); err == nil {
		t.Fatal("duplicate agent id accepted")
	}
	// duplicate base_url (different id, trailing slash + case)
	ag2, _ := MaterializeAgent(AgentRegisterOptions{ID: "agent_other", BaseURL: "HTTP://127.0.0.1:13055/", TokenEnv: "X"})
	if err := RegisterAgent(p, ag2); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate base_url accepted: %v", err)
	}
}

func TestRegisterAgentValidateRejectsNonLoopback(t *testing.T) {
	p := writeCfg(t, validYAML)
	ag, err := MaterializeAgent(AgentRegisterOptions{ID: "agent_remote", BaseURL: "http://10.0.0.5:13055", TokenEnv: "X"})
	if err != nil {
		t.Fatal(err)
	}
	// ADR-031: full Validate runs on the appended document — loopback only.
	if err := RegisterAgent(p, ag); err == nil {
		t.Fatal("non-loopback base_url accepted")
	}
}
