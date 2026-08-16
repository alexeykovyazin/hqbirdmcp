# ADR-006 — Confirmation channels (D3)

Status: accepted (2026-08-16) · Fed by: P0.3

## Context
Tier ≥ 1 mutations need a human gate. Candidate channels: client approval UX /
elicitation, in-band `fb_confirm` token, out-of-band surface.

## Evidence (P0.3)
SDK v1.7.0 / protocol 2026-07-28: elicitation is multi-round-trip (SEP-2322) —
tool returns `InputRequests`, client elicits and retries with
`InputResponses`; round-trip verified working. Older clients are bridged by
the SDK's server middleware. Elicitation proves only "someone interacted with
the client UI" — equivalent trust to client-approval UX.

## Decision
All three channels specified from day one (P1.6):
1. **Client elicitation/approval UX** — Tier 1; documented client
   requirement; uses InputRequests round-trip.
2. **`fb_confirm` in-band token** — Tier 1 only; single-use, TTL, identity-
   bound; always rejected for Tier ≥ 2.
3. **Out-of-band surface** — localhost approval page + `fbmcp approve` CLI —
   required for Tier 2, dual-control for Tier 3.

## Consequences
Fuse #7 enforces Tier-2 in-band rejection. Audit records the channel used.
Channel trust model is §5.5 of the main plan, unchanged.
