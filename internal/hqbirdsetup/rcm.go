package hqbirdsetup

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/secrets"
)

// DefaultRCMConfig is the config file of the HQbird cluster manager
// (hqbirdrcm) that the .run kit installs on Linux.
const DefaultRCMConfig = "/opt/hqbirdrcm/rcm.json"

// RCMHost is the cluster manager found on this server.
type RCMHost struct {
	URL    string
	CAFile string
	User   string
	// Password is the first password of the RCM user (rcm-initial-password
	// in the cluster CA folder of the agent that ran --cluster-init); ""
	// when not found.
	Password string
	// PasswordFile is where Password came from (for the notes).
	PasswordFile string
}

// rcmFile is the part of rcm.json fbmcp needs.
type rcmFile struct {
	OperatorAddr string `json:"operator_addr"`
	OperatorTLS  bool   `json:"operator_tls"`
	TLS          struct {
		CACert string `json:"ca_cert"`
	} `json:"tls"`
}

// DiscoverRCM reads the RCM config (rcmConfig) and the first password of
// the RCM user from the cluster CA folders of the lines. ok is false when
// no RCM is installed here.
func DiscoverRCM(rcmConfig string, lines []Line) (RCMHost, bool, []string) {
	var notes []string
	if rcmConfig == "" {
		return RCMHost{}, false, nil
	}
	b, err := os.ReadFile(rcmConfig) // #nosec G304 -- fixed install path
	if err != nil {
		if !os.IsNotExist(err) {
			notes = append(notes, fmt.Sprintf("cluster manager: %v", err))
		}
		return RCMHost{}, false, notes
	}
	var f rcmFile
	if err := json.Unmarshal(b, &f); err != nil {
		return RCMHost{}, false, append(notes, fmt.Sprintf("cluster manager: %s: %v", rcmConfig, err))
	}
	// RCM defaults (hqcluster3 rcmsrv/config): with TLS the operator port
	// is :7444 on every address, without TLS 127.0.0.1:7444.
	port := "7444"
	if f.OperatorAddr != "" {
		if _, p, err := net.SplitHostPort(f.OperatorAddr); err == nil && p != "" {
			port = p
		}
	}
	scheme := "http"
	if f.OperatorTLS {
		scheme = "https"
	}
	h := RCMHost{URL: scheme + "://127.0.0.1:" + port, User: "rcmadmin"}
	if f.OperatorTLS {
		h.CAFile = f.TLS.CACert
		if h.CAFile == "" {
			h.CAFile = filepath.Join(filepath.Dir(rcmConfig), "certs", "ca.crt")
		}
	}
	var ia struct {
		Username string `json:"username"`
	}
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(rcmConfig), "initial-admin.json")); err == nil && json.Unmarshal(b, &ia) == nil && ia.Username != "" {
		h.User = ia.Username
	}
	var files []string
	for _, l := range lines {
		if l.AgentConfig == "" {
			continue
		}
		m, _ := filepath.Glob(filepath.Join(filepath.Dir(l.AgentConfig), "clusterca", "*", "rcm-initial-password"))
		files = append(files, m...)
	}
	sort.Strings(files)
	for _, p := range files {
		v, err := os.ReadFile(p) // #nosec G304 -- found under the agent folder
		if err == nil && strings.TrimSpace(string(v)) != "" {
			h.Password, h.PasswordFile = strings.TrimSpace(string(v)), p
			break
		}
	}
	if len(files) > 1 {
		notes = append(notes, fmt.Sprintf("cluster manager: %d first-password files; used %s", len(files), h.PasswordFile))
	}
	return h, true, notes
}

// ApplyRCM adds the rcm: section of the cluster manager found here (or
// removes the section this command added when the RCM is gone). The
// operator's own rcm: section is never changed. The password secret is
// written only when it does not exist yet: fb_cluster_rcm_password and the
// operator change it later.
func (pl *Plan) ApplyRCM(m Managed, h RCMHost, found bool, haveSecret func(string) bool) {
	cfg := pl.Config
	if cfg.RCM.On() && !m.RCM {
		return // the operator's entry
	}
	if !found {
		if m.RCM {
			cfg.RCM = config.RCM{}
			pl.Changes = append(pl.Changes, "remove rcm (the cluster manager is gone)")
		}
		return
	}
	want := config.RCM{URL: h.URL, CAFile: h.CAFile, User: h.User,
		PasswordEnv: cfg.RCM.PasswordEnv, TimeoutSeconds: cfg.RCM.TimeoutSeconds}
	if want.User == "rcmadmin" {
		want.User = ""
	}
	if cfg.RCM != want {
		verb := "add"
		if cfg.RCM.On() {
			verb = "update"
		}
		cfg.RCM = want
		pl.Changes = append(pl.Changes, verb+" rcm "+h.URL)
	}
	pl.Managed.RCM = true
	env := cfg.RCM.PasswordEnvOrDefault()
	switch {
	case haveSecret(env):
	case h.Password != "":
		pl.Secrets[env] = h.Password
	default:
		pl.Notes = append(pl.Notes, "cluster manager: no first password found; store the RCM password: fbmcp ctl secret set "+env)
	}
	if !contains(pl.Managed.Secrets, env) && (haveSecret(env) || h.Password != "") && secrets.ValidName(env) {
		pl.Managed.Secrets = append(pl.Managed.Secrets, env)
		sort.Strings(pl.Managed.Secrets)
	}
}
