# ADR-029: Tier-3 dual-control is deliberately not built (by-design-disabled)

Date: 2026-08-25
Status: accepted
Context: improvement-plan E.2 / phase8_plan D4.2; closes the C12 accepted-residual.

## Context

The claims register's C12 residual: "dual-control for Tier-3 (`fb_db_drop`)".
Tier 3 is the only tier that would ever need two-person confirmation. Today
Tier 3 is **disabled and not schedulable** (policy tests + schedule tests
prove it), and `fb_db_drop`'s executor is a stub that refuses with
"Tier 3 DROP DATABASE is disabled".

Building true dual-control would require, at minimum:

- `internal/identity` to model the approving *principal* (today
  `identity.Operator()` is a generic "operator" string — two distinct humans
  are inexpressible);
- `fbmcpctl approve` to capture and sign with the invoking OS user;
- `internal/gate` to require two distinct principals, both within TTL,
  order-fixed, on the gate record.

## Decision

**Do not build dual-control.** Tier 3 stays disabled by design:

1. The only consumer (`fb_db_drop`) is a stub; DROP DATABASE is recoverable
   only by restore, and the operator already has `fb_restore_replace` — the
   controlled, snapshotted path — for destructive lifecycle changes.
2. The cost is not the implementation but the *review* burden: two-person
   logic that must be proven unfakeable on a channel the model cannot reach.
   That proof does not exist for a feature nobody can invoke.
3. Fail-closed default: an unbuilt path cannot fail open.

**Re-enablement contract:** if Tier 3 is ever enabled, this ADR is void and
dual-control (identity principals + two-approver gate records + OOB channels)
is a hard prerequisite, recorded as a new ADR before any code lands.

## Consequences

- C12 moves from accepted-residual to verified-green with the wording
  "Tier-3 disabled and not schedulable; dual-control deliberately not built
  (ADR-029, re-enablement contract above)".
- The `identity` OS-user modeling work is dropped from WS5 (it had no other
  consumer); if E.4 keyring ever needs principal identity, that is a
  separate, smaller surface.
- Existing fuse coverage is sufficient: `policy_test.go` (Tier-3 deny),
  `schedule_test.go` (not schedulable), and the `fb_db_drop` stub refusal.
