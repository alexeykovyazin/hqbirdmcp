# fbmcp — Firebird DBA MCP Server

Work-in-progress implementation per the plans in the parent directory
(`mcp_implementation_plan.md`, `phase0_plan.md` … `phase6_plan.md`).

**Current status: Phase 0 (spikes & decisions).**

Everything under `spikes/` is throwaway/reference code — it intentionally cuts
corners (hardcoded ports, `masterkey` passwords) that production code must not.

## Environment (this dev host)

- Go 1.24.5, Windows amd64
- Local Firebird instances (HQbird installs, all running as services):
  - FB 2.5 — port 3052 (`C:\HQbird\Firebird25`)
  - FB 3.0 — port 3053 (`C:\HQbird\Firebird30`)
  - FB 4.0 — port 3054 (`C:\HQbird\Firebird40`)
  - FB 5.0 — port 3055 (`C:\HQbird\Firebird50`)
- Linux side: Docker Desktop present but not running; FB-version coverage on
  Linux is deferred until containers are brought up (plan E2 tracks this).

Spikes run only against dedicated spike databases created under
`C:\HQbirdData\output\fbmcp-spike\` — never against real databases.
