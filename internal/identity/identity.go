// Package identity implements the P1.3 identity surface in its Phase-1
// (local) form: stdio-local callers map to a single local identity with a
// configurable tier ceiling and optional database scope. Remote identities
// (API keys, per-tool/per-DB profiles) arrive in P5.1 and will reuse this
// type via the façade on every transport.
package identity

import "github.com/aleks/fbmcp/internal/policy"

// Local returns the stdio-local identity. In v1 local mode is the explicit
// opt-out documented in P1.3: the OS user is the trust root (threat T-11).
func Local(maxTier int, dbs []string) policy.Identity {
	return policy.Identity{Name: "local", MaxTier: maxTier, DBs: dbs, Kind: "local"}
}

// Operator is the out-of-band confirmation identity (fbmcp approve CLI /
// approval page). Confirmations by the operator identity count as
// out-of-band channel authority.
func Operator() policy.Identity {
	return policy.Identity{Name: "operator", MaxTier: 3, Kind: "operator"}
}
