//go:build windows

package agentdiscover

import (
	"sort"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const servicesKey = `SYSTEM\CurrentControlSet\Services`

// Discover enumerates the local fbagent installs (HQbirdFBAgent-* service
// registry rows — locale-independent and readable without elevation, unlike
// `sc` output) and returns candidates plus host-level notes.
func Discover() ([]Candidate, []string, error) {
	entries, err := listAgentServices()
	if err != nil {
		return nil, nil, err
	}
	cands := discoverFromServices(entries)
	// Secondary source: the IBSurgeon HQbird registry tree (covers only
	// some installs — note candidates it knows that SCM did not list).
	for _, extra := range registryCandidatesNotIn(cands) {
		cands = append(cands, extra)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].Service < cands[j].Service })

	var notes []string
	if HasClusterNode() {
		notes = append(notes, "hqclusternode detected on this host: Firebird engine restarts should go through the cluster node (its restart window / pending_restart state), not fb_agent_restart directly")
	}
	return cands, notes, nil
}

// listAgentServices reads HKLM\SYSTEM\CurrentControlSet\Services for
// HQbirdFBAgent-* subkeys and their ImagePath values.
func listAgentServices() ([]serviceEntry, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, servicesKey, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil, err
	}
	defer k.Close()
	names, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return nil, err
	}
	var out []serviceEntry
	for _, name := range names {
		if !strings.HasPrefix(strings.ToLower(name), "hqbirdfbagent") {
			continue
		}
		sk, err := registry.OpenKey(registry.LOCAL_MACHINE, servicesKey+`\`+name, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		img, _, _ := sk.GetStringValue("ImagePath")
		sk.Close()
		out = append(out, serviceEntry{Name: name, ImagePath: img})
	}
	return out, nil
}

// registryCandidatesNotIn reads HKLM\SOFTWARE\IBSurgeon\HQbird2027 (and its
// engine subtrees) for AgentDir/ConfigPath rows that SCM enumeration missed.
func registryCandidatesNotIn(have []Candidate) []Candidate {
	seen := map[string]bool{}
	for _, c := range have {
		if c.ConfigPath != "" {
			seen[strings.ToLower(c.ConfigPath)] = true
		}
	}
	var out []Candidate
	walk := func(path string) {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
		if err != nil {
			return
		}
		cfgPath, _, _ := k.GetStringValue("ConfigPath")
		k.Close()
		if cfgPath == "" || seen[strings.ToLower(cfgPath)] {
			return
		}
		seen[strings.ToLower(cfgPath)] = true
		c := buildCandidate(serviceEntry{Name: "(registry HQbird2027)", ImagePath: `--run "` + cfgPath + `"`})
		c.Notes = append(c.Notes, "found only in HKLM\\SOFTWARE\\IBSurgeon\\HQbird2027 (no HQbirdFBAgent service row) — check the service exists before registering")
		out = append(out, c)
	}
	root := `SOFTWARE\IBSurgeon\HQbird2027`
	walk(root)
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, root, registry.ENUMERATE_SUB_KEYS); err == nil {
		if subs, err := k.ReadSubKeyNames(-1); err == nil {
			for _, s := range subs {
				walk(root + `\` + s)
				if k2, err := registry.OpenKey(registry.LOCAL_MACHINE, root+`\`+s, registry.ENUMERATE_SUB_KEYS); err == nil {
					if subs2, err := k2.ReadSubKeyNames(-1); err == nil {
						for _, s2 := range subs2 {
							walk(root + `\` + s + `\` + s2)
						}
					}
					k2.Close()
				}
			}
		}
		k.Close()
	}
	return out
}

// HasClusterNode reports whether an hqclusternode service is installed.
func HasClusterNode() bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, servicesKey, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return false
	}
	defer k.Close()
	names, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return false
	}
	for _, n := range names {
		if strings.Contains(strings.ToLower(n), "clusternode") {
			return true
		}
	}
	return false
}
