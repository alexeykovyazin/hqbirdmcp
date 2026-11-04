//go:build !windows && !linux

package agentdiscover

import "fmt"

// Discover has no enumeration on this OS (Windows: service registry;
// Linux: systemd units, phase 12 B9).
func Discover() ([]Candidate, []string, error) {
	return nil, nil, fmt.Errorf("fb_agent_discover supports Windows and Linux only")
}

// HasClusterNode: no service registry to consult off Windows.
func HasClusterNode() bool { return false }
