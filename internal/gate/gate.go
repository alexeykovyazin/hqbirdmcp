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
	"github.com/aleks/fbmcp/internal/killpoint"
	"github.com/aleks/fbmcp/internal/policy"
	"github.com/aleks/fbmcp/internal/state"
)

const DefaultTTL = 15 * time.Minute // main plan §5.1

var (
	ErrExpired = errors.New("confirmation expired — re-request the operation")
	ErrReplay  = errors.New("token already used (single-use)")
	// ErrBadToken: the token does not belong to this request (or its args).
	ErrBadToken      = errors.New("wrong in-band token for this request: copy request_id and token from the same pending reply")
	ErrUnknown       = errors.New("unknown request id")
	ErrIdentity      = errors.New("token bound to a different identity")
	ErrChannelPolicy = errors.New("this tier cannot be confirmed through the in-band channel")
	// ErrOOBOnly: the requesting identity has inband_confirm: false (or
	// profile autonomous), so even Tier 1 needs the out-of-band surface.
	ErrOOBOnly = errors.New("this identity cannot confirm in-band (inband_confirm: false / profile autonomous) — an operator must approve out of band (tray or fbmcp ctl approve)")
)

// Channels (§5.5). Trust increases downward.
const (
	ChannelElicitation = "elicitation" // Tier 1 only (client UX requirement documented)
	ChannelInBand      = "in-band-token"
	ChannelOutOfBand   = "out-of-band" // approval page / fbmcp approve CLI
)

// Gate issues and consumes confirmations.
type Gate struct {
	st        *state.Store
	aud       *audit.Logger
	ttl       time.Duration
	now       func() time.Time
	onPending func(state.PendingAction)
	onConfirm func(state.PendingAction, string)
	onExpire  func(state.PendingAction)
}

func New(st *state.Store, aud *audit.Logger) *Gate {
	return &Gate{st: st, aud: aud, ttl: DefaultTTL, now: time.Now}
}

func (g *Gate) WithTTL(d time.Duration) *Gate    { g.ttl = d; return g }
func (g *Gate) WithNow(f func() time.Time) *Gate { g.now = f; return g }

// SetHooks wires K7 (optional; nil-safe).
func (g *Gate) SetHooks(onPending func(state.PendingAction), onConfirm func(state.PendingAction, string), onExpire func(state.PendingAction)) {
	g.onPending, g.onConfirm, g.onExpire = onPending, onConfirm, onExpire
}

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
		OOBOnly:       id.OOBOnly,
		Profile:       id.Profile,
		RemoteAddr:    id.RemoteAddr,
	}
	if err := g.st.AddPending(p); err != nil {
		return state.PendingAction{}, err
	}
	killpoint.Hit("gate.pending") // chaos harness: kill with an unconsumed pending action on disk
	detail := map[string]interface{}{"request_id": p.ID, "expires_in": g.ttl.String()}
	if id.RemoteAddr != "" { // phase 12 B0.8
		detail["remote_addr"] = id.RemoteAddr
	}
	if id.Profile != "" {
		detail["profile"] = id.Profile
	}
	if p.OOBOnly {
		detail["oob_only"] = true
	}
	g.aud.Log(audit.Entry{Identity: id.Name, Database: db, Tool: meta.Name, Tier: meta.Tier, Decision: "pending", Detail: detail})
	if g.onPending != nil {
		g.onPending(p)
	}
	return p, nil
}

// SweepExpired removes timed-out pending actions and notifies K7.
func (g *Gate) SweepExpired() int {
	n := 0
	for _, p := range g.st.Pending() {
		if g.now().After(p.Expires) {
			if _, ok, _ := g.st.TakePending(p.ID); ok {
				n++
				g.aud.Log(audit.Entry{Identity: p.Identity, Database: p.Database, Tool: p.Tool, Tier: p.Tier, Decision: "denied",
					Detail: map[string]interface{}{"request_id": p.ID, "reason": "expired"}})
				if g.onExpire != nil {
					g.onExpire(p)
				}
			}
		}
	}
	return n
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
	if p.Tier >= 2 && channel != ChannelOutOfBand {
		g.requeue(p)
		g.aud.Log(audit.Entry{Identity: identity, Database: p.Database, Tool: p.Tool, Tier: p.Tier, Decision: "denied", Channel: channel,
			Detail: map[string]interface{}{"request_id": requestID, "reason": "channel policy: tier >= 2 requires out-of-band"}})
		return state.PendingAction{}, fmt.Errorf("%w (tier %d)", ErrChannelPolicy, p.Tier)
	}
	if p.OOBOnly && channel != ChannelOutOfBand { // phase 12 B8
		g.requeue(p)
		g.aud.Log(audit.Entry{Identity: identity, Database: p.Database, Tool: p.Tool, Tier: p.Tier, Decision: "denied", Channel: channel,
			Detail: map[string]interface{}{"request_id": requestID, "reason": "identity is out-of-band only"}})
		return state.PendingAction{}, ErrOOBOnly
	}
	if channel == ChannelInBand {
		if token == "" {
			g.requeue(p)
			return state.PendingAction{}, errors.New("missing in-band token")
		}
		if token != IssueToken(requestID, p.ArgHash) {
			g.requeue(p)
			return state.PendingAction{}, ErrBadToken
		}
	}

	g.aud.Log(audit.Entry{Identity: identity, Database: p.Database, Tool: p.Tool, Tier: p.Tier, Decision: "approved", Channel: channel,
		Detail: map[string]interface{}{"request_id": requestID}})
	killpoint.Hit("gate.confirmed") // chaos harness: kill after consume, before dispatch
	p.ConfirmedChannel = channel
	if g.onConfirm != nil {
		g.onConfirm(p, channel)
	}
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

// ImpactStatement renders the human-facing impact text: what runs, who
// asked, when the request expires, and how it can be confirmed. The chat,
// the tray and `fbmcp ctl approve` all show this text (UX W1.2).
func ImpactStatement(p state.PendingAction) string {
	return impactStatement(p, time.Now())
}

func impactStatement(p state.PendingAction, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\nTool: %s | Tier: %d | Database: %s\n", p.ImpactText, p.Tool, p.Tier, p.Database)
	fmt.Fprintf(&b, "Requested by: %s\n", Requester(p))
	fmt.Fprintf(&b, "Request ID: %s\n", p.ID)
	fmt.Fprintf(&b, "Expires: %s\n", expiryText(p.Expires, now))
	b.WriteString("Confirm: " + ConfirmHow(p.Tier, p.OOBOnly) + "\n")
	return b.String()
}

// ConfirmHow says in plain words how a request of this tier is confirmed.
func ConfirmHow(tier int, oobOnly bool) string {
	switch {
	case tier >= 3:
		return "disabled (Tier 3)"
	case tier == 2:
		return "out-of-band only (fbmcp tray, or fbmcp ctl approve on the server)"
	case oobOnly:
		return "out-of-band only — this identity cannot confirm in chat (fbmcp tray, or fbmcp ctl approve on the server)"
	default:
		return "in chat (fb_confirm with the token) or out-of-band (fbmcp tray, fbmcp ctl approve)"
	}
}

// Requester names who asked: identity, api-key profile and TCP peer.
func Requester(p state.PendingAction) string {
	var extra []string
	if p.Profile != "" {
		extra = append(extra, "profile "+p.Profile)
	}
	if p.RemoteAddr != "" {
		extra = append(extra, "from "+p.RemoteAddr)
	}
	if len(extra) == 0 {
		return p.Identity
	}
	return p.Identity + " (" + strings.Join(extra, ", ") + ")"
}

// expiryText is the expiry in server-local time plus UTC and the time left.
func expiryText(exp, now time.Time) string {
	left := exp.Sub(now).Round(time.Minute)
	rel := fmt.Sprintf("in %d min", int(left.Minutes()))
	if left <= 0 {
		rel = "expired"
	}
	return fmt.Sprintf("%s server time (%s, %s)", exp.Local().Format("2006-01-02 15:04"), exp.UTC().Format("15:04Z"), rel)
}

// ExpiryText is expiryText at the current time (fb_job_status by request_id).
func ExpiryText(exp time.Time) string { return expiryText(exp, time.Now()) }
