# Claims register (P6.0 / M6 gate)

Binary M6 status only: **verified-green** | **accepted-residual**. Empty status means not yet closed.

Test column tags: `seed` (exists from Phases 1–5) · `extend` (P6.1 added coverage) · `missing` (still open).

Origin: live tree 2026-08-17 + [`phase6_plan_v2.md`](../../../phase6_plan_v2.md) §4.

Fuse map: #1→C1 · #2→C2 · #3→C7a · #4→C22 · #5→C15 · #6→C4 · #7→C3.

| # | Live wording | Tests | Test tag | M6 status |
|---|---|---|---|---|
| C1 | Read pool engine-refuses writes even if the classifier is wrong | [`pool_test.go`](../../internal/dbpool/pool_test.go) fuse #1; [`classify_test.go`](../../internal/classify/classify_test.go); [`p61_adversarial_test.go`](../../internal/classify/p61_adversarial_test.go) | seed+extend | verified-green |
| C2 | No Tier ≥ 1 **side effect** without confirmation — enumerate `toolMeta` **and invoke** | [`fuse_test.go`](../../cmd/fbmcp/fuse_test.go) enum; [`c2_invoke_test.go`](../../cmd/fbmcp/c2_invoke_test.go) | seed+extend | verified-green |
| C3 | Tier ≥ 2 not confirmable in-band, including remote-session token relay | [`gate_test.go`](../../internal/gate/gate_test.go) fuse #7; [`c3_http_test.go`](../../cmd/fbmcp/c3_http_test.go) | seed+extend | verified-green |
| C4 | Second **kernel** fail-fast (lock); extra piped stdio clients attach, they must not acquire the store | [`instlock_test.go`](../../internal/instlock/instlock_test.go) fuse #6; [`c4_lock_test.go`](../../internal/instlock/c4_lock_test.go); [`attach_test.go`](../../internal/attach/attach_test.go) | seed+extend | verified-green |
| C5 | Audit chain detects edit/delete/reorder | [`audit_test.go`](../../internal/audit/audit_test.go) | seed | verified-green |
| C6 | Facts fail-closed | [`policy_test.go`](../../internal/policy/policy_test.go) | seed | verified-green |
| C7a | Kill in `restore_replace` / `shutdown_window` ⇒ file intact or `.pre-restore`; `CloseDB`; AutoReopen = **database** online | [`killharness_test.go`](../../internal/killharness/killharness_test.go): restore_replace, shutdown, mid-CloseDB scenarios (killpoint fault injection, hard kill, restart invariants); 3× stability loop green on the dev host | extend | verified-green |
| C7b | Kill in `nightly_verify` ⇒ **source DB untouched** | [`killharness_test.go`](../../internal/killharness/killharness_test.go): schedule-driven nightly_verify killed mid-backup, source file sha256 identical, grant survives restart | extend | verified-green |
| C8 | No orphan subprocess; Backup/Trace drain goroutines bounded | [`drain_test.go`](../../internal/backupsvc/drain_test.go) | extend | verified-green |
| C9 | File I/O confined to registry ids + backup/work dirs (incl. `CleanOrphans`) | [`confine_test.go`](../../internal/confine/confine_test.go); [`housekeep_test.go`](../../internal/housekeep/housekeep_test.go); [`config_test.go`](../../internal/config/config_test.go) | extend | verified-green |
| C10 | Secrets never in argv, logs, audit, job output, `events.jsonl`, `fbmcpctl` stdout | [`audit_test.go`](../../internal/audit/audit_test.go); [`c10_secrets_test.go`](../../internal/notify/c10_secrets_test.go); [`adminexec_test.go`](../../internal/adminexec/adminexec_test.go) | seed+extend | verified-green |
| C11 | `/mcp` and `/sse` unauthenticated ⇒ 401; `/healthz` = `ok` only; **E.1 (closed)**: per-identity rate limit + session cap (429s) and default-deny Origin (remote refuses empty allowlist) | [`transport_test.go`](../../internal/transport/transport_test.go); [`c11_residuals_test.go`](../../internal/transport/c11_residuals_test.go) (enforced-behavior tests) | seed+extend | verified-green |
| C12 | Tier-3 disabled and not schedulable; **dual-control = accepted-residual** | [`policy_test.go`](../../internal/policy/policy_test.go); [`schedule_test.go`](../../internal/schedule/schedule_test.go) | seed | accepted-residual (dual-control) |
| C13 | Retention never touches uncataloged or unverified files | [`retention_test.go`](../../internal/retention/retention_test.go) | seed | verified-green |
| C14 | Grants minted only via gated create; lineage audited; hash drift skips. **`state.json` rewrite = accepted-residual (T-11)** | [`schedule_test.go`](../../internal/schedule/schedule_test.go) | seed | accepted-residual (state-dir write) |
| C15 | No phantom tools in `toolMeta`, prompts, runbook | [`playbook_lint_test.go`](../../cmd/fbmcp/playbook_lint_test.go); [`fuse_test.go`](../../cmd/fbmcp/fuse_test.go) | seed | verified-green |
| C16 | Version gating fail-closed | [`policy_test.go`](../../internal/policy/policy_test.go) MinFB | extend | verified-green |
| C17 | Config writes atomic (`.prev`) under kill | [`configedit_test.go`](../../internal/configedit/configedit_test.go) | seed | verified-green |
| C18 | Webhook HMAC + event-id emitted; **test receiver** rejects replay | [`notify_test.go`](../../internal/notify/notify_test.go) | seed | verified-green |
| C19 | OOB marker: unknown id dropped; consumed marker cannot double-dispatch; dual-marker same id once | [`c19_marker_test.go`](../../cmd/fbmcp/c19_marker_test.go) | extend | verified-green |
| C20 | Service start/stop refuse without `posture.verified` | [`posture_test.go`](../../internal/posture/posture_test.go); [`c20_posture_test.go`](../../cmd/fbmcp/c20_posture_test.go) | seed+extend | verified-green |
| C21 | Remote process refuses start unless non-localhost + TLS + ≥1 identity | `TestCheckRemoteRefuse` | seed | verified-green |
| C22 | Fuse #4: policy and audit invoked on the real tool path | [`c22_deadcode_test.go`](../../cmd/fbmcp/c22_deadcode_test.go) | extend | verified-green |
| C23 | P4 guards: last-admin lockout, constraint-backing drop, self-kill, nbackup gap, trace templates | lockout/privs/trace tests; [`c23_reattack_test.go`](../../cmd/fbmcp/c23_reattack_test.go) | seed+extend | verified-green |

## Known residuals (named; not silent)

- (closed 2026-08-25, phase8_plan D4.1/E.1): rate limit + session cap + default-deny Origin are enforced (`limits:` config, 429s, CheckRemote requires a non-empty allowlist in remote mode).
- C12: dual-control for Tier-3 (`fb_db_drop` stub).
- C14: host with `state.dir` write can rewrite `ArgHash` (T-11).
- D5: secrets from env, not OS keyring.
- Bitwise-identical PE/ELF not claimed (ADR-026).
- Windows service Stop does not cancel `runForeground` (P5.2 stub) — P6.2 T6.
- Linux container matrix deferred.
- Soak / unattended week: P6.2 T5, not an M6 veto.

## CI policy (C1)

`go test` on GitHub Actions **skips** fuse #1 when Firebird is absent (`t.Skip` with log). The Windows HQbird host sets `FBMCP_REQUIRE_FIREBIRD=1` so the same skip becomes a **fail**.
