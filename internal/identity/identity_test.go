package identity

import (
	"context"
	"testing"

	"github.com/aleks/fbmcp/internal/config"
)

func TestContextRoundTrip(t *testing.T) {
	id := APIKey("ci", 1)
	ctx := With(context.Background(), id)
	got, ok := FromContext(ctx)
	if !ok || got.Name != "ci" || got.MaxTier != 1 || got.Kind != "api-key" {
		t.Fatalf("round trip: %+v ok=%v", got, ok)
	}
	if c := Caller(ctx); c.Name != "ci" {
		t.Fatalf("Caller with identity set = %+v", c)
	}
}

func TestCallerFallbackCountsAndUsesCeiling(t *testing.T) {
	SetLocalMaxTier(1)
	defer SetLocalMaxTier(2)
	before := FallbackCount()
	c := Caller(context.Background())
	if c.Name != "local" || c.Kind != "local" {
		t.Fatalf("fallback identity = %+v", c)
	}
	if c.MaxTier != 1 {
		t.Fatalf("ceiling not applied: MaxTier=%d want 1", c.MaxTier)
	}
	if FallbackCount() != before+1 {
		t.Fatalf("fallback not counted: %d -> %d", before, FallbackCount())
	}
}

func TestSetLocalMaxTierClamps(t *testing.T) {
	defer SetLocalMaxTier(2)
	SetLocalMaxTier(-3)
	if c := Caller(context.Background()); c.MaxTier != 0 {
		t.Fatalf("negative not clamped to 0: %d", c.MaxTier)
	}
	SetLocalMaxTier(9)
	if c := Caller(context.Background()); c.MaxTier != 2 {
		t.Fatalf("Tier 3+ not clamped to 2: %d", c.MaxTier)
	}
}

func TestAPIKeyDefaultTier(t *testing.T) {
	// Phase 12 B0.1 (F1): 0 is read-only, never Tier 2.
	if id := APIKey("x", 0); id.MaxTier != 0 {
		t.Fatalf("max_tier 0 must stay 0 (read-only), got %d", id.MaxTier)
	}
	if id := APIKey("x", 7); id.MaxTier != 2 {
		t.Fatalf("max_tier above 2 must clamp to 2, got %d", id.MaxTier)
	}
	if id := Operator(); id.MaxTier != 3 || id.Kind != "operator" {
		t.Fatalf("operator = %+v", id)
	}
}

func TestFromConfigTierAndProfile(t *testing.T) {
	cases := []struct {
		id      config.APIIdentity
		tier    int
		oobOnly bool
	}{
		{config.APIIdentity{Name: "a"}, 1, false},                          // missing → 1 (R5)
		{config.APIIdentity{Name: "a", MaxTier: config.Tier(0)}, 0, false}, // 0 → read-only
		{config.APIIdentity{Name: "a", MaxTier: config.Tier(2)}, 2, false},
		{config.APIIdentity{Name: "a", Profile: "autonomous"}, 0, true}, // B8 default
		{config.APIIdentity{Name: "a", Profile: "autonomous", MaxTier: config.Tier(1)}, 1, true},
		{config.APIIdentity{Name: "a", Profile: "autonomous", InbandConfirm: boolp(true)}, 0, true}, // profile forces false
		{config.APIIdentity{Name: "a", InbandConfirm: boolp(false)}, 1, true},
	}
	for i, c := range cases {
		got := FromConfig(c.id)
		if got.MaxTier != c.tier || got.OOBOnly != c.oobOnly || got.Kind != "api-key" {
			t.Fatalf("case %d: %+v, want tier %d oobOnly %v", i, got, c.tier, c.oobOnly)
		}
	}
}

func boolp(b bool) *bool { return &b }
