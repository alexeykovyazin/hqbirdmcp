# ADR-037: fbagent config writes via the candidate file + reload

Status: accepted (2026-10-29, Phase 11 / D5b)

## Context

Operators want to change backup/restore/sweep job settings of the
co-located fbagent from the MCP surface (`fb_agent_config_set`). Survey
facts:

- The agent Local API has **no route that accepts a config document**.
  `POST /v1/config/reload` re-reads `agent_config.json` and first promotes a
  waiting `agent_config_pending*.json` candidate
  (fbagent `internal/config/candidate.go`, `config_reload_bridge.go`).
- HQ Monitor's config editor uses exactly this path: it writes
  `agent_config_pending_<UTC>.json` beside the live file (whole document,
  unknown keys kept, no BOM, atomic) and calls the reload. HQ Monitor adds
  only browser auth (cookie/CSRF); proxying through it brings nothing.
- The agent promotes a candidate only after it has been unchanged for 2 s
  (`CandidateSettleTime`); it validates (structure, preflight, cron,
  nbackup), backs the live file up, installs, activates and rolls back if
  activation fails. A rejected candidate is archived as
  `agent_config_rejected_*`; the live file stays as it was.
- The config snapshot artifact is redacted with `***`. A candidate built
  from it would write `***` over every credential, so the snapshot cannot
  be a base document.
- On this host the live `agent_config.json` is ACL-locked for the
  interactive account.

## Decision

1. fbmcp writes agent config **only** through the candidate + reload path.
   A direct overwrite of `agent_config.json` is rejected: it skips the
   agent's validation, backup and rollback.
2. The base document is the **live file** at `agents[].config_path`
   (recorded by `fb_agent_register` from discovery). When it is unreadable,
   the tool refuses with the ACL instruction. Granting the fbmcp account
   read on the file (and create on its directory) is an explicit operator
   decision: that file contains the agent token and Firebird credentials.
3. The patch surface is narrow (`internal/agentcfg`): one family
   (vbackup, nbackup, restore, sweep, grp_sweep, gfix_check, index_recalc,
   scheduler), optionally one job by `id` or one scheduler task by name
   (`enabled`, `schedule`, `timeout_seconds`). Only existing settings, same
   JSON type. Refused: credential keys, `id`/`name`, `command`/`args`/binary
   paths, replacing `jobs`/`tasks` lists, `null`, the `***` placeholder,
   and enabling a block that carries a command. `sql_script` is excluded
   (its jobs are code).
4. Tier 2, out-of-band. The impact statement shows the exact diff. The
   approval is bound to the live file's sha256; the executor refuses when
   the file changed after the request, or when another candidate is
   already waiting (ours would supersede someone else's edit).
5. Execution: write the candidate atomically (temp name outside the agent's
   glob, then rename), wait 2.5 s, `POST /v1/config/reload`, report
   `promoted_from` and `restart_required_for`.

## Consequences

- Same safety net as HQ Monitor: the agent remains the only component that
  installs its config.
- Until the operator grants read access, `fb_agent_config_set` returns the
  ACL message and `fb_agent_backup_config` (snapshot, read-only) is the
  only config view.
- fbmcp holds the agent's secrets in memory while it composes a candidate;
  they never reach the diff, the impact text or the audit log.
