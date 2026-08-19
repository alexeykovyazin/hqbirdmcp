// Package statetest holds test doubles for state.FactsProvider, shared by
// tests across packages (test files cannot be imported cross-package). Not
// for production use.
package statetest

import (
	"fmt"

	"github.com/aleks/fbmcp/internal/state"
)

// StubFacts returns fixed values; unknown facts error (fail-closed, matching
// the production providers' contract).
type StubFacts map[string]any

var _ state.FactsProvider = StubFacts{}

func (s StubFacts) Fact(_ state.FactContext, name string, _ map[string]string) (any, error) {
	if v, ok := s[name]; ok {
		return v, nil
	}
	return nil, fmt.Errorf("no facts provider registered for %q (fail-closed)", name)
}
