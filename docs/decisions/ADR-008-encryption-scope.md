# ADR-008 — Encryption scope (D9)

Status: accepted (2026-08-16) · Fed by: P0.4

## Decision
- **WireCrypt** = a config parameter surface (op 39) in the config editor
  (P4.6): read/report + template-edit with restart flags. Nothing more.
- **DbCrypt + key management deferred to v2.** Key custody would become a
  crown-jewel asset (threat T-06/T-11 intersection) with no mature Go-side
  key-store story; v1 gains no defensible guarantee from shipping it.

## Consequences
Threat model residual-risk list notes encryption-at-rest as out of scope;
MON$DATABASE.MON$CRYPT_STATE may still be *reported* (read-only visibility).
