# ADR-038 — Versions of fbmcp in HQbird

Status: accepted (2026-11-04) · Replaces: the Versioning paragraph of ADR-025

## Context

HQbird 2027 setups and fbagent install fbmcp by default, one per server
(`docs/hqbird.md`). goafts' release manager and fbagent's product update
compare versions of every HQbird product. They need one version line for
fbmcp, a version the binary itself reports, and releases that are signed
like the other Windows exe.

## Decision

**Version line.** `YYYY.N.N`, its own line (not the HQbird kit version, not
SemVer `0.x`). The first release is `2027.2.20`. Each release is above the
last `v*` tag; `scripts/make-release.ps1` refuses anything else.

**Source of truth.** The `VERSION` file. The build sets it with
`-ldflags "-X main.version=<ver>"`; a build without it reports `dev`.

**The binary reports it.** `fbmcp version` prints one JSON line,
`{"version":"<ver>"}`, and exits. It never reads a config and never starts
the kernel (fbagent `VersionFromCLI`, 2 s timeout). The MCP
`serverInfo.version` and `fb_ping` show the same value.

**One binary.** The operator CLI runs as `fbmcp ctl <command>`; `fbmcpctl`
is a link (Linux) or a thin wrapper build, so a server has one file to
update.

**Release.** `scripts/make-release.ps1 -Stage build` (vet, tests, three
platforms: windows-amd64, linux-amd64, linux-arm64) → Authenticode of the
exe on the signing machine → `-Stage finish` (checks the signature against
`scripts/authenticode.pin`, commits `VERSION` and `CHANGELOG.md`, tags
`v<ver>`, pushes). Publishing to the zones is goafts' job (zone signatures),
not this repo's.

**Compatibility.** A config written by an older fbmcp loads in a newer one.
A tool whose semantics break ships as `<tool>_v2` (unchanged from ADR-025).

## Consequences

- fbagent and goafts read the version with `fbmcp version`; an fbmcp older
  than 2027.2.20 has no such command, so it must never sit next to a config
  that the kernel would load (`docs/hqbird.md`, Layout).
- ADR-025 keeps the support window, reporting and embargo rules.
