# Changelog

Versions follow ADR-038 (`YYYY.N.N`, own line). Older builds were `dev`.

## [Unreleased]

## [2027.2.20] - 2026-11-04

First versioned release; HQbird 2027 setups and fbagent install it by default.

- `fbmcp version` prints `{"version":"..."}` and exits (no config, no
  kernel); fbagent reads it for product updates.
- The operator CLI runs as `fbmcp ctl <command>`; `fbmcpctl` is a link or
  a thin wrapper.
- `fbmcp ctl hqbird-install`: one idempotent command for HQbird servers.
  It finds the Firebird lines (fbagent services, `--agent-config`,
  `--lines-stdin`), writes `fbmcp.yaml` entries and secrets for them,
  keeps the operator's own entries, sets up the service (Windows
  `NT SERVICE\fbmcp`; Linux `fbmcp.service` as `firebird` with sudoers for
  the Firebird units), the posture marker, and remote mode with
  `--allow-from` (self-signed certificate, firewall rule, `doctor --remote`).
  API keys stay a separate step (`fbmcp ctl apikey new`). See
  `docs/hqbird.md`.
- `apikey new --config fbmcp.yaml` adds the identity to the config and
  stores the key in its `secrets.dir`; before, it only printed the entry.
- Linux unit: `ReadWritePaths` covers `/opt/hqbird` (HQbird 2027 agent
  folders).
- `scripts/make-release.ps1`: build, Authenticode on the signing machine,
  tag.
