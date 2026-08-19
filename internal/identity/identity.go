package identity

import (
	"context"

	"github.com/aleks/fbmcp/internal/policy"
)

type ctxKey struct{}

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

// Operator is the out-of-band confirmation identity (fbmcp approve CLI).
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

// Caller is the request identity, or Local if unset (stdio).
func Caller(ctx context.Context) policy.Identity {
	if id, ok := FromContext(ctx); ok {
		return id
	}
	return Local(2, nil)
}
