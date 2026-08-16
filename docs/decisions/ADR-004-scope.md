# ADR-004 — Local vs remote management in v1 (D4)

Status: accepted (2026-08-16) · Fed by: P0.2, P0.4

## Decision
**Local-first.** v1 manages Firebird instances reachable from the host
(spike host: four local instances on ports 3052–3055 — the registry models
them as per-database entries with per-DB policy). Remote transport (HTTPS+SSE)
ships hardened in P5.1 with mandatory auth on every entry; remote *database*
targets remain read-only in v1.

## Rationale
Service lifecycle (P3.7) and config editing (P4.6) are inherently local;
backups need filesystem allowlists. Remote mutation adds credential-handling
surface without v1 value.

## Consequences
- The DSN layer (P1.2) already takes host:port per database — local multi-
  instance (ports) and remote-readonly are the same code path.
- P5.1 carries TLS + per-tool/per-DB API-key profiles; no unauthenticated
  entry (fuse from audit lessons).
