# Phase 6 progress — hardening (P6.0 + P6.1 core)

Date: 2026-08-17.

## P6.0

[`claims-register.md`](../findings/claims-register.md) exists with C1–C23 live wording.

## P6.1 landed

- ADR-025 / ADR-026, [`SECURITY.md`](../../SECURITY.md)
- CI: [`.github/workflows/fbmcp-security.yml`](../../../.github/workflows/fbmcp-security.yml) (govulncheck, `go test`, three-arch `-trimpath`, SBOM, fuzz time-cap)
- C9: `internal/confine` + `CleanOrphans` refuse + config `..`/UNC reject
- C16: `MinFB` fail-closed in `policy.EvaluateMeta`
- C8: `drainUntil` bounded verbose-channel drain
- C2/C19/C22/C20/C23 tests under `cmd/fbmcp`
- P5.1 routed: `fb_confirm` / `fb_schedule_create` / `fb_demo_write` use `identity.Caller`

## Not in this slice

- P6.2 live `nightly_verify` fire + soak (C7a/C7b kill harness)
- P6.3 operator runbook + walkthrough
- Rate-limit / default Origin allowlist (named C11 residual)
