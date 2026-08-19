package main

import (
	"context"
	"strings"
	"testing"

	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/policy"
)

// C3: remote API-key identity still cannot confirm Tier ≥ 2 in-band.
func TestC3APIKeyCannotConfirmTier2InBand(t *testing.T) {
	gt := newTestGT(t)
	ctx := identity.With(context.Background(), identity.APIKey("op", 2))
	meta := policy.ToolMeta{Name: "fb_restore_replace", Tier: 2, Scope: "database"}
	out := gt.requestGated(ctx, meta, "restore %s", "spike5", nil, "", nil)
	if strings.HasPrefix(out, "DENIED:") {
		t.Fatalf("tier-2 request denied before confirm: %s", out)
	}
	if len(gt.st.Pending()) != 1 {
		t.Fatalf("pending=%d out=%s", len(gt.st.Pending()), out)
	}
	p := gt.st.Pending()[0]
	if _, err := gt.g.Confirm(p.ID, "op", "in-band-token", "forged"); err == nil {
		t.Fatal("C3 FAIL: API-key in-band confirmed Tier 2")
	}
	if _, err := gt.g.Confirm(p.ID, "op", "out-of-band", ""); err != nil {
		t.Fatalf("OOB should succeed: %v", err)
	}
}

func TestC3IdentityMismatchOnConfirm(t *testing.T) {
	gt := newTestGT(t)
	local := identity.Local(2, nil)
	ctx := identity.With(context.Background(), local)
	meta := policy.ToolMeta{Name: "fb_backup_start", Tier: 1, Scope: "database"}
	_ = gt.requestGated(ctx, meta, "x %s", "spike5", nil, "", nil)
	p := gt.st.Pending()[0]
	if _, err := gt.g.Confirm(p.ID, "op", "in-band-token", "x"); err == nil {
		t.Fatal("remote identity confirmed a local-bound pending action")
	}
}
