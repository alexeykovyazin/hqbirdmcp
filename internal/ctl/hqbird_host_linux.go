//go:build linux

package ctl

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/hqbirdsetup"
)

// linuxHqbirdHost is the Linux host of hqbird-install: fbmcp.service runs
// as the Firebird user (install-remote.sh steps).
type linuxHqbirdHost struct {
	user     string
	unitDir  string
	sudoers  string
	links    string
	unitDirs []string // where FirebirdService looks for units
	changed  bool     // a unit file changed: a running service restarts
}

func newHqbirdHost() hqbirdHost {
	return &linuxHqbirdHost{
		user: "firebird", unitDir: "/etc/systemd/system", sudoers: "/etc/sudoers.d/fbmcp", links: "/usr/local/bin",
		unitDirs: []string{"/etc/systemd/system", "/lib/systemd/system", "/usr/lib/systemd/system"},
	}
}

func (h *linuxHqbirdHost) Account() string { return h.user }

func (h *linuxHqbirdHost) ids() (int, int, error) {
	u, err := user.Lookup(h.user)
	if err != nil {
		return 0, 0, fmt.Errorf("user %s: %v (install Firebird first)", h.user, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return uid, gid, nil
}

// FirebirdService finds the unit whose ExecStart runs a program from
// installPath (firebird.opt_fb50 and the like).
func (h *linuxHqbirdHost) FirebirdService(installPath string) string {
	root := strings.TrimRight(filepath.Clean(installPath), "/") + "/"
	for _, dir := range h.unitDirs {
		files, _ := filepath.Glob(filepath.Join(dir, "firebird*.service"))
		sort.Strings(files)
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			for _, ln := range strings.Split(string(b), "\n") {
				ln = strings.TrimSpace(ln)
				if strings.HasPrefix(ln, "ExecStart=") && strings.Contains(ln, root) {
					return strings.TrimSuffix(filepath.Base(f), ".service")
				}
			}
		}
	}
	return ""
}

// PrepareFolders: the config folder is root:firebird 0770 (fb_db_register
// writes fbmcp.yaml), state and secrets are the Firebird user's (0700),
// TLS is root:firebird 0750 with the key owned by the Firebird user.
func (h *linuxHqbirdHost) PrepareFolders(p hqbirdsetup.Paths) error {
	uid, gid, err := h.ids()
	if err != nil {
		return err
	}
	dirs := []struct {
		path     string
		uid, gid int
		mode     os.FileMode
	}{
		{filepath.Dir(p.Config), 0, gid, 0o770},
		{p.StateDir, uid, gid, 0o700},
		{p.SecretsDir, uid, gid, 0o700},
		{p.TLSDir, 0, gid, 0o750},
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return err
		}
		if err := os.Chown(d.path, d.uid, d.gid); err != nil {
			return err
		}
		if err := os.Chmod(d.path, d.mode); err != nil {
			return err
		}
	}
	return nil
}

// AfterWrite gives written files to the Firebird user (mode 0640).
func (h *linuxHqbirdHost) AfterWrite(paths ...string) error {
	uid, gid, err := h.ids()
	if err != nil {
		return err
	}
	for _, p := range paths {
		if err := os.Chown(p, uid, gid); err != nil {
			return err
		}
		if err := os.Chmod(p, 0o640); err != nil {
			return err
		}
	}
	return nil
}

// stateDirOf reads state.dir from the config written just before.
func stateDirOf(cfgPath string) string {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return ""
	}
	return cfg.State.Dir
}

func fileOwner(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

func (h *linuxHqbirdHost) writeIfChanged(path, body string, mode os.FileMode) (bool, error) {
	if cur, err := os.ReadFile(path); err == nil && string(cur) == body {
		return false, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), mode); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

// EnsureService writes fbmcp.service, the links in /usr/local/bin and
// enables the unit.
func (h *linuxHqbirdHost) EnsureService(exe, cfgPath string) error {
	if _, _, err := h.ids(); err != nil {
		return err
	}
	cfgDir := filepath.Dir(cfgPath)
	// /opt/hqbird holds the HQbird 2027 agent folders (fbagent-<line>),
	// /opt/hqbird-fbagent the older single agent: fb_agent_config_set writes
	// its candidate file next to agent_config.json.
	rw := []string{"/var/lib/fbmcp", "/var/lib/fbmcp-backups", "/var/lib/fbmcp-work", cfgDir, "/opt/hqbird", "/opt/hqbird-fbagent"}
	if sd := stateDirOf(cfgPath); sd != "" && !containsFold(rw, sd) {
		rw = append([]string{sd}, rw...)
	}
	unit := renderUnit(unitData{User: h.user, Exe: exe, Config: cfgPath, ConfigDir: cfgDir, RW: rw})
	changed, err := h.writeIfChanged(filepath.Join(h.unitDir, hqbirdsetup.ServiceName+".service"), unit, 0o644)
	if err != nil {
		return err
	}
	h.changed = h.changed || changed
	for _, name := range []string{"fbmcp", "fbmcpctl"} {
		link := filepath.Join(h.links, name)
		if cur, err := os.Readlink(link); err == nil && cur == exe {
			continue
		}
		_ = os.Remove(link)
		if err := os.Symlink(exe, link); err != nil {
			return fmt.Errorf("link %s: %v", link, err)
		}
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	return systemctl("enable", hqbirdsetup.ServiceName)
}

// GrantFirebird writes /etc/sudoers.d/fbmcp: start, stop and restart of
// each Firebird unit, exactly as the kernel calls them (R9).
func (h *linuxHqbirdHost) GrantFirebird(services []string) []string {
	if len(services) == 0 {
		return nil
	}
	units := append([]string{}, services...)
	sort.Strings(units)
	var cmds []string
	for _, u := range units {
		for _, verb := range []string{"start", "stop", "restart"} {
			cmds = append(cmds, "/bin/systemctl "+verb+" "+u)
		}
	}
	body := "# Written by \"fbmcp ctl hqbird-install\": fb_service_* may start, stop and\n" +
		"# restart these Firebird units. Run hqbird-install again instead of editing.\n" +
		h.user + " ALL=(root) NOPASSWD: " + strings.Join(cmds, ", ") + "\n"
	if cur, err := os.ReadFile(h.sudoers); err == nil && string(cur) == body {
		return nil
	}
	tmp := h.sudoers + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o440); err != nil {
		return []string{fmt.Sprintf("sudoers: %v", err)}
	}
	if out, err := exec.Command("visudo", "-cf", tmp).CombinedOutput(); err != nil {
		os.Remove(tmp)
		return []string{fmt.Sprintf("sudoers refused by visudo: %s", strings.TrimSpace(string(out)))}
	}
	if err := os.Rename(tmp, h.sudoers); err != nil {
		return []string{fmt.Sprintf("sudoers: %v", err)}
	}
	return nil
}

// GrantAgentConfigs: fbagent writes agent_config.json 0600 as the Firebird
// user, the user fbmcp runs as. A config of another owner gets a warning.
func (h *linuxHqbirdHost) GrantAgentConfigs(paths []string) []string {
	uid, _, err := h.ids()
	if err != nil {
		return []string{err.Error()}
	}
	var warns []string
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			warns = append(warns, err.Error())
			continue
		}
		if st, ok := fileOwner(fi); ok && st != uid {
			warns = append(warns, fmt.Sprintf("%s is not owned by %s: fbmcp may not read it", p, h.user))
		}
	}
	return warns
}

func (h *linuxHqbirdHost) GrantKeyRead(keyPath string) error { return grantKeyRead(keyPath, h.user) }

// Firewall opens port for allow with firewalld (rich rules) or ufw; the
// rules added are recorded in m so they can be removed later. Without
// either tool it is a warning: the operator opens the port.
func (h *linuxHqbirdHost) Firewall(port int, allow []string, _ string, m *hqbirdsetup.Managed) error {
	_ = h.RemoveFirewall(m)
	var added []string
	switch {
	case hasCmd("firewall-cmd") && exec.Command("firewall-cmd", "--state").Run() == nil:
		for _, c := range allow {
			fam := "ipv4"
			if strings.Contains(c, ":") {
				fam = "ipv6"
			}
			rule := fmt.Sprintf(`rule family="%s" source address="%s" port port="%d" protocol="tcp" accept`, fam, c, port)
			if out, err := exec.Command("firewall-cmd", "--permanent", "--add-rich-rule="+rule).CombinedOutput(); err != nil {
				m.Firewall = added
				return fmt.Errorf("firewall-cmd: %s", strings.TrimSpace(string(out)))
			}
			added = append(added, "firewalld:"+rule)
		}
		_ = exec.Command("firewall-cmd", "--reload").Run()
	case hasCmd("ufw"):
		for _, c := range allow {
			args := []string{"from", c, "to", "any", "port", strconv.Itoa(port), "proto", "tcp"}
			if out, err := exec.Command("ufw", append([]string{"allow"}, args...)...).CombinedOutput(); err != nil {
				m.Firewall = added
				return fmt.Errorf("ufw: %s", strings.TrimSpace(string(out)))
			}
			added = append(added, "ufw:"+strings.Join(args, " "))
		}
		if out, _ := exec.Command("ufw", "status").Output(); bytes.Contains(out, []byte("inactive")) {
			m.Firewall = added
			return fmt.Errorf("ufw is inactive: the rules apply after 'ufw enable' (allow ssh first)")
		}
	default:
		return fmt.Errorf("no firewalld or ufw: open TCP %d only from %s with your firewall tool", port, strings.Join(allow, ", "))
	}
	m.Firewall = added
	return nil
}

func (h *linuxHqbirdHost) RemoveFirewall(m *hqbirdsetup.Managed) error {
	reload := false
	for _, r := range m.Firewall {
		kind, rule, _ := strings.Cut(r, ":")
		switch kind {
		case "firewalld":
			_ = exec.Command("firewall-cmd", "--permanent", "--remove-rich-rule="+rule).Run()
			reload = true
		case "ufw":
			_ = exec.Command("ufw", append([]string{"delete", "allow"}, strings.Fields(rule)...)...).Run()
		}
	}
	if reload {
		_ = exec.Command("firewall-cmd", "--reload").Run()
	}
	m.Firewall = nil
	return nil
}

func (h *linuxHqbirdHost) State() string {
	if _, err := os.Stat(filepath.Join(h.unitDir, hqbirdsetup.ServiceName+".service")); err != nil {
		return "absent"
	}
	out, _ := exec.Command("systemctl", "is-active", hqbirdsetup.ServiceName).Output()
	switch strings.TrimSpace(string(out)) {
	case "active", "activating", "reloading":
		return "running"
	case "failed":
		return "failed"
	}
	return "stopped"
}

// Start starts the service, or restarts it when its unit changed.
func (h *linuxHqbirdHost) Start() error {
	verb := "start"
	if h.changed && h.State() == "running" {
		verb = "restart"
	} else if h.State() == "running" {
		return nil
	}
	h.changed = false
	return systemctl(verb, hqbirdsetup.ServiceName)
}

func (h *linuxHqbirdHost) StopDisable() error {
	return systemctl("disable", "--now", hqbirdsetup.ServiceName)
}

func systemctl(args ...string) error {
	if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return nil
}

func hasCmd(name string) bool { _, err := exec.LookPath(name); return err == nil }
