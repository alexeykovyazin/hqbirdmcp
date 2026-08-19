# ADR-026 — Security CI and fuzzing (not bitwise-identical binaries)

Status: accepted (2026-08-17) · Fed by: P6.1 T1 / phase6_plan_v2.md

## Context

There was no CI. Phase 6 must **create** the lane. Go PE/ELF output is not
stable bit-for-bit across machines without a hermetic builder we do not have.

## Decision

**Lane.** GitHub Actions workflow [`.github/workflows/fbmcp-security.yml`](../../../.github/workflows/fbmcp-security.yml)
at the AIDBA repo root, `working-directory: fbmcp`.

**Blocking on every PR / push:**

1. `go mod verify`
2. `go test ./...` (C1 skips without Firebird; see claims register)
3. `govulncheck ./...`
4. Three-arch `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"` (windows/amd64, linux/amd64, linux/arm64) plus SHA-256 checksums
5. SBOM: `go version -m` on each `dist/` binary, committed as a CI artifact (not a golden bit-identical file)

**Fuzz.** Self-hosted `go test -fuzz` with a time cap in CI:

- `FuzzParse` / `FuzzSplit` (`internal/fbparse`)
- `FuzzMatches` (`internal/schedule`)
- `FuzzPlan` (`internal/retention`)
- `FuzzScript` (`internal/classify`)
- `FuzzBearer` (`internal/transport`)

OSS-Fuzz is **out of 1.0**.

**Reproducible recipe (honest).** Pinned module toolchain (`go 1.25.0` in `go.mod`), `CGO_ENABLED=0`, `-trimpath`, checksums. **Do not claim** bitwise-identical binaries across hosts. Residual named here and in the claims register.

**Firebird on CI.** Default runners have no Firebird. Fuse #1 and other live-engine tests `t.Skip` with a log. The Windows HQbird host sets `FBMCP_REQUIRE_FIREBIRD=1` so those skips fail.

## Consequences

Supply-chain evidence is checksums + `go version -m` SBOM, not bit-identity.
A clean-host rebuild using this recipe should match checksums **on the same
OS/arch/toolchain**; mismatches across hosts are investigated, not treated as
a release blocker by themselves.
