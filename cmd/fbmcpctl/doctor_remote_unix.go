//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	"github.com/aleks/fbmcp/internal/config"
)

type svcInfo struct {
	name, account, cmd string
	serve              bool
	serveImplied       bool // always false: a unit without -serve exits on stdin EOF
	rwPaths            []string
	env                map[string]bool // names the unit sets (never values)
	err                string
}

// serviceEnvNames: names from the unit's Environment= and EnvironmentFile=
// (the files are read as root; an unreadable file is skipped).
func serviceEnvNames(unit string) map[string]bool {
	names := map[string]bool{}
	if env, err := systemctlShow(unit, "Environment"); err == nil {
		for k := range envNames(strings.Fields(env)) {
			names[k] = true
		}
	}
	files, _ := systemctlShow(unit, "EnvironmentFiles")
	// one "/etc/fbmcp/fbmcp.env (ignore_errors=yes)" per line
	for _, line := range strings.Split(files, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		data, err := os.ReadFile(f[0])
		if err != nil {
			continue
		}
		for k := range envNames(strings.Split(string(data), "\n")) {
			names[k] = true
		}
	}
	return names
}

func systemctlShow(unit, prop string) (string, error) {
	out, err := exec.Command("systemctl", "show", "-p", prop, "--value", unit).Output()
	return strings.TrimSpace(string(out)), err
}

// serviceInfo reads the fbmcp unit (systemctl show).
func serviceInfo(string) svcInfo {
	s := svcInfo{name: "fbmcp.service"}
	if st, err := systemctlShow(s.name, "LoadState"); err != nil || st != "loaded" {
		s.err = "unit fbmcp.service not loaded: install packaging/linux/fbmcp.service (install-remote.sh)"
		return s
	}
	s.cmd, _ = systemctlShow(s.name, "ExecStart")
	s.serve = strings.Contains(s.cmd, " -serve")
	s.account, _ = systemctlShow(s.name, "User")
	if s.account == "" {
		s.account = "root"
	}
	rw, _ := systemctlShow(s.name, "ReadWritePaths")
	s.rwPaths = strings.Fields(rw)
	s.env = serviceEnvNames(s.name)
	return s
}

// keyAccess: owner = the service user, no group/other access.
func keyAccess(path, account string) (string, string) {
	fi, err := os.Stat(path)
	if err != nil {
		return "FAIL", err.Error()
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "WARN", "cannot read the owner of " + path
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "WARN", fmt.Sprintf("key %s mode %o: group/other can read it; chmod 0600", path, fi.Mode().Perm())
	}
	if account == "root" {
		return "ok", "root can read " + path
	}
	u, err := user.Lookup(account)
	if err != nil {
		return "FAIL", "service user " + account + ": " + err.Error()
	}
	if strconv.Itoa(int(st.Uid)) != u.Uid {
		return "FAIL", fmt.Sprintf("key %s is not owned by %s (the service cannot read it): chown %s %s", path, account, account, path)
	}
	return "ok", account + " owns " + path + " (0600)"
}

// portOwner: ss -ltnp names the process (root sees every process).
func portOwner(port string) string {
	out, err := exec.Command("ss", "-H", "-ltnp", "sport = :"+port).Output()
	if err != nil {
		return ""
	}
	s := string(out)
	if i := strings.Index(s, `(("`); i >= 0 {
		rest := s[i+3:]
		if j := strings.Index(rest, `"`); j > 0 {
			return rest[:j]
		}
	}
	return ""
}

// firewallState: a ufw allow rule for the port that names a source.
func firewallState(port string) (string, string) {
	out, err := exec.Command("ufw", "status").CombinedOutput()
	if err != nil {
		return "WARN", "ufw not available (" + strings.TrimSpace(string(out)) + "): check the firewall by hand — port " + port + " must be open only to the admin network"
	}
	s := string(out)
	if strings.Contains(s, "Status: inactive") {
		return "WARN", "ufw is inactive: port " + port + " is open to every address that can route here"
	}
	var rules []string
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && (f[0] == port || strings.HasPrefix(f[0], port+"/")) && strings.Contains(line, "ALLOW") {
			if strings.HasPrefix(f[len(f)-1], "Anywhere") || strings.HasSuffix(line, "Anywhere (v6)") {
				return "FAIL", "ufw allows port " + port + " from anywhere: " + strings.Join(f, " ")
			}
			rules = append(rules, strings.Join(f, " "))
		}
	}
	if len(rules) == 0 {
		return "WARN", "no ufw allow rule for port " + port + " (ufw allow from <cidr> to any port " + port + " proto tcp)"
	}
	return "ok", strings.Join(rules, "; ")
}

// checkPlatform: ReadWritePaths exist; sudo lets the service user control
// each Firebird unit (R9).
func checkPlatform(d *doctorOut, cfg *config.Config, svc svcInfo) {
	if svc.err != "" {
		return
	}
	for _, p := range svc.rwPaths {
		opt := strings.HasPrefix(p, "-")
		p = strings.TrimPrefix(p, "-")
		if _, err := os.Stat(p); err != nil {
			if opt {
				d.warn("unit", "ReadWritePaths %s does not exist (optional)", p)
			} else {
				d.fail("unit", "ReadWritePaths %s does not exist: the unit will not start (add a - prefix or create it)", p)
			}
		}
	}
	if svc.account == "root" {
		return
	}
	out, err := exec.Command("sudo", "-n", "-l", "-U", svc.account).CombinedOutput()
	list := string(out)
	for _, in := range cfg.Instances {
		unit := in.Service
		if unit == "" {
			continue
		}
		label := "sudo:" + in.ID
		if err != nil {
			d.warn(label, "cannot list sudo rules for %s (run doctor as root): %s", svc.account, strings.TrimSpace(list))
			return
		}
		if strings.Contains(list, "/bin/systemctl") && (strings.Contains(list, unit) || strings.Contains(list, "/bin/systemctl *")) {
			d.ok(label, "%s may run systemctl for %s", svc.account, unit)
		} else {
			d.warn(label, "%s has no sudo rule for systemctl %s: fb_service_start/stop/restart will fail (packaging/posture/sudoers.fbmcp)", svc.account, unit)
		}
	}
}
