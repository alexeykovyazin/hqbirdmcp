package gate

import (
	"strings"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/state"
)

// UX review W1.2: the impact statement names the requester and gives the
// expiry in server time with the time left.
func TestImpactStatementRequesterAndExpiry(t *testing.T) {
	now := time.Date(2026, 11, 6, 13, 17, 0, 0, time.UTC)
	p := state.PendingAction{
		ID: "r1", Tool: "fb_set_forcewrite", Tier: 1, Database: "spike5", ImpactText: "Set Forced Writes on spike5.",
		Identity: "alice", Profile: "autonomous", RemoteAddr: "10.0.0.21", Expires: now.Add(15 * time.Minute),
	}
	out := impactStatement(p, now)
	for _, want := range []string{
		"Requested by: alice (profile autonomous, from 10.0.0.21)",
		"Request ID: r1",
		"13:32Z, in 15 min",
		"Confirm: in chat (fb_confirm",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("impact statement lacks %q:\n%s", want, out)
		}
	}
	if got := expiryText(now.Add(-time.Minute), now); !strings.Contains(got, "expired") {
		t.Errorf("past expiry: %q", got)
	}
	if got := Requester(state.PendingAction{Identity: "local"}); got != "local" {
		t.Errorf("plain requester: %q", got)
	}
}
