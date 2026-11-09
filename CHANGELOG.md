# Changelog

Versions follow ADR-038 (`YYYY.N.N`, own line). Older builds were `dev`.

## [Unreleased]

- Docs: runbook, remote, hqbird, claude-desktop and tool-reference use the
  `--config` forms and say which tools the client does not see. New: the
  full guide in Portuguese (Brazil), `docs/runbook-pt-br.md`.

## [2027.2.21] - 2026-11-09

Operator CLI and the tool list for an HQbird install.

- `fbmcp ctl` prints one line per command. Config is `--config`, `$FBMCP_CONFIG`, `./fbmcp.yaml`, the file next to the program, or the HQbird default.
- `approve` asks before it runs, can `--deny` with a reason, and does not create a state folder from a typo.
- `doctor` logs in to each database with the read-only user and the admin user, and every FAIL ends with a Fix line.
- Gated tools check arguments before the gate. A missing or string `on` no longer turns Forced Writes off.
- The tool list hides tools the caller cannot use. `fb_demo_write` is registered only when `FBMCP_DEMO_TOOLS=1`.
- README and the en/ru/pt quickstart describe the HQbird path, the new ctl forms, and the read-only user.

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
