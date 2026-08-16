# ADR-002 — Minimum Firebird version (D1)

Status: accepted (2026-08-16) · Fed by: P0.1

## Context
v3 table targets FB 3.0–5.x with 2.5 best-effort. Choice affects system
privileges (P4.3), EXPLAIN plans (P2.4), MON$ richness.

## Decision
**Minimum Firebird 3.0** for the supported feature set; **FB 2.5 is
best-effort, read-only tools only** (connectivity + MON$ + RO enforcement all
spike-verified on 2.5). Mutation/admin tools declare `min FB 3.0` in version
gating (P1.5).

## Consequences
- P4.3 (users/roles/mappings) and EXPLAIN-based analysis assume 3.0+.
- 2.5 databases still get Tier-0 insight (sessions, transactions, stats) —
  verified working.
- No dialect-1-specific handling in v1 (flag-and-lenient per requirements doc).
