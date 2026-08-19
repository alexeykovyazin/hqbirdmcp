# Threat model — fbmcp (v1) — P0.4

Date: 2026-08-16. Scope: main plan §3–§5 architecture. Inputs: P0.1/P0.2/P0.3
spike findings (esp. trusted-auth on Windows, SEP-2322 elicitation semantics,
os/exec output-loss pitfall).

## 1. Assets

A1 DB data · A2 DB credentials (SYSDBA/per-DB users) · A3 backups ·
A4 firebird.conf/databases.conf · A5 audit log + kernel state (pending
actions, tokens, job store) · A6 host service control (Firebird service) ·
A7 MCP transport (stdio local / HTTP remote) · A8 confirmation surface (CLI marker-file; approval page not in v1) · A9 server binary + config/registry files.

## 2. Actors & trust boundaries

- **LLM/MCP client** — *untrusted initiator*: may be manipulated (prompt
  injection) or adversarial; controls tool calls and all in-band arguments.
- **Authenticated API identity** (remote mode) — semi-trusted, scoped.
- **Local operator** — trusted human (out-of-band approvals, CLI).
- **OS peer / same-account process** — on Windows, any process of the service
  account can admin local Firebird via trusted auth (spike finding) → the
  service account is the real local privilege boundary.
- **Malicious payload in DB content / utility output** — untrusted text that
  the server parses or relays to the LLM (indirect prompt injection source).

Boundaries: (B1) MCP client → façade; (B2) façade → kernel (policy/gate);
(B3) kernel → pools/executor; (B4) server → OS (subprocesses, files, service
control); (B5) approval surface → local operator; (B6) server ↔ Firebird
engine.

## 3. Data-flow (numbered)

F1 tool call (args from LLM) → façade; F2 façade → policy engine (identity,
tool, args); F3 policy → pending-action store + token issue; F4 confirmation
(client elicitation / fb_confirm in-band / approval page or CLI out-of-band);
F5 gate pass → job manager; F6 job → admin pool SQL or admin executor
subprocess/Services API; F7 executor → files (backups, trace, work dirs);
F8 results/impact statements → LLM; F9 audit writes → hash-chained log;
F10 config-editor → firebird.conf writes; F11 scheduler (P5.3) →
pre-authorized jobs; F12 notification delivery.

## 4. Threat register (STRIDE)

| ID | Threat (class) | Flow | Mitigation (part) | Test |
|---|---|---|---|---|
| T-01 | In-band confirmation spoofing: model relays/derives its own token (Spoofing/Elevation) | F3–F4 | Channel policy: Tier≥2 out-of-band only (§5.5); token bound to action+db+identity, single-use, TTL | Fuse #7; token replay test (P1.6) |
| T-02 | Prompt injection → destructive tool chain (Elevation) | F1–F2 | Tier/impact metadata from v3 table; preconditions; impact statements from table data not model text; Tier-3 disabled | Fuse #2; P6.1 injection battery |
| T-03 | Concurrent stdio server instances dual-write kernel state (Tampering/DoS) | F5, F9 | D8 lock file, fail-fast second instance; single-writer state store | Fuse #6 |
| T-04 | Privilege escalation via host ops / config editor (Elevation) | F10, F6 | §4.8 narrow elevation; allowlisted params; `.prev` + journal; restart-required flags; instance-scope = Tier 2 | P4.6 param allowlist tests; P6.1 |
| T-05 | Path traversal in backup/restore/trace/clone args (Elevation/Info) | F7 | Registry IDs only; allowlisted dirs; structural rejection of absolute/`..` paths | P6.1 traversal fuzz |
| T-06 | Credential leakage: argv, env, logs, job output (Info) | F6, F9 | env-only (proven); scrubbing in audit/log writers; never in results | secret-scrub test (P1.4); P6.1 |
| T-07 | Audit tampering (Tampering/Repudiation) | F9 | hash-chained JSONL; integrity-check tool; file ACLs | tamper test (P1.4) |
| T-08 | DoS via unbounded jobs/output (DoS) | F5–F7 | statement/size/time budgets; output caps (harness proven); queue limits; per-DB mutex | budget tests (P1.7); P6.2 |
| T-09 | Token theft & replay (Spoofing) | F3–F4 | single-use, TTL, identity-bound; OOB channel is localhost-only | P1.6 tests |
| T-10 | Supply chain: driver, SDK (Tampering) | build | pinned versions, go.sum, govulncheck, SBOM, reproducible builds | P6.1 CI |
| T-11 | Same-account local process can admin DBs via Windows trusted auth (Elevation) — *spike finding* | B4/B6 | dedicated least-privilege service account is the boundary; no broader host rights; document as residual on hosts where account is shared | posture checklist (P5.2); ADR-007 note |
| T-12 | Indirect prompt injection via DB content/utility output relayed to LLM (Elevation) | F8 | advisory-only framing; mutation requires independent gate; no auto-chaining from advisory text to execution | import-boundary test (P2.5); P6.1 |
| T-13 | Approval page takeover / CSRF on localhost surface (Spoofing) | F5(OOB) | localhost bind only; per-request approval nonce shown on page+CLI; no remote exposure; P5.1 TLS/auth for remote mode | P1.6 tests; P6.1 |
| T-14 | Scheduler pre-authorization abuse (stale approvals re-run) (Elevation) | F11 | pre-auth bound to job spec hash + window; re-confirmation on drift (ADR-023 territory, Phase 5) | P5.3 tests |
| T-15 | Error-message oracle: connection strings/paths in errors (Info) | F6, F8 | typed error taxonomy; registry IDs in messages; scrubbing | P6.1 |
| T-16 | Orphaned subprocess after crash keeps locks/credentials (Tampering/DoS) | F6 | PID tracking + stale-lock cleanup (P1.7) | P6.2 kill tests |
| T-17 | RO-pool bypass via classifier bug (Elevation) | F6(read) | engine-enforced RO TPB (spike-proven on 2.5–5.0) + RO users | Fuse #1 |
| T-18 | Elicitation spoofing: client UI approval ≠ operator intent (Spoofing) — *SEP-2322 finding* | F4 | elicitation accepted for Tier 1 only, documented client requirement; Tier≥2 OOB | Fuse #7 |

## 5. Decision confirmations

- **D7 (ADR-007): confirmed** with sharpened wording: Tier 1 = client
  elicitation/approval or in-band token (client-requirement documented);
  Tier 2 = OOB only; Tier 3 = OOB dual control. Elicitation (new round-trip
  semantics included) is Tier-1-equivalent trust — see T-18.
- **D9 (ADR-008): confirmed** — WireCrypt via config parameter only; DbCrypt
  + key management out of v1 (key custody would become the crown-jewel asset
  with no mature Go-side story).

## 6. Residual risks (accepted in v1)

1. Local same-account compromise = DB admin (T-11) — accepted: matches the
   posture of native Firebird tooling; narrowed by dedicated account.
2. Stdio local mode without identity (opt-out) trusts the local OS user.
3. No outbound egress control from server host (backups stay local).
4. 2.5 best-effort only; parsed-utility-output guarantees capped at 3.0+.
5. Single-instance lock file is advisory-locked only against same-path
   configs; misconfigured second data dir could still dual-write (documented
   operator responsibility).

## 7. Phase 5 surface (2026-08-17)

- **Remote (A7):** `listen` is opt-in. Start requires non-localhost bind + TLS
  + ≥1 identity (ADR-022). `/healthz` is liveness only. `/mcp` and `/sse` are
  authenticated. X-Forwarded-For untrusted. No remote approval page; humans
  use `fbmcpctl approve` on the host (SSH).
- **Scheduler (F11 / T-14):** durable `state.Schedule` grant, not a 15-minute
  pending action. Fire path does not call the gate. Arg-hash drift skips the
  run. Tier-3 and `fb_restore_replace` cannot be scheduled (ADR-023).
- **K7 (F12):** local event log + optional HMAC webhook (ADR-024). POST_EVENT
  deferred.

## 8. Phase 6 residuals (P6.1, 2026-08-17)

- Empty Origin allowlist (any Origin) and no HTTP rate-limit / session cap (C11).
- Dual-control for Tier-3 not implemented (`fb_db_drop` stub) (C12).
- Host write to `state.dir` can rewrite schedule `ArgHash` (C14 / T-11).
- D5 env secrets (no OS keyring).
- Non-bitwise-identical builds (ADR-026).
- Windows service Stop does not cancel `runForeground`.
- Linux container matrix and unattended soak: P6.2.

`fb_confirm` now uses `identity.Caller` (P5.1 routed fix) so a remote API-key
cannot confirm a pending action bound to a different identity.

