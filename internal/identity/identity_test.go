package identity

import (
	"context"
	"testing"
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
	if id := APIKey("x", 0); id.MaxTier != 2 {
		t.Fatalf("zero max_tier should default to 2, got %d", id.MaxTier)
	}
	if id := Operator(); id.MaxTier != 3 || id.Kind != "operator" {
		t.Fatalf("operator = %+v", id)
	}
}
