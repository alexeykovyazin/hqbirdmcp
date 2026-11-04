//go:build windows

package main

import (
	"testing"

	"github.com/aleks/fbmcp/internal/state"
)

// Phase 12 test W: an autonomous key's Tier-1 request is OOB-only, so the
// tray must show it; a plain Tier-1 request is confirmed in-band.
func TestNeedsOperator(t *testing.T) {
	cases := []struct {
		p    state.PendingAction
		want bool
	}{
		{state.PendingAction{Tier: 1}, false},
		{state.PendingAction{Tier: 1, OOBOnly: true}, true},
		{state.PendingAction{Tier: 2}, true},
		{state.PendingAction{Tier: 3}, true},
	}
	for _, c := range cases {
		if got := needsOperator(c.p); got != c.want {
			t.Errorf("needsOperator(tier %d, oob %v) = %v, want %v", c.p.Tier, c.p.OOBOnly, got, c.want)
		}
	}
}
