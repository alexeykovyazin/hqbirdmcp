//go:build !windows && !linux

package ctl

import (
	"errors"

	"github.com/aleks/fbmcp/internal/hqbirdsetup"
)

var errHqbirdOS = errors.New("hqbird-install runs on Windows and Linux only; use --no-service")

// otherHqbirdHost: HQbird has no setups for this OS. --no-service still
// writes the config.
type otherHqbirdHost struct{}

func newHqbirdHost() hqbirdHost { return otherHqbirdHost{} }

func (otherHqbirdHost) FirebirdService(string) string             { return "" }
func (otherHqbirdHost) Account() string                           { return "" }
func (otherHqbirdHost) PrepareFolders(hqbirdsetup.Paths) error    { return errHqbirdOS }
func (otherHqbirdHost) AfterWrite(...string) error                { return nil }
func (otherHqbirdHost) EnsureService(string, string) error        { return errHqbirdOS }
func (otherHqbirdHost) GrantFirebird([]string) []string           { return nil }
func (otherHqbirdHost) GrantAgentConfigs([]string) []string       { return nil }
func (otherHqbirdHost) GrantKeyRead(string) error                 { return nil }
func (otherHqbirdHost) RemoveFirewall(*hqbirdsetup.Managed) error { return nil }
func (otherHqbirdHost) State() string                             { return "absent" }
func (otherHqbirdHost) Start() error                              { return errHqbirdOS }
func (otherHqbirdHost) StopDisable() error                        { return nil }
func (otherHqbirdHost) Firewall(int, []string, string, *hqbirdsetup.Managed) error {
	return errHqbirdOS
}
