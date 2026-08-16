// Package gate implements the P1.6 human gate: pending actions with
// single-use TTL tokens bound to (action, database, identity), the per-tier
// channel policy (§5.5 / ADR-006/007), and the confirmation entry points
// used by fb_confirm (in-band, Tier 1 only) and the out-of-band surface.
package gate

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/policy"
	"github.com/aleks/fbmcp/internal/state"
)

const DefaultTTL = 15 * time.Minute // main plan §5.1

var (
	ErrExpired       = errors.New("confirmation expired — re-request the operation")
	ErrReplay        = errors.New("token already used (single-use)")
	ErrUnknown       = errors.New("unknown request id")
	ErrIdentity      = errors.New("token bound to a different identity")
	ErrChannelPolicy = errors.New("this tier cannot be confirmed through the in-band channel")
)

// Channels (§5.5). Trust increases downward.
const (
	ChannelElicitation = "elicitation" // Tier 1 only (client UX requirement documented)
	ChannelInBand      = "in-band-token"
	ChannelOutOfBand   = "out-of-band" // approval page / fbmcp approve CLI
)

// Gate issues and consumes confirmations.
type Gate struct {
	st  *state.Store
	aud *audit.Logger
	ttl time.Duration
	now func() time.Time
}

func New(st *state.Store, aud *audit.Logger) *Gate {
	return &Gate{st: st, aud: aud, ttl: DefaultTTL, now: time.Now}
}

func (g *Gate) WithTTL(d time.Duration) *Gate { g.ttl = d; return g }
func (g *Gate) WithNow(f func() time.Time) *Gate { g.now = f; return g }

// Request records a pending action and returns it (with its id) for the
// impact-statement response. The token is NOT returned here — Confirm issues it.
func (g *Gate) Request(id policy.Identity, db string, meta policy.ToolMeta, impactText, argHash string, preconds []string) (state.PendingAction, error) {
	p := state.PendingAction{
		ID:            newID(),
		Created:       g.now(),
		Expires:       g.now().Add(g.ttl),
		Identity:      id.Name,
		Database:      db,
		Tool:          meta.Name,
		Tier:          meta.Tier,
		ImpactText:    impactText,
		ArgHash:       argHash,
		Preconditions: toMap(preconds),
	}
	if err := g.st.AddPending(p); err != nil {
		return state.PendingAction{}, err
	}
	g.aud.Log(audit.Entry{Identity: id.Name, Database: db, Tool: meta.Name, Tier: meta.Tier, Decision: "pending",
		Detail: map[string]interface{}{"request_id": p.ID, "expires_in": g.ttl.String()}})
	return p, nil
}

// AllowedChannels returns the confirmation channels accepted for a tier
// (ADR-007). Displayed in the pending-action impact statement.
func AllowedChannels(tier int) []string {
	switch {
	case tier <= 1:
		return []string{ChannelElicitation, ChannelInBand, ChannelOutOfBand}
	case tier == 2:
		return []string{ChannelOutOfBand}
	default:
		return []string{ChannelOutOfBand + " (dual control x2, disabled by default)"}
	}
}

// Confirm consumes a pending action. channel must be one the tier accepts;
// in-band/elicitation on Tier ≥ 2 is rejected (safety fuse #7).
// The token is the one derived by IssueToken for this action; out-of-band
// surface callers pass token="" (their authority is the channel itself).
func (g *Gate) Confirm(requestID, identity, channel, token string) (state.PendingAction, error) {
	p, ok, err := g.st.TakePending(requestID)
	if err != nil {
		return state.PendingAction{}, err
	}
	if !ok {
		return state.PendingAction{}, ErrUnknown
	}
	if g.now().After(p.Expires) {
		g.aud.Log(audit.Entry{Identity: identity, Database: p.Database, Tool: p.Tool, Tier: p.Tier, Decision: "denied", Channel: channel,
			Detail: map[string]interface{}{"request_id": requestID, "reason": "expired"}})
		return state.PendingAction{}, ErrExpired
	}
	if p.Identity != identity {
		g.requeue(p)
		return state.PendingAction{}, ErrIdentity
	}
	if channel == ChannelInBand {
		if token == "" || token != IssueToken(requestID, p.ArgHash) {
			g.requeue(p)
			return state.PendingAction{}, ErrReplay
		}
	}
	if p.Tier >= 2 && channel != ChannelOutOfBand {
		g.requeue(p)
		g.aud.Log(audit.Entry{Identity: identity, Database: p.Database, Tool: p.Tool, Tier: p.Tier, Decision: "denied", Channel: channel,
			Detail: map[string]interface{}{"request_id": requestID, "reason": "channel policy: tier >= 2 requires out-of-band"}})
		return state.PendingAction{}, fmt.Errorf("%w (tier %d)", ErrChannelPolicy, p.Tier)
	}
	g.aud.Log(audit.Entry{Identity: identity, Database: p.Database, Tool: p.Tool, Tier: p.Tier, Decision: "approved", Channel: channel,
		Detail: map[string]interface{}{"request_id": requestID}})
	return p, nil
}

// IssueToken derives the in-band confirmation token for a pending action.
// The LLM relays the request id + token; the human got them from the same
// conversation (Tier 1 only, per channel policy).
func IssueToken(requestID, argHash string) string {
	h := sha256.Sum256([]byte("fbmcp-token:" + requestID + ":" + argHash))
	return hex.EncodeToString(h[:8])
}

func (g *Gate) requeue(p state.PendingAction) {
	// action stays available (not consumed) — a wrong-channel or wrong-identity
	// attempt must not destroy the pending confirmation
	g.st.AddPending(p)
}

func newID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func toMap(preconds []string) map[string]string {
	if len(preconds) == 0 {
		return nil
	}
	m := make(map[string]string, len(preconds))
	for i, p := range preconds {
		m[fmt.Sprintf("check_%d", i)] = p
	}
	return m
}

// ImpactStatement renders the human-facing impact text with accepted channels.
func ImpactStatement(p state.PendingAction) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\nTier: %d | Database: %s | Request ID: %s\nExpires: %s\n",
		p.ImpactText, p.Tier, p.Database, p.ID, p.Expires.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "Accepted confirmation channels: %s\n", strings.Join(AllowedChannels(p.Tier), ", "))
	return b.String()
}
