# Status (v1.0 run-up, phase 8 + 9)

Moved here from the README (UX review 2026-11-05, W1.7). Start: [README](../README.md).

Plans: [`../phase8_plan.md`](../../phase8_plan.md) and
[`../phase9_plan.md`](../../phase9_plan.md) (operational),
[`../improvement_plan.md`](../../improvement_plan.md) (workstreams).
Claims register: [`docs/findings/claims-register.md`](findings/claims-register.md)
(23 verified-green, 1 accepted-residual, 0 open).

**Done** (development phase 8D complete):

- Core kernel: single-process MCP server (stdio + optional remote TLS/HTTP
  with API-key identities), live `fbmcp.yaml` reload without restarts,
  registry ids instead of connection strings.
- Confirmation gate: tiered policy engine with identity ceilings,
  maintenance windows and verified-backup preconditions; in-band
  `fb_confirm` for Tier 1; out-of-band only for Tier ≥ 2
  (`fbmcp-tray` popup / `fbmcpctl approve`); Tier 3 disabled by design
  (ADR-029); `mode: preview` impact statements on every gated tool.
- Monitoring & diagnostics: engine facts, sessions, transactions, gstat,
  query plan analysis + index advice, schema/data diff, capacity trends,
  lightweight monitoring, engine traces.
- Data safety: gbak backup, incremental nbackup, test-restore verification
  catalog, restore-replace with `.pre-restore` snapshot, retention with
  dry-run default, `nightly_verify` scheduled workflow.
- SQL surface: `fb_write` classified scripts (per-statement tiers,
  DML-atomic vs DDL-per-statement execution), HQBird/FB5 extensions
  (materialized views, `INDEX CONCURRENTLY`), migrations with manifest-bound
  batch confirmation (ADR-030).
- Security & scheduler: users/roles/grants with lockout guards, session
  kill, named trace templates; durable pre-authorized schedule grants
  (ADR-023).
- Hardening: chaos kill-harness (13 fault-injection sites, 9+ scenarios),
  audit chain with `fbmcpctl verify`/`repair`, structured error envelopes,
  OS-keyring secret fallback, per-identity rate limits, fuzz corpus,
  `fbmcpctl gate` report. CI green.

**Phase 9 — fbagent bridge (landed 2026-09-13, live-verified):**

- 17 agent-bridge tools proxy the co-located fbagent Local API under the
  same tiered policy / confirmation gate / audit discipline: Tier-0 reads
  of the agent's artifact hub (LWM, gstat-full, per-database metrics,
  index stats, traces, deadlock reports, metadata, backup snapshots), the
  agent's 36-job scheduler (`fb_agent_job_run`, dynamic Tier 1→2 for the
  heavy families), config reload, pprof, and the frozen engine-control
  contract plus update apply (Tier 2, maintenance window on `db`).
  Design: ADR-031/032/033; ops guide:
  [`docs/agent-bridge.md`](agent-bridge.md).
- Live-lab suite green against the real agent on this host (port 13055,
  instance `fb-master-3055`): full Tier-1 governed round trip, artifact
  fixtures, error mapping, conditional negative fixtures; extended and
  engine tiers opt-in via env gates. Tool surface at the phase-9
  landing: 84 (42 read-only / 30 Tier 1 / 11 Tier 2 / 1 Tier 3) —
  now 85/43 after phase 10 (see the count line at the top).

**In progress** (manual validation phase 8M, running unattended):

- **M2 soak week** — live on the dev kernel since 2026-08-30: 6
  `nightly_verify` grants, hourly health samples into
  [`docs/findings/soak-report.md`](findings/soak-report.md); 7-day window.
- **M3 nightly chaos loop** — `fbmcp-chaos-nightly` (daily 02:30, N=50),
  results in [`docs/findings/chaos-log.md`](findings/chaos-log.md);
  acceptance is ≥ 5 consecutive clean nights.
- **M8 client smoke** — real-task usage of the deployed tools.

**Planned** (remaining to the v1.0 gate):

- **M1** runbook walkthrough on a fresh Windows host
- **M4** Linux matrix execution (FB 2.5/3.0/4.0/5.0 via compose)
- **M5** disk-full injection (VHD/quota) behavior documentation
- **M6** materialized-view refresh live verification under concurrent reads
- **M7** lwmonitoring levels-2–4 upstream report to HQBird
- **M9** go/no-go gate review against `phase6_plan_v2.md` §9, release
  assembly and the v1.0 tag
