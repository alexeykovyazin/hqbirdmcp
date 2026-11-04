//go:build linux

package agentdiscover

import (
	"context"
	"os/exec"
	"time"
)

func execRunner(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

// Discover enumerates systemd units that run fbagent (phase 12 B9).
func Discover() ([]Candidate, []string, error) {
	return discoverSystemd(execRunner)
}

// HasClusterNode reports whether an hqclusternode unit is installed.
func HasClusterNode() bool { return hasClusterNodeUnit(execRunner) }
