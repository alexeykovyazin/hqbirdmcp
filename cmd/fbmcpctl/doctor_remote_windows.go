//go:build windows

package main

import (
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"

	"github.com/aleks/fbmcp/internal/config"
)

type svcInfo struct {
	name, account, cmd string
	serve              bool
	serveImplied       bool            // a Windows service always serves (F4, confirmed in test W)
	env                map[string]bool // names the service sets (never values)
	err                string
}

// serviceEnvNames: names from the service's registry Environment value
// (REG_MULTI_SZ "NAME=value" lines).
func serviceEnvNames(name string) map[string]bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\`+name, registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	lines, _, err := k.GetStringsValue("Environment")
	if err != nil {
		return nil
	}
	return envNames(lines)
}

// serviceByConfig finds the service whose command line names cfgPath: one
// installed with install-remote.ps1 -Name <other> (test W: "fbmcpw").
func serviceByConfig(cfgPath string) string {
	abs, err := filepath.Abs(cfgPath)
	if cfgPath == "" || err != nil {
		return ""
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services`, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return ""
	}
	defer k.Close()
	names, _ := k.ReadSubKeyNames(-1)
	for _, n := range names {
		sk, err := registry.OpenKey(k, n, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		img, _, err := sk.GetStringValue("ImagePath")
		sk.Close()
		if err == nil && strings.Contains(strings.ToLower(img), strings.ToLower(abs)) {
			return n
		}
	}
	return ""
}

// serviceInfo reads the fbmcp service (sc qc): the service "fbmcp", else
// the one whose command line names cfgPath.
func serviceInfo(cfgPath string) svcInfo {
	s := svcInfo{name: "fbmcp", serveImplied: true}
	out, err := exec.Command("sc.exe", "qc", s.name, "4096").CombinedOutput()
	if err != nil {
		if n := serviceByConfig(cfgPath); n != "" {
			s.name = n
			out, err = exec.Command("sc.exe", "qc", n, "4096").CombinedOutput()
		}
	}
	if err != nil {
		s.err = "no fbmcp service found (sc qc fbmcp; no service command names " + cfgPath + "): install with packaging/windows/install-remote.ps1"
		return s
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "BINARY_PATH_NAME":
			s.cmd = strings.TrimSpace(v)
		case "SERVICE_START_NAME":
			s.account = strings.TrimSpace(v)
		}
	}
	s.serve = strings.Contains(s.cmd, " -serve")
	s.env = serviceEnvNames(s.name)
	return s
}

// keyAccess checks the key ACL (icacls): the service account can read it,
// and no broad group can.
func keyAccess(path, account string) (string, string) {
	out, err := exec.Command("icacls", path).CombinedOutput()
	if err != nil {
		return "FAIL", "icacls " + path + ": " + strings.TrimSpace(string(out))
	}
	acl := strings.ToLower(string(out))
	for _, broad := range []string{"everyone:", "builtin\\users:", "nt authority\\authenticated users:"} {
		if strings.Contains(acl, broad) {
			return "WARN", "key " + path + " is readable by " + strings.TrimSuffix(broad, ":") + " (fbmcpctl tls selfsign --grant sets a tight ACL)"
		}
	}
	acct := strings.ToLower(account)
	if acct == "localsystem" || strings.Contains(acl, acct+":") {
		return "ok", account + " can read " + path
	}
	return "FAIL", account + " has no entry in the key ACL: icacls \"" + path + "\" /grant \"" + account + ":R\""
}

// portOwner: the process that listens on port (netstat -ano + tasklist).
func portOwner(port string) string {
	out, err := exec.Command("netstat", "-ano", "-p", "TCP").Output()
	if err != nil {
		return ""
	}
	pid := ""
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 5 && strings.EqualFold(f[3], "LISTENING") && strings.HasSuffix(f[1], ":"+port) {
			pid = f[4]
			break
		}
	}
	if pid == "" {
		return ""
	}
	out, err = exec.Command("tasklist", "/FI", "PID eq "+pid, "/FO", "CSV", "/NH").Output()
	if err != nil {
		return "pid " + pid
	}
	name, _, _ := strings.Cut(strings.TrimSpace(string(out)), ",")
	return strings.Trim(name, `"`) + " (pid " + pid + ")"
}

// firewallState: an enabled inbound allow rule for the port with a
// RemoteAddress limit.
func firewallState(port string) (string, string) {
	ps := `$ErrorActionPreference='Stop'; Get-NetFirewallRule -Direction Inbound -Enabled True -Action Allow | ForEach-Object { $p = $_ | Get-NetFirewallPortFilter; if ($p.LocalPort -contains '` + port + `') { $a = $_ | Get-NetFirewallAddressFilter; $_.DisplayName + '|' + (($a.RemoteAddress) -join ',') } }`
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps).CombinedOutput()
	if err != nil {
		return "WARN", "cannot read firewall rules: " + strings.TrimSpace(string(out))
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) == 0 {
		return "WARN", "no inbound allow rule for port " + port + " (remote clients are blocked; install-remote.ps1 -AllowFrom adds one)"
	}
	for _, l := range lines {
		name, addrs, _ := strings.Cut(l, "|")
		if addrs == "" || strings.EqualFold(addrs, "Any") {
			return "FAIL", "rule " + name + " allows port " + port + " from any address; limit RemoteAddress to the admin network"
		}
	}
	return "ok", strings.Join(lines, "; ")
}

func checkPlatform(d *doctorOut, cfg *config.Config, svc svcInfo) {}
