//go:build !windows

package agentdiscover

import "fmt"

// Discover is Windows-only in v1: the enumeration anchor is the Windows
// service registry. Linux installs would enumerate systemd units instead —
// not implemented (the lab and production hosts are Windows).
func Discover() ([]Candidate, []string, error) {
	return nil, nil, fmt.Errorf("fb_agent_discover is Windows-only in this build (service-registry enumeration)")
}

// HasClusterNode: no service registry to consult off Windows.
func HasClusterNode() bool { return false }
