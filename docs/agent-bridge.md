# fbagent bridge (phase 9)

fbmcp can proxy the **fbagent** Local API — the loopback HTTP surface of the
co-located HQBird agent service (documented in the fbagent repo,
`docs/API_REFERENCE.md`, release 2.21.0 at the time of writing). This gives
the MCP client governed access to data and operations fbmcp cannot reach
otherwise:

- the **artifact hub**: LWM summaries/raw samples, backup status & size
  snapshots, per-database `metrics` / `index_stats` / `advanced_stats`
  JSONL, `gstat_full` logs, processed traces, perf reports, deadlock
  reports, metadata bundles, disk & filesystem-health records, redacted
  agent config snapshots;
- the agent's **36-job scheduler** (`sweep`, `collect_metadata`,
  `process_traces`, `find_deadlocks`, `sql_script`, …);
- the **frozen engine-control contract** (stop → start → TCP readiness with
  async operations and coalescing) and the Firebird **update** pipeline.

The bridge is **additive**: native `fb_*` tools stay; where the two overlap,
the tables below say which side to prefer.

## Configuration (`fbmcp.yaml`)

```yaml
agents:
  - id: fb1
    base_url: http://127.0.0.1:13050   # loopback enforced (see Security)
    token_env: FBMCP_AGENT_TOKEN       # env var NAME; value resolved env-first, keyring fallback
    instance_id: fb1                   # used in /v1/instances/{id} paths; default "fb1"
    timeout_seconds: 130               # poll budget; MUST exceed the agent's restart_timeout_seconds (120)
    tier2_jobs: []                     # extra job names classified Tier 2 by fb_agent_job_run
    allow_non_loopback: false          # keep false (see Security)
```

- The bridge is **disabled** unless at least one agent is configured
  (config validation passes with an empty `agents:` list).
- Exactly one agent → every tool targets it; several → pass `agent: <id>`.
- Token: `FBMCP_AGENT_TOKEN=<the agent's local_api.token>` in the
  environment, or `fbmcpctl secret set FBMCP_AGENT_TOKEN` (OS keyring).
  Never put the token value in the YAML — only the env-var **name**.

## Tool inventory

Per-tool reference (args, tiers, confirmation channels):
[tool-reference.md](tool-reference.md) → section *fbagent bridge*. Summary:

| Tier | Tools |
|---|---|
| 0 (10) | `fb_agent_instances`, `fb_agent_status`, `fb_agent_databases`, `fb_agent_artifact_list`, `fb_agent_artifact_get`, `fb_agent_job_status`, `fb_agent_operation`, `fb_agent_dbaccess`, `fb_agent_dbaccess_check`, `fb_agent_update_status` |
| 1 (3) | `fb_agent_job_run` (dynamic), `fb_agent_config_reload`, `fb_agent_pprof` |
| 2 (4) | `fb_agent_restart`, `fb_agent_stop`, `fb_agent_start`, `fb_agent_update_apply` |

Async agent operations (202 → `operation_id`) are polled internally
(~2 s ticks, bounded by `timeout_seconds`, progress via the job runner);
if the budget runs out the tool returns the `operation_id` and the client
can resume later with Tier-0 `fb_agent_operation`.

## Governance

- Tier 0 needs no confirmation; Tier 1 confirms in-band
  (`fb_confirm` + token); Tier 2 requires an open maintenance window and
  **out-of-band** approval (`fbmcpctl approve` / tray), identical to
  `fb_service_restart`.
- The **`db` argument** (a registry database id) is the audit key on every
  bridge tool and the **window key** for Tier-2 tools: open the window
  against a co-located registry database (`fb_window_open`), then pass the
  same `db` to the bridge tool. On multi-instance hosts this proves
  "downtime authorized on this host", not per-instance intent — the same
  trust level as `fb_service_restart`.
- `fb_agent_job_run` is Tier 1 with a **dynamic tier** (same mechanism as
  `fb_write`): the hot-list families `restore`, `sql_script`, `vbackup`,
  `grpvbackup`, `nbackup_dump`, `grpnbackup_dump` (plus per-agent
  `tier2_jobs`) classify as Tier 2.
- **Double governance:** the fbmcp gate approves the *request*; execution
  happens inside the agent under the agent's own scheduler and policy.
  fbmcp's audit records who requested what; the agent's logs record the
  execution. Do not assume fbmcp policy constrains agent-side execution.
- `requested_by` on every run/engine/update request is filled with the
  confirming identity — the agent side shows who drove it.

## Which side to use (overlap matrix)

| Want | Use |
|---|---|
| Agent artifacts (LWM, gstat-full, per-db metrics/index stats, traces, deadlocks, metadata) | bridge only |
| Agent scheduler jobs (36 families) | `fb_agent_job_run` |
| Engine service control | `fb_service_start`/`stop`/`restart` for direct SCM control of a registry instance; `fb_agent_restart`/`stop`/`start` when the agent owns supervision (frozen contract, coalescing) |
| Sweeps/backups/SQL on a registry database | native `fb_*` tools — governed end-to-end by fbmcp |

## Security

- **Loopback only.** The agent rejects non-loopback binds, and fbmcp
  config validation enforces a loopback `base_url` unless
  `allow_non_loopback: true` is set explicitly (not recommended).
- **SSRF stance:** `base_url` comes only from `fbmcp.yaml`; no tool accepts
  a host/URL argument. Artifact scopes/paths are validated client-side
  (no separators or `..` in database folder keys), mirroring the agent's
  own pathguard.
- The Bearer token never appears in config files, tool output, or audit
  detail (audit scrubbing drops `token`-keyed fields).
- The chaos harness strips `agents:` from its derived config — a killed
  kernel can never resurrect into a live-agent call.

## Error mapping

The agent's error envelope maps onto fbmcp's structured error codes
(`errdoc`); the agent's own code and message ride along in the text:

| Agent response | fbmcp code |
|---|---|
| transport failure | `agent-unreachable` |
| 401 | `agent-auth` (check `token_env`) |
| 404 / `not_found` | `agent-not-found` |
| 400/405 / `denied` | `agent-remote-denied` |
| 503 / `unavailable` | `agent-unavailable` (feature not wired on the agent) |
| 5xx / `service_error` | `agent-remote-error` |
| operation `error.code=timeout` / poll deadline | `agent-timeout` (poll later with `fb_agent_operation`) |
| 413 / `payload_too_large` | `agent-payload-too-large` (retry with `tail=`) |

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| "no agent configured" | no `agents:` entry in `fbmcp.yaml` (bridge disabled by default) |
| `agent-auth` on every call | token mismatch: env/keyring value ≠ agent's `local_api.token` |
| `agent-unreachable` | agent not running, `local_api.enabled: false`, or wrong port (default listen is `127.0.0.1:(10000 + firebird.port)` → 13050 for 3050) |
| `agent-unavailable` on artifacts | the agent's `data_path`/collector has not produced the artifact yet |
| Tier-2 tool always "no open maintenance window" | the window must be opened for the **same `db`** you pass to the tool |
| `agent-timeout` on restarts | `agents[].timeout_seconds` ≤ the agent's `restart_timeout_seconds`; raise it |

## Local lab (this dev host)

A real agent is deployed at `C:\HQbirdbagent-p3055` with the Local API on
`127.0.0.1:13055` and **instance_id `fb-master-3055`** (not the `fb1`
default — every instance-scoped call 404s without it). It supervises the
FB5 instance (`FirebirdServerHQBirdInstanceFB5`, port 3055) that the soak
kernel attaches to, so Tier-2 engine tools are opt-in and coordinated.

Run the live suites (`go test ./...` runs them green-skipped without env):

```bash
FBMCP_AGENT_TOKEN=<local_api.token> FBMCP_AGENT_LIVE=1   go test ./cmd/fbmcp -run AgentLive -v          # full Tier A matrix
FBMCP_AGENT_TOKEN=<...> FBMCP_AGENT_LIVE=1   go test ./internal/agentapi -run TestLive -v   # client-level, GET-only
```

Tier B adds `FBMCP_AGENT_LIVE_EXTENDED=1` (CPU-sampled pprof, lwm job
triggers); Tier C adds `FBMCP_AGENT_LIVE_ENGINE=yes-restart-fb5`
(**restarts/stops the FB5 service** — pause `fbmcp-soak-week` first; the
suite skips itself when the agent reports an update in progress). Suite
design, fixtures, and safety model: `phase9_live_test_plan.md` (AIDBA root).

## Tests

- `internal/agentapi`: httptest fixtures for the full error mapping, the
  202 → poll lifecycle, artifact guards, and coalescing shapes
  (`go test ./internal/agentapi/`).
- `cmd/fbmcp` (`TestAgent*`): config resolution, gate paths (Tier-1 token,
  Tier-2 window denial, hot-list escalation, `tier2_jobs` override),
  executor polling against the stub, and one MCP end-to-end call.
- Live: `FBMCP_AGENT_LIVE=1` plus `FBMCP_AGENT_LIVE_URL` /
  `FBMCP_AGENT_LIVE_TOKEN_ENV` runs the read suite against a real agent
  (phase 9M).
