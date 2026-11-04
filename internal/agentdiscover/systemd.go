package agentdiscover

// Linux enumeration (phase 12 B9, F12): systemd units whose ExecStart runs
// an fbagent binary with --run <config>. The parsers are portable so the
// fixture tests run on every OS; scm_linux.go runs systemctl/journalctl.

import (
	"path"
	"regexp"
	"strings"
)

// cmdRunner runs a command and returns stdout (tests replace it).
type cmdRunner func(name string, args ...string) ([]byte, error)

// unitPatterns: the HQbird unit name plus any unit with fbagent in it; the
// ExecStart basename check below decides.
var unitPatterns = []string{"hqbirdfbagent*", "*fbagent*"}

// parseUnitNames takes the first column of `systemctl list-units --plain
// --no-legend` or `systemctl list-unit-files --no-legend` output.
func parseUnitNames(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasSuffix(f[0], ".service") {
			continue
		}
		names = append(names, f[0])
	}
	return names
}

var (
	execPathRe = regexp.MustCompile(`path=([^ ;]+)`)
	execArgvRe = regexp.MustCompile(`argv\[\]=(.*?)\s;\s`)
)

// parseExecStart reads `systemctl show -p ExecStart --value` output:
// "{ path=/opt/hqbird-fbagent/fbagent ; argv[]=/opt/hqbird-fbagent/fbagent --run /opt/hqbird-fbagent/agent_config.json ; ... }".
// It returns the binary path and the argv line.
func parseExecStart(out string) (bin, argv string) {
	if m := execPathRe.FindStringSubmatch(out); m != nil {
		bin = m[1]
	}
	if m := execArgvRe.FindStringSubmatch(out); m != nil {
		argv = strings.TrimSpace(m[1])
	}
	return bin, argv
}

// isFbagentBinary matches the executable basename, not the unit name.
func isFbagentBinary(bin string) bool {
	b := strings.ToLower(path.Base(bin))
	return b == "fbagent" || b == "fbagent.exe"
}

// listUnits returns one serviceEntry per unit that runs fbagent. The entry
// name is the unit without ".service"; ImagePath is the argv line, so the
// shared --run parser applies.
func listUnits(run cmdRunner) ([]serviceEntry, error) {
	seen := map[string]bool{}
	var units []string
	for _, args := range [][]string{
		append([]string{"list-units", "--type=service", "--all", "--plain", "--no-legend"}, unitPatterns...),
		append([]string{"list-unit-files", "--type=service", "--no-legend"}, unitPatterns...),
	} {
		out, err := run("systemctl", args...)
		if err != nil && len(out) == 0 {
			continue
		}
		for _, u := range parseUnitNames(string(out)) {
			if !seen[u] {
				seen[u] = true
				units = append(units, u)
			}
		}
	}
	var entries []serviceEntry
	for _, u := range units {
		out, err := run("systemctl", "show", "-p", "ExecStart", "--value", u)
		if err != nil {
			continue
		}
		bin, argv := parseExecStart(string(out))
		if !isFbagentBinary(bin) {
			continue
		}
		entries = append(entries, serviceEntry{Name: strings.TrimSuffix(u, ".service"), ImagePath: argv})
	}
	return entries, nil
}

// journalListen finds the newest "local_api listening" line of a unit in
// the journal (Linux fbagent logs there, not to jsonl files). The service
// user needs journal access (group systemd-journal) for this fallback.
func journalListen(run cmdRunner, unit string) (addr, instanceID string) {
	out, _ := run("journalctl", "-u", unit+".service", "--no-pager", "-o", "cat", "-n", "5000")
	for _, m := range listenLogRe.FindAllStringSubmatch(string(out), -1) {
		addr = m[1]
		if m[2] != "" {
			instanceID = m[2]
		}
	}
	return addr, instanceID
}

// discoverSystemd is the Linux Discover over an injectable runner.
func discoverSystemd(run cmdRunner) ([]Candidate, []string, error) {
	entries, err := listUnits(run)
	if err != nil {
		return nil, nil, err
	}
	cands := discoverFromServices(entries)
	const noLogNote = "no 'local_api listening' log line found — base_url derived from the 10000+port rule"
	for i := range cands {
		c := &cands[i]
		var kept []string
		hadNoLog := false
		for _, n := range c.Notes {
			if n == noLogNote {
				hadNoLog = true
				continue
			}
			kept = append(kept, n)
		}
		if !hadNoLog {
			continue
		}
		addr, inst := journalListen(run, c.Service)
		if addr == "" {
			c.Notes = append(kept, noLogNote+" (journal not readable or no line)")
			continue
		}
		c.Notes = kept
		c.BaseURL = "http://" + addr
		if inst != "" {
			c.InstanceID = inst
		}
	}
	var notes []string
	if hasClusterNodeUnit(run) {
		notes = append(notes, "hqclusternode detected on this host: Firebird engine restarts should go through the cluster node (its restart window / pending_restart state), not fb_agent_restart directly")
	}
	if len(cands) == 0 {
		notes = append(notes, "no systemd unit runs an fbagent binary (systemctl list-units '*fbagent*')")
	}
	return cands, notes, nil
}

func hasClusterNodeUnit(run cmdRunner) bool {
	out, _ := run("systemctl", "list-unit-files", "--type=service", "--no-legend", "*clusternode*")
	return len(parseUnitNames(string(out))) > 0
}
