# ADR-007 — Confirmation channel policy, tier mapping (D7)

Status: accepted (2026-08-16) · Fed by: P0.4 threat model

## Decision
Confirmed as proposed, with two sharpenings from the threat register:
- **Tier 1:** single confirmation; accepted channels = client elicitation /
  approval UX (client requirement documented) or in-band token. Out-of-band
  also accepted.
- **Tier 2:** preconditions + dry-run where available; **out-of-band only**
  (T-01, T-18: in-band and elicitation channels cannot prove operator intent).
- **Tier 3:** disabled by default; config unlock + dual control via
  out-of-band by two distinct operator identities.
- Windows trusted-auth finding (T-11): the OOB CLI's authority derives from
  the invoking OS account — the dedicated service account is the local
  privilege boundary; approval CLI and server must run in that trust domain
  (operator docs, P6.3).

## Consequences
Policy engine (P1.5) carries channel acceptance per tier; gate (P1.6)
enforces; audit logs channel; fuse #7 verifies.
