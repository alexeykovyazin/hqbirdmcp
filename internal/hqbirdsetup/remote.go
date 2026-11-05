package hqbirdsetup

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/aleks/fbmcp/internal/config"
)

// PlaceholderOrigin fills allowed_origins: remote mode refuses an empty
// list, and no browser client uses fbmcp (remote.md §3).
const PlaceholderOrigin = "https://fbmcp.invalid"

// RemoteState is what ApplyRemote decided.
type RemoteState struct {
	// Listen is the address set ("" = remote mode is off).
	Listen string
	// Pending: remote mode is asked for, but no API key exists yet, and the
	// kernel refuses remote mode without one (ADR-022).
	Pending bool
	Changes []string
}

// ApplyRemote sets listen, tls and allowed_origins for the remote mode in
// r. A config with no remote mode asked for is not changed: a listen the
// operator set stays. ip is the address to bind (--ip, else the first
// address of the server).
func ApplyRemote(cfg *config.Config, r Remote, ip string, p Paths) RemoteState {
	var st RemoteState
	if !r.On() {
		st.Listen = strings.TrimSpace(cfg.Listen)
		return st
	}
	port := r.Port
	if port <= 0 {
		if _, ps, err := net.SplitHostPort(cfg.Listen); err == nil {
			port, _ = strconv.Atoi(ps)
		}
	}
	if port <= 0 {
		port = DefaultPort
	}
	if cfg.TLS.Cert == "" || cfg.TLS.Key == "" {
		cfg.TLS.Cert, cfg.TLS.Key = TLSCert(p), TLSKey(p)
		st.Changes = append(st.Changes, "set tls "+cfg.TLS.Cert)
	}
	if len(cfg.AllowedOrigins) == 0 {
		cfg.AllowedOrigins = []string{PlaceholderOrigin}
		st.Changes = append(st.Changes, "set allowed_origins")
	}
	if len(cfg.Identities) == 0 {
		st.Pending = true
		return st
	}
	listen := net.JoinHostPort(ip, strconv.Itoa(port))
	if strings.TrimSpace(cfg.Listen) != listen {
		cfg.Listen = listen
		st.Changes = append(st.Changes, "set listen "+listen)
	}
	st.Listen = listen
	return st
}

// TLSCert and TLSKey are the self-signed pair of the HQbird layout.
func TLSCert(p Paths) string { return joinLike(p.TLSDir, "tls.crt") }
func TLSKey(p Paths) string  { return joinLike(p.TLSDir, "tls.key") }

// joinLike joins with the separator the folder already uses.
func joinLike(dir, name string) string {
	if strings.Contains(dir, `\`) {
		return strings.TrimRight(dir, `\`) + `\` + name
	}
	return strings.TrimRight(dir, "/") + "/" + name
}

// URL is the MCP endpoint of a listen address; host names it when set.
func URL(listen, host string) string {
	h, port, err := net.SplitHostPort(listen)
	if err != nil {
		return ""
	}
	if host != "" {
		h = host
	}
	return "https://" + net.JoinHostPort(h, port) + "/mcp"
}

// ConnectInfo is the input of connect.txt.
type ConnectInfo struct {
	Server    string
	URL       string
	Pin       string
	AllowFrom []string
	Pending   bool
	Config    string
	StateDir  string
	Windows   bool
}

// RenderConnect writes connect.txt: how a client on an admin PC connects.
// It never holds a key value.
func RenderConnect(ci ConnectInfo) string {
	var b strings.Builder
	// adm runs the commands that change fbmcp.yaml: root or an
	// Administrator prompt, never the service account.
	ctl, adm := "fbmcp ctl", "sudo fbmcp ctl"
	if ci.Windows {
		ctl, adm = "fbmcp.exe ctl", "fbmcp.exe ctl"
	}
	name := "fb-" + strings.ToLower(ci.Server)
	keyEnv := "FBMCP_KEY_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(ci.Server))
	fmt.Fprintf(&b, "fbmcp on %s\n\n", ci.Server)
	switch {
	case ci.URL == "" && !ci.Pending:
		fmt.Fprintf(&b, "Remote mode is off. Turn it on (Administrator/root), with the admin addresses:\n")
		fmt.Fprintf(&b, "  %s hqbird-install --allow-from <CIDR>\n", adm)
		fmt.Fprintf(&b, "Local clients on this server: %s (stdio) with -config %s\n", "fbmcp", ci.Config)
		return b.String()
	case ci.Pending:
		fmt.Fprintf(&b, "Remote mode is ready but off: it needs one API key first.\n")
		fmt.Fprintf(&b, "  1. %s apikey new <name> --max-tier 2 --config \"%s\"\n", adm, ci.Config)
		fmt.Fprintf(&b, "  2. %s hqbird-install\n\n", adm)
		return b.String()
	}
	fmt.Fprintf(&b, "URL:  %s\nPin:  %s\nOpen to: %s\n\n", ci.URL, ci.Pin, strings.Join(ci.AllowFrom, ", "))
	fmt.Fprintf(&b, "1. Personal key (on this server, Administrator/root):\n   %s apikey new <name> --max-tier 2 --config \"%s\"\n", adm, ci.Config)
	fmt.Fprintf(&b, `2. On the admin PC: copy fbmcp.exe to C:\fbmcp\ and store the key:`+"\n"+`   C:\fbmcp\fbmcp.exe ctl secret set %s`+"\n", keyEnv)
	fmt.Fprintf(&b, "3. Claude Desktop, Cursor, Codex (claude_desktop_config.json):\n")
	fmt.Fprintf(&b, `   {"mcpServers": {%q: {"command": "C:\\fbmcp\\fbmcp.exe",`+"\n", name)
	fmt.Fprintf(&b, "     \"args\": [\"bridge\", \"--url\", %q, \"--key-env\", %q, \"--pin\", %q]}}}\n", ci.URL, keyEnv, ci.Pin)
	fmt.Fprintf(&b, "4. Claude Code (.mcp.json; the key in a user env var, the server certificate in NODE_EXTRA_CA_CERTS):\n")
	fmt.Fprintf(&b, "   {\"mcpServers\":{%q:{\"type\":\"http\",\"url\":%q,\"headers\":{\"Authorization\":\"Bearer ${%s}\"}}}}\n", name, ci.URL, keyEnv)
	fmt.Fprintf(&b, "\nTier 2 approval (on this server): %s approve %s <id>\n", ctl, ci.StateDir)
	return b.String()
}
