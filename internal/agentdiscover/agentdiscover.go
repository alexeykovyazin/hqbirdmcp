// Package agentdiscover implements phase-11 D1: local, read-only discovery
// of co-located fbagent Local API endpoints. It never performs HTTP calls
// and never reads or returns the local_api token (ADR-031: the base_url an
// MCP tool uses still comes only from fbmcp.yaml; discovery only REPORTS
// candidates, and fb_agent_register persists them through the gate).
//
// Evidence order per candidate (mirrors fbagent's own rules —
// fbagent/internal/localapi/listen.go and nodecontract.BuildFirebirdBlock):
//  1. Windows services named HQbirdFBAgent-* (registry ImagePath holds
//     `fbagent.exe --run <config path>`),
//  2. agent_config.json when readable: firebird.port, local_api
//     {listen, port, instance_id}, agent.id, storage.data_path,
//  3. the newest logs/firebird-agent_*.jsonl "local_api listening on
//     <addr> (instance_id=<id>)" line,
//  4. logs/.fbagent-instance.json,
//  5. the HKLM\SOFTWARE\IBSurgeon\HQbird2027 registry tree (secondary —
//     it covers only some installs).
//
// Address rule: local_api.listen → 127.0.0.1:(10000 + local_api.port or
// firebird.port) → 127.0.0.1:13055. Instance id: log line →
// local_api.instance_id → agent.id → .fbagent-instance.json → "fb1".
package agentdiscover

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Candidate is one discovered fbagent.
type Candidate struct {
	Service      string   `json:"service"`
	ConfigPath   string   `json:"config_path"`
	AgentDir     string   `json:"agent_dir"`
	BaseURL      string   `json:"base_url"`
	InstanceID   string   `json:"instance_id"`
	FirebirdPort int      `json:"firebird_port,omitempty"`
	Notes        []string `json:"notes,omitempty"`
}

// serviceEntry is the platform-neutral shape of one SCM registry row.
type serviceEntry struct {
	Name      string
	ImagePath string
}

// runArgRe extracts the --run argument (quoted or bare) from ImagePath.
var runArgRe = regexp.MustCompile(`--run\s+(?:"([^"]+)"|(\S+))`)

func configPathFromImage(imagePath string) string {
	m := runArgRe.FindStringSubmatch(imagePath)
	if m == nil {
		return ""
	}
	if m[1] != "" {
		return m[1]
	}
	return m[2]
}

// agentConfig is the minimal readable slice of agent_config.json. The token
// field is deliberately absent from the struct: it is never decoded.
type agentConfig struct {
	Agent struct {
		ID string `json:"id"`
	} `json:"agent"`
	Firebird struct {
		Port int `json:"port"`
	} `json:"firebird"`
	LocalAPI struct {
		Enabled    *bool  `json:"enabled"`
		Listen     string `json:"listen"`
		Port       int    `json:"port"`
		InstanceID string `json:"instance_id"`
	} `json:"local_api"`
	Storage struct {
		DataPath string `json:"data_path"`
	} `json:"storage"`
}

// effectiveListen mirrors fbagent localapi.EffectiveListen: explicit listen
// wins; else 127.0.0.1:(10000 + apiPort|fbPort); else the 13055 fallback.
func effectiveListen(listen string, apiPort, fbPort int) string {
	if strings.TrimSpace(listen) != "" {
		return strings.TrimSpace(listen)
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

// listenLogRe parses the agent's startup line:
// "local_api listening on 127.0.0.1:13055 (instance_id=honor...-p3055)"
var listenLogRe = regexp.MustCompile(`local_api listening on ([0-9A-Za-z\.\-:\[\]]+:[0-9]+)(?: \(instance_id=([^)]+)\))?`)

// instanceFile is logs/.fbagent-instance.json.
type instanceFile struct {
	AgentID     string `json:"agent_id"`
	ServiceName string `json:"service_name"`
}

// fbPortFromServiceName recovers the Firebird port from the HQbirdFBAgent-
// p<port> convention when the config is unreadable.
var servicePortRe = regexp.MustCompile(`-p(\d{3,5})$`)

// buildCandidate assembles one candidate from its service row plus local
// files. Everything is best-effort: missing or unreadable sources append a
// note instead of failing discovery.
func buildCandidate(svc serviceEntry) Candidate {
	c := Candidate{Service: svc.Name, ConfigPath: configPathFromImage(svc.ImagePath)}
	if c.ConfigPath == "" {
		c.Notes = append(c.Notes, "service ImagePath has no --run <config> argument")
	} else {
		c.AgentDir = filepath.Dir(c.ConfigPath)
	}

	var cfg agentConfig
	cfgReadable := false
	if c.ConfigPath != "" {
		if b, err := os.ReadFile(c.ConfigPath); err == nil {
			if err := json.Unmarshal(b, &cfg); err == nil {
				cfgReadable = true
			} else {
				c.Notes = append(c.Notes, "agent_config.json unparsable: "+err.Error())
			}
		} else {
			c.Notes = append(c.Notes, "agent_config.json unreadable (expected when ACL-locked): "+err.Error())
		}
	}
	if cfgReadable {
		c.FirebirdPort = cfg.Firebird.Port
		if cfg.LocalAPI.Enabled != nil && !*cfg.LocalAPI.Enabled {
			c.Notes = append(c.Notes, "local_api.enabled=false — the API will not answer")
		}
	}
	if c.FirebirdPort == 0 {
		if m := servicePortRe.FindStringSubmatch(svc.Name); m != nil {
			fmt.Sscanf(m[1], "%d", &c.FirebirdPort)
			c.Notes = append(c.Notes, "firebird port derived from the service name suffix")
		}
	}
	c.BaseURL = "http://" + effectiveListen(cfg.LocalAPI.Listen, cfg.LocalAPI.Port, c.FirebirdPort)

	// Log line is the most authoritative address + instance id source.
	addr, logInst := scanAgentLogs(logDirs(c.AgentDir, cfg.Storage.DataPath))
	if addr != "" {
		c.BaseURL = "http://" + addr
	}
	switch {
	case logInst != "":
		c.InstanceID = logInst
	case cfg.LocalAPI.InstanceID != "":
		c.InstanceID = cfg.LocalAPI.InstanceID
	case cfg.Agent.ID != "":
		c.InstanceID = cfg.Agent.ID
	default:
		if inst := readInstanceFile(logDirs(c.AgentDir, cfg.Storage.DataPath)); inst != "" {
			c.InstanceID = inst
		} else {
			c.InstanceID = "fb1"
			c.Notes = append(c.Notes, "instance_id not found in config/log — using the agent default \"fb1\"; verify after registering")
		}
	}
	if addr == "" {
		c.Notes = append(c.Notes, "no 'local_api listening' log line found — base_url derived from the 10000+port rule")
	}
	return c
}

// logDirs lists the directories that may hold the agent's jsonl logs:
// <agentDir>/logs, <data_path>, <data_path>/logs.
func logDirs(agentDir, dataPath string) []string {
	var dirs []string
	if agentDir != "" {
		dirs = append(dirs, filepath.Join(agentDir, "logs"))
	}
	if dataPath != "" {
		dirs = append(dirs, dataPath, filepath.Join(dataPath, "logs"))
	}
	return dirs
}

// scanAgentLogs finds the newest firebird-agent_*.jsonl and returns the
// last "local_api listening" address + instance id seen in its tail.
func scanAgentLogs(dirs []string) (addr, instanceID string) {
	var newest string
	var newestMod int64
	for _, d := range dirs {
		matches, _ := filepath.Glob(filepath.Join(d, "firebird-agent_*.jsonl"))
		for _, m := range matches {
			if fi, err := os.Stat(m); err == nil && fi.ModTime().UnixNano() > newestMod {
				newest, newestMod = m, fi.ModTime().UnixNano()
			}
		}
	}
	if newest == "" {
		return "", ""
	}
	b, err := readTail(newest, 256<<10)
	if err != nil {
		return "", ""
	}
	for _, m := range listenLogRe.FindAllStringSubmatch(string(b), -1) {
		addr = m[1]
		if m[2] != "" {
			instanceID = m[2]
		}
	}
	return addr, instanceID
}

func readInstanceFile(dirs []string) string {
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(d, ".fbagent-instance.json"))
		if err != nil {
			continue
		}
		var f instanceFile
		if json.Unmarshal(b, &f) == nil && f.AgentID != "" {
			return f.AgentID
		}
	}
	return ""
}

// readTail returns the final max bytes of a file (log tails are enough).
func readTail(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() > max {
		if _, err := f.Seek(fi.Size()-max, 0); err != nil {
			return nil, err
		}
	}
	buf := make([]byte, min64(fi.Size(), max))
	n, _ := f.Read(buf)
	return buf[:n], nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// discoverFromServices is the portable core over a service row list.
func discoverFromServices(entries []serviceEntry) []Candidate {
	out := make([]Candidate, 0, len(entries))
	for _, e := range entries {
		out = append(out, buildCandidate(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}
