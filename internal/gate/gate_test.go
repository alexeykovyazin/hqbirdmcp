package gate

import (
	"strings"
	"testing"
	"time"

	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/policy"
	"github.com/aleks/fbmcp/internal/state"
)

func newGate(t *testing.T) (*Gate, *state.Store, *audit.Logger) {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	aud, err := audit.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { aud.Close() })
	return New(st, aud), st, aud
}

var demoMeta = policy.ToolMeta{Name: "fb_demo_write", Tier: 1}
var dropMeta = policy.ToolMeta{Name: "fb_demo_drop", Tier: 2}

func TestTier1HappyPath(t *testing.T) {
	g, st, _ := newGate(t)
	p, err := g.Request(policy.Identity{Name: "local", MaxTier: 2}, "spike5", demoMeta, "demo impact", "arghash1", nil)
	if err != nil {
		t.Fatal(err)
	}
	tok := IssueToken(p.ID, "arghash1")
	got, err := g.Confirm(p.ID, "local", ChannelInBand, tok)
	if err != nil {
		t.Fatalf("tier1 in-band confirm failed: %v", err)
	}
	if got.Tool != "fb_demo_write" {
		t.Fatalf("wrong action: %+v", got)
	}
	// token replay: pending consumed by TakePending
	if _, err := g.Confirm(p.ID, "local", ChannelInBand, tok); err == nil {
		t.Fatal("token replay accepted (single-use violated)")
	}
	if len(st.Pending()) != 0 {
		t.Fatal("pending action not consumed")
	}
}

// Safety fuse #7 (main plan §8): Tier-2 confirmed in-band only ⇒ REJECTED;
// out-of-band channel succeeds.
func TestFuse7Tier2InBandRejected(t *testing.T) {
	g, _, _ := newGate(t)
	p, err := g.Request(policy.Identity{Name: "local", MaxTier: 2}, "spike5", dropMeta, "drop impact", "ah", nil)
	if err != nil {
		t.Fatal(err)
	}
	tok := IssueToken(p.ID, "ah")
	if _, err := g.Confirm(p.ID, "local", ChannelInBand, tok); err == nil {
		t.Fatal("FUSE FAILURE: tier-2 confirmed in-band")
	}
	if _, err := g.Confirm(p.ID, "local", ChannelElicitation, tok); err == nil {
		t.Fatal("FUSE FAILURE: tier-2 confirmed via elicitation channel")
	}
	// action survived the rejected attempts (not consumed)
	if _, err := g.Confirm(p.ID, "local", ChannelOutOfBand, ""); err != nil {
		t.Fatalf("tier-2 out-of-band confirm failed: %v", err)
	}
}

func TestExpiry(t *testing.T) {
	g, _, _ := newGate(t)
	g.WithTTL(1 * time.Millisecond)
	p, _ := g.Request(policy.Identity{Name: "local", MaxTier: 2}, "spike5", demoMeta, "x", "ah", nil)
	time.Sleep(5 * time.Millisecond)
	if _, err := g.Confirm(p.ID, "local", ChannelInBand, IssueToken(p.ID, "ah")); err == nil {
		t.Fatal("expired token accepted")
	} else if !strings.Contains(err.Error(), "expired") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestIdentityBinding(t *testing.T) {
	g, _, _ := newGate(t)
	p, _ := g.Request(policy.Identity{Name: "alice", MaxTier: 2}, "spike5", demoMeta, "x", "ah", nil)
	if _, err := g.Confirm(p.ID, "mallory", ChannelInBand, IssueToken(p.ID, "ah")); err == nil {
		t.Fatal("token confirmed by different identity")
	}
}

func TestWrongTokenRejected(t *testing.T) {
	g, _, _ := newGate(t)
	p, _ := g.Request(policy.Identity{Name: "local", MaxTier: 2}, "spike5", demoMeta, "x", "ah", nil)
	if _, err := g.Confirm(p.ID, "local", ChannelInBand, "deadbeef"); err == nil {
		t.Fatal("forged token accepted")
	}
}

func TestChannelsAdvertised(t *testing.T) {
	if c := AllowedChannels(2); len(c) != 1 || c[0] != ChannelOutOfBand {
		t.Fatalf("tier2 channels: %v", c)
	}
	if c := AllowedChannels(1); len(c) != 3 {
		t.Fatalf("tier1 channels: %v", c)
	}
}
