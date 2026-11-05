//go:build windows

package ctl

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/aleks/fbmcp/internal/hqbirdsetup"
)

// winHqbirdHost is the Windows host of hqbird-install: the service runs as
// the virtual account NT SERVICE\fbmcp (install-remote.ps1 steps).
type winHqbirdHost struct {
	name    string
	changed bool // EnsureService changed the definition: a running service restarts
}

func newHqbirdHost() hqbirdHost { return &winHqbirdHost{name: hqbirdsetup.ServiceName} }

func (h *winHqbirdHost) Account() string { return `NT SERVICE\` + h.name }
func (h *winHqbirdHost) sid() string     { return serviceSID(h.name) }

// FirebirdService finds the service whose ImagePath runs firebird.exe (else
// fbguard.exe) from installPath.
func (h *winHqbirdHost) FirebirdService(installPath string) string {
	root := strings.ToLower(strings.TrimRight(filepath.Clean(installPath), `\`)) + `\`
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services`, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return ""
	}
	defer k.Close()
	names, _ := k.ReadSubKeyNames(-1)
	guard := ""
	for _, n := range names {
		sk, err := registry.OpenKey(k, n, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		img, _, err := sk.GetStringValue("ImagePath")
		sk.Close()
		img = strings.ToLower(strings.Trim(img, `"`))
		if err != nil || !strings.Contains(img, root) {
			continue
		}
		switch {
		case strings.Contains(img, `\firebird.exe`), strings.Contains(img, `\fbserver.exe`):
			return n
		case strings.Contains(img, `\fbguard.exe`):
			guard = n
		}
	}
	return guard
}

// Access masks for the folder rules.
const (
	accessModify  = "0x1301bf" // read, write, execute, delete
	accessRead    = "FR"
	accessReadExe = "0x1200a9"
)

// PrepareFolders: SYSTEM and Administrators full; the service modifies the
// data and state folders (fb_db_register writes fbmcp.yaml) and only reads
// secrets and TLS. Nothing is inherited: HQbird folders give Authenticated
// Users Modify (fbmcp docs/findings/phase12-lab-2026-11-03.md).
func (h *winHqbirdHost) PrepareFolders(p hqbirdsetup.Paths) error {
	dirs := []struct{ path, access string }{
		{filepath.Dir(p.Config), accessModify},
		{p.StateDir, accessModify},
		{p.SecretsDir, accessRead},
		{p.TLSDir, accessRead},
		// backups and work files of the registered databases (the
		// Firebird service runs as LocalSystem: SY has full access)
		{p.BackupRoot, accessModify},
		{p.WorkRoot, accessModify},
	}
	for _, d := range dirs {
		if d.path == "" {
			continue
		}
		if err := os.MkdirAll(d.path, 0o750); err != nil {
			return err
		}
		if err := setDACL(d.path, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;"+d.access+";;;"+h.sid()+")"); err != nil {
			return fmt.Errorf("%s: %v", d.path, err)
		}
	}
	return nil
}

// AfterWrite: the files take the folder rules.
func (h *winHqbirdHost) AfterWrite(...string) error { return nil }

func setDACL(path, sddl string) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

// EnsureService creates or updates the service: auto start, restart on
// failure, unrestricted service SID. The program folder gets its rules
// when it is a folder of its own (…\fbmcp).
func (h *winHqbirdHost) EnsureService(exe, cfgPath string) error {
	if strings.EqualFold(filepath.Base(filepath.Dir(exe)), "fbmcp") {
		sddl := "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;" + accessReadExe + ";;;BU)(A;OICI;" + accessReadExe + ";;;" + h.sid() + ")"
		if err := setDACL(filepath.Dir(exe), sddl); err != nil {
			return fmt.Errorf("program folder %s: %v", filepath.Dir(exe), err)
		}
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	binPath := fmt.Sprintf(`"%s" -config "%s" -serve`, exe, cfgPath)
	s, err := m.OpenService(h.name)
	if err != nil {
		s, err = m.CreateService(h.name, exe, mgr.Config{
			DisplayName: "fbmcp", Description: "fbmcp - Firebird DBA MCP server (HQbird)",
			StartType: mgr.StartAutomatic, ServiceStartName: h.Account(), SidType: windows.SERVICE_SID_TYPE_UNRESTRICTED,
		}, "-config", cfgPath, "-serve")
		if err != nil {
			return err
		}
	} else {
		c, err := s.Config()
		if err != nil {
			s.Close()
			return err
		}
		h.changed = !strings.EqualFold(c.BinaryPathName, binPath) || !strings.EqualFold(c.ServiceStartName, h.Account())
		c.BinaryPathName, c.StartType, c.ServiceStartName, c.SidType = binPath, mgr.StartAutomatic, h.Account(), windows.SERVICE_SID_TYPE_UNRESTRICTED
		c.Description = "fbmcp - Firebird DBA MCP server (HQbird)"
		if err := s.UpdateConfig(c); err != nil {
			s.Close()
			return err
		}
	}
	defer s.Close()
	return s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, 86400)
}

// GrantFirebird adds start/stop/query rights for the service SID to each
// Firebird service (install-remote.ps1 step 4).
func (h *winHqbirdHost) GrantFirebird(services []string) []string {
	var warns []string
	for _, name := range services {
		if err := grantServiceControl(name, h.sid()); err != nil {
			warns = append(warns, fmt.Sprintf("rights on Firebird service %s: %v (fb_service_* will be refused by Windows)", name, err))
		}
	}
	return warns
}

func grantServiceControl(name, sid string) error {
	sd, err := windows.GetNamedSecurityInfo(name, windows.SE_SERVICE, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	cur := sd.String()
	if strings.Contains(cur, ";;;"+sid+")") {
		return nil
	}
	nsd, err := windows.SecurityDescriptorFromString(cur + "(A;;CCLCRPWPLORC;;;" + sid + ")")
	if err != nil {
		return err
	}
	dacl, _, err := nsd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(name, windows.SE_SERVICE, windows.DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

// GrantAgentConfigs: read on agent_config.json and add-file on its folder
// (the phase 11 candidate-file write), as install-remote.ps1 step 5.
func (h *winHqbirdHost) GrantAgentConfigs(paths []string) []string {
	var warns []string
	for _, p := range paths {
		for _, a := range [][]string{{p, "/grant", "*" + h.sid() + ":R"}, {filepath.Dir(p), "/grant", "*" + h.sid() + ":(AD,WD)"}} {
			if out, err := exec.Command("icacls.exe", a...).CombinedOutput(); err != nil {
				warns = append(warns, fmt.Sprintf("icacls %s: %s", a[0], strings.TrimSpace(string(out))))
			}
		}
	}
	return warns
}

func (h *winHqbirdHost) GrantKeyRead(keyPath string) error { return grantKeyRead(keyPath, h.Account()) }

const firewallRule = "fbmcp-remote"

// Firewall replaces the rule fbmcp-remote: inbound TCP port, only from allow.
func (h *winHqbirdHost) Firewall(port int, allow []string, exe string, m *hqbirdsetup.Managed) error {
	_ = exec.Command("netsh", "advfirewall", "firewall", "delete", "rule", "name="+firewallRule).Run()
	out, err := exec.Command("netsh", "advfirewall", "firewall", "add", "rule", "name="+firewallRule, "dir=in", "action=allow",
		"protocol=TCP", fmt.Sprintf("localport=%d", port), "remoteip="+strings.Join(allow, ","), "program="+exe, "profile=any").CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh: %s", strings.TrimSpace(string(out)))
	}
	m.Firewall = []string{firewallRule}
	// A rule of a switched-off firewall limits nothing.
	off, err := firewallProfilesOff()
	if err != nil {
		return fmt.Errorf("cannot read the Windows Firewall state: %v", err)
	}
	if off != "" {
		return fmt.Errorf("Windows Firewall is off for the profile(s) %s: the rule limits nothing", off)
	}
	return nil
}

// firewallProfilesOff lists the Windows Firewall profiles that are off
// (netsh prints the state in the language of Windows; this does not).
func firewallProfilesOff() (string, error) {
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		"(Get-NetFirewallProfile | Where-Object { $_.Enabled -eq 'False' } | ForEach-Object { $_.Name }) -join ','").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (h *winHqbirdHost) RemoveFirewall(m *hqbirdsetup.Managed) error {
	if len(m.Firewall) == 0 {
		return nil
	}
	_ = exec.Command("netsh", "advfirewall", "firewall", "delete", "rule", "name="+firewallRule).Run()
	m.Firewall = nil
	return nil
}

func (h *winHqbirdHost) withService(f func(s *mgr.Service) error) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(h.name)
	if err != nil {
		return err
	}
	defer s.Close()
	return f(s)
}

func (h *winHqbirdHost) State() string {
	state := "absent"
	_ = h.withService(func(s *mgr.Service) error {
		st, err := s.Query()
		if err != nil {
			return err
		}
		state = "stopped"
		if st.State == svc.Running {
			state = "running"
		}
		return nil
	})
	return state
}

func (h *winHqbirdHost) Start() error {
	return h.withService(func(s *mgr.Service) error {
		st, err := s.Query()
		if err != nil {
			return err
		}
		if st.State == svc.Running {
			if !h.changed {
				return nil
			}
			if _, err := s.Control(svc.Stop); err != nil {
				return err
			}
			if err := waitState(s, svc.Stopped, 40*time.Second); err != nil {
				return err
			}
		}
		h.changed = false
		if err := s.Start(); err != nil {
			return err
		}
		return waitState(s, svc.Running, 30*time.Second)
	})
}

// StopDisable stops the service and sets it to disabled; EnsureService
// turns it back to automatic.
func (h *winHqbirdHost) StopDisable() error {
	return h.withService(func(s *mgr.Service) error {
		if st, err := s.Query(); err == nil && st.State != svc.Stopped {
			if _, err := s.Control(svc.Stop); err != nil {
				return err
			}
			if err := waitState(s, svc.Stopped, 40*time.Second); err != nil {
				return err
			}
		}
		c, err := s.Config()
		if err != nil {
			return err
		}
		c.StartType = mgr.StartDisabled
		return s.UpdateConfig(c)
	})
}

func waitState(s *mgr.Service, want svc.State, max time.Duration) error {
	deadline := time.Now().Add(max)
	for {
		st, err := s.Query()
		if err != nil {
			return err
		}
		if st.State == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("service state %d after %s", st.State, max)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
