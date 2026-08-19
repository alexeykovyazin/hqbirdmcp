package identity

import (
	"context"
	"sync/atomic"

	"github.com/aleks/fbmcp/internal/policy"
)

type ctxKey struct{}

// localMaxTier is the ceiling for the stdio-local fallback identity
// (WS2.3: configurable via SetLocalMaxTier instead of a hard literal).
var localMaxTier atomic.Int32

// fallbacks counts Caller() invocations that found no identity on the
// context. In stdio mode that is the normal path; in remote mode a non-zero
// delta means some handler lost the request context — observable via
// FallbackCount instead of silently minting a Tier-2 identity.
var fallbacks atomic.Int64

func init() { localMaxTier.Store(2) }

// SetLocalMaxTier sets the ceiling of the local fallback identity
// (fbmcp.yaml local_max_tier; values outside 0-2 are clamped — Tier 3 stays
// unreachable by policy regardless).
func SetLocalMaxTier(t int) {
	if t < 0 {
		t = 0
	}
	if t > 2 {
		t = 2
	}
	localMaxTier.Store(int32(t))
}

// FallbackCount reports how many Caller() calls fell back to the local
// identity since process start (self-observability; see selfobs).
func FallbackCount() int64 { return fallbacks.Load() }

// Local returns the stdio-local identity. In v1 local mode is the explicit
// opt-out documented in P1.3: the OS user is the trust root (threat T-11).
func Local(maxTier int, dbs []string) policy.Identity {
	return policy.Identity{Name: "local", MaxTier: maxTier, DBs: dbs, Kind: "local"}
}

// APIKey is a remote identity (P5.1 / ADR-022).
func APIKey(name string, maxTier int) policy.Identity {
	if maxTier <= 0 {
		maxTier = 2
	}
	return policy.Identity{Name: name, MaxTier: maxTier, Kind: "api-key"}
}

// Operator is the out-of-band confirmation identity (fbmcpctl approve).
func Operator() policy.Identity {
	return policy.Identity{Name: "operator", MaxTier: 3, Kind: "operator"}
}

func With(ctx context.Context, id policy.Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

func FromContext(ctx context.Context) (policy.Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(policy.Identity)
	return id, ok
}

// Caller is the request identity, or the local fallback if unset (stdio).
func Caller(ctx context.Context) policy.Identity {
	if id, ok := FromContext(ctx); ok {
		return id
	}
	fallbacks.Add(1)
	return Local(int(localMaxTier.Load()), nil)
}
