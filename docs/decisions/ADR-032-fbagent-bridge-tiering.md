# ADR-032: fbagent bridge — tiering, dynamic job tiers, window keying

Status: accepted (2026-09-13, Phase 9 / A3–A4)

## Context

The bridge proxies an external actor that can trigger work (36-job
scheduler), change a running service (engine control, update apply) or
reload the agent's config. fbmcp's policy engine tiers tools statically
(`internal/policy.SystemTools`), requires a maintenance window for Tier ≥ 2
keyed by a **registry database id** (`st.InWindow(db)`), and has an
established dynamic-tier precedent (`fb_write` classifies statements and
calls `EvaluateMeta`).

## Decision

1. **Tier map.** Tier 0: the ten read tools (including
   `fb_agent_dbaccess_check`, a POST that only evaluates the agent's
   policy — the engine is verb-agnostic and the call mutates nothing).
   Tier 1: `fb_agent_job_run` (base), `fb_agent_config_reload`,
   `fb_agent_pprof`. Tier 2: `fb_agent_restart`/`stop`/`start`/
   `fb_agent_update_apply` — the same bar as `fb_service_start`/`stop`/`restart`.
2. **Dynamic tier for `fb_agent_job_run`.** Base Tier 1; the hot-list
   families `restore`, `sql_script`, `vbackup`, `grpvbackup`,
   `nbackup_dump`, `grpnbackup_dump` classify Tier 2 via the `fb_write`
   `EvaluateMeta` path (`requestGatedMeta`), extensible per agent with
   `tier2_jobs`. The hot list is hard-coded where it drives the tier;
   job *names* are otherwise soft-validated (warn-and-passthrough — the
   agent's 404 is authoritative; the API has no catalog endpoint).
3. **`db` is the window key.** Tier-2 bridge tools require the `db`
   argument (a registry database id), exactly like `fb_service_restart`:
   windows are database-keyed and `fb_window_open` refuses non-registry
   ids, so an agent-only argument would make Tier 2 permanently
   un-confirmable. On a multi-instance host this proves "downtime
   authorized on this host", not per-instance intent — accepted at the
   same trust level as `fb_service_restart`.
4. **Double governance, stated.** The fbmcp gate approves the *request*;
   execution happens inside the agent under the agent's own scheduler and
   policy. fbmcp's audit records who requested what; the agent's logs
   record execution.

## Consequences

- Identity `MaxTier` ceilings apply unchanged (Tier 0 still evaluates —
  the bridge never skips `eng.Evaluate`, unlike the older p2 read tools).
- The dynamic-tier classification is unit-tested (`tier2_jobs` override)
  and live-verified (Tier-1 round trip green; Tier-2 deny/pending paths
  green against the lab).
