# ADR-025 — Release versioning and security disclosure

Status: accepted (2026-08-17) · Fed by: P6.1 T8 / phase6_plan_v2.md

## Context

v1.0 needs a support window, a public reporting path, and an embargo rule.
External security review is **not** a 1.0 gate (plan §12). Internal
threat-model walkthrough is.

## Decision

**Versioning.** Replaced by [ADR-038](ADR-038-fbmcp-versions-in-hqbird.md)
(2026-11-04): own `YYYY.N.N` line, first release `2027.2.20`. Breaking tool
semantics still ship as `<tool>_v2` (main plan Appendix B).

**Support window.** The latest release is the supported one. Security
fixes go into the next release; there are no backports.

**Reporting.** [`SECURITY.md`](../../SECURITY.md). Preferred: private email
to the address listed there. No public GitHub issue for unreleased vulns.

**Embargo.** 90 days from private report, or until a patched release is
available, whichever is sooner. Coordinated disclosure after that.

**External review.** Out of 1.0. An internal walkthrough of
[`threat-model.md`](../findings/threat-model.md) plus this claims register
is the 1.0 review.

## Consequences

`SECURITY.md` is a 1.0 artifact. OSS-Fuzz and paid external review stay on
the post-1.0 backlog.
