# fbmcp — project review & test implementation plan (2026-09-09)

Scope: full codebase review, nightly-chaos lane review, and a prioritized
plan to close the test gaps. Measurements taken from `go test ./...
-coverprofile` (unit suites, live suites skipped — CI runs those separately).

## 1. Project review

### Layout and test posture

Single Go module, four binaries: `cmd/fbmcp` (the MCP kernel), `fbmcpctl`,
`fbmcpsoak`, `fbmcp-tray` (Windows tray approver). ~32k lines of source,
~15k lines of tests across 45 packages. Two live lanes plus ops scripts:

| Lane | Where | What runs |
|---|---|---|
| fbmcp-security | CI on push | gofmt gate, vet, unit tests, govulncheck (pinned v1.7.0), fuzz (60s), dist build |
| fbmcp-matrix | CI nightly 03:43 UTC + PR | per-FB-version (2.5/3.0/4.0/5.0) unit + live suites with skips forced to fail, binary+checksum, reduced chaos loop |
| nightly chaos (M3) | HQBird host, Task Scheduler 02:30 | `packaging/chaos-nightly.ps1`: killharness ×50 against dev instances |
| soak (M2) | HQBird host | `packaging/soak-week.ps1` 7-day unattended run, hourly samples, 6 schedule grants |

Coverage (unit-only, per-package average of function coverage): overall
**45.5%** of statements. Strong: fbparse, classify, config, transport,
reload, schedule, retention, gstat, notify, qlog, audit. Weak (see §3).

The chaos-relevant architecture invariants are well-factored and testable:
audit hash chain (`internal/audit`), atomic state persistence
(`internal/state`, temp+rename), Tier-1 in-band / Tier-2 out-of-band gate
(`internal/gate`, `internal/policy`), durable schedule grants
(`internal/schedule`), workflow reconciliation on restart (AutoReopen), and
deterministic fault injection (`internal/killpoint`, armed only via env —
zero effect in release).

### CI status

Both lanes green as of 2026-09-09 (matrix all four FB versions; security
test/dist/fuzz). Recent fixes landed: firebird docker image tag changes,
FIREBIRD_ROOT_PASSWORD, bash health check, employee sample restore+seed,
in-container gstat, 127.0.0.1 dialing (IPv6 loopback proxy timeouts),
govulncheck pin, actions bump to Node 24 targets.

## 2. Nightly chaos review (M3)

### What the harness covers today

`internal/killharness` boots the real server binary on an isolated state
dir, drives it over the attach socket, hard-kills at a deterministic
killpoint, restarts, and asserts recovery invariants. 11 scenarios:

| Scenario | Killpoint | Invariants checked |
|---|---|---|
| TestKillAtGatePending | gate.pending | pending action replays (never dropped) |
| TestKillAtJobRunning | job.running | job marked interrupted, no double-dispatch |
| TestKillAtBackupStarted | backup.started | same, Firebird-dependent |
| TestKillAtRestoreReplaceC7a | wf.replace | file intact or .pre-restore present; reconcile brings DB online |
| TestKillAtShutdownWindowC7a | wf.shut | file intact; online after restart |
| TestKillDuringCloseDB | db.closedb | file intact; online after restart |
| TestKillNightlyVerifyC7b | backup.started (schedule) | source bytes identical; grant survives; audit verifies |
| TestCorruptAuditTailRefusesStart | injected truncation | kernel refuses to start (fail-closed) |
| TestDeadWebhookIsNonFatal | dead endpoint | jobs complete; kernel healthy |
| TestKillAtStateMidPersist | state.mid-persist | state.json never torn |
| TestSurfacesAndWaitLive / TestRestoreTestNoMatviewsLive | — | surfaces, structuredContent, -NO_MATVIEWS |

This is a genuinely good chaos suite: deterministic, isolated (per-run DB
copies since a143950), and it asserts product invariants (audit chain,
pending replay, single-dispatch) rather than "no crash".

### Findings

1. **F1 — the CI "reduced chaos loop" has been a no-op.**
   `fbmcp-matrix.yml` ran `-run 'TestKillGatePending|TestKillMidPersist'`,
   which matches **none** of the real names (`TestKillAtGatePending`,
   `TestKillAtStateMidPersist`), so the step ran zero tests and passed.
   Fixed in this pass: corrected anchored pattern plus a `=== RUN` count
   guard so an empty match fails the step.
2. **F2 — RED-night forensics are destroyed.** `chaos-nightly.ps1` wrote the
   full output to one rolling file (`%TEMP%\fbmcp-chaos-last.log`). The RED
   nights 09-06/07/08 are unexplainable because the 09-09 GREEN run
   overwrote the log (it now holds a single 60-byte "ok" line). Fixed in
   this pass: per-night timestamped detail files, pruned to 14 days.
3. **F3 — M3 acceptance is not met.** Plan requires ≥5 consecutive clean
   nights; the log shows RED 09-06/07/08, GREEN 09-09 → streak = 1. With F1
   and F2 fixed, a RED night is at least diagnosable; the streak clock
   effectively restarts.
4. **F4 — soak ↔ chaos cross-talk shows up as engine-level flakiness.** The
   soak report records `nightly_verify` compensation failing with
   `dial tcp [::1]:3050 refused` — the same IPv6-loopback flakiness that hit
   CI, on the host config. Recommend `fbmcp.dev.yaml` use `127.0.0.1` and/or
   the kernel prefer IPv4 for localhost addrs.
5. **F5 — coverage gaps in the chaos surface itself** (see §3): killpoints
   exist for gate/job/backup/restore/shutdown/persist, but not for
   `fb_migration_apply` (batch mid-apply), `fb_diff_data` streaming, the
   audit-rotate path, or schedule dispatch itself. Also nothing kills the
   kernel *during* restart/reconcile (double-kill), and no scenario runs two
   concurrent clients where one dies mid-confirm.

## 3. Test coverage gaps (measured)

Overall 45.5% statements. Worst packages (function-coverage average) and
what is missing; "live" = needs a real engine, runs in the matrix lane.

| Package | Cov | Missing |
|---|---|---|
| internal/state | 22% | Jobs/PutJob, pending TakePending/replay, maintenance windows (AddWindow/InWindow), catalog (AddCatalogEntry/LatestVerifiedBackup), workflows (Put/Get/Running). All logic-rich, engine-free — pure unit targets. |
| internal/executor | 23% | estimate, Exec, execAtomic, execPerStatement, shortOf — the write path every Tier-1 tool depends on. Needs a driver seam or a live lane. |
| internal/secrets | 25% | Set/Drop, env fallback resolution order. |
| internal/schemadiff | 32% | Capture, capturePKs, DiffData streaming/cap, canonicalType, quotedList, tableShape. Helpers are pure; Capture/DiffData are SQL-heavy → live. |
| internal/configedit | 47% | ParseFile (firebird.conf/databases.conf round-trips), AppendJournal, path helpers. Fixture-file driven, engine-free. |
| internal/backupsvc | 49% | Sweep, SetForceWrite, SetReadOnly, NBackup, RestoreNoMatviews, trace start/stop/list (services API → live; request assembly can be faked). |
| internal/policy | 53% | Tools listing vs registered surface, TierForRisk mapping, WithNow clock seam. |
| internal/adminexec, confine, dbpool, gate | 51–59% | Denied/refusal branches, pool close races, gate expiry paths. |
| cmd/fbmcp | 15% | Wiring (http.go attach server lifecycle, main run/serve), tool registration completeness; covered today only via live MCP tests. |
| cmd/fbmcpctl | 4% | gate.go OOB approval flow. |
| cmd/fbmcp-tray | 0% | Windows GUI; poll logic (Snooze/pollOnce) is testable, dialogs are not. |
| internal/statetest | — | 23 lines of code, no tests. |

## 4. Plan

Ordered by risk/effort. "Lane" = where the test runs. Each phase is
independently landable.

### P0 — lane correctness (landed with this document)

- [x] F1: fix the matrix reduced-chaos `-run` pattern + empty-match guard.
- [x] F2: chaos-nightly per-night detail retention (14 days).
- [ ] F4: `fbmcp.dev.yaml` localhost → `127.0.0.1` (host soak shows ::1
  refusals); consider kernel-side IPv4-preference for `localhost`.

### P1 — unit tests for engine-free high-risk logic (≈2 days)

Highest value: these packages gate every write and every restart.

1. `internal/state`: full lifecycle table tests — jobs transitions
   (running→interrupted reconcile), pending take/replay/drop semantics,
   window add/expire, catalog latest-verified selection, workflow state
   machine; plus in-process torn-persist test arming `killpoint.SetEnabled`
   ("state.mid-persist") to assert the temp+rename atomicity directly.
   Lane: unit (hermetic).
2. `internal/executor`: introduce a narrow `DB` interface seam (already
   implicit via database/sql) or a fake `*sql.DB` driver; cover
   execAtomic vs execPerStatement switching, statement-cap (`shortOf`),
   estimate; error mapping. Lane: unit + one live case per branch in matrix.
3. `internal/secrets`: resolution order env → file → missing, Set/Drop
   idempotency. Lane: unit.
4. `internal/policy`: TierForRisk exhaustively over ops_v3_gen; Tools()
   listing equals the registered tool set (pairs with the existing
   tool_surface_test); WithNow clock seam for expiry. Lane: unit.
5. `internal/configedit`: golden-file round-trips for firebird.conf /
   databases.conf fixtures (parse → edit → render → re-parse), AppendJournal
   format. Lane: unit.
6. `internal/schemadiff`: extract pure helpers into testable form
   (canonicalType, quotedList, firstLine, tableShape) with table tests;
   Capture/DiffData one live case each in the matrix lane.

### P2 — kernel wiring (≈2 days)

7. `cmd/fbmcp/http.go`: attach-socket server lifecycle (Start/ReplaceAuth/
   Replace/Stop/Close/Wait) over net.Pipe — auth replacement mid-flight,
   double-start refusal. Lane: unit.
8. `cmd/fbmcp` tool registration: every register* function produces
   name/tier/args metadata; assert registry == policy tool list (drift
   guard), and that every Tier-1/2 tool has a killpoint-compatible path.
   Lane: unit.
9. `cmd/fbmcpctl/gate.go`: OOB approval file watcher — marker appears,
   stale marker, malformed marker. Lane: unit (tempdir).

### P3 — chaos expansion (M3 continuation, 1 scenario/day)

10. New killpoints + scenarios, one per PR, each with its invariant:
    - `migrate.batch-mid`: kill between migrations of an apply batch →
      history table shows all-or-nothing per migration (ADR-030 atomicity).
    - `schedule.mid-dispatch`: kill inside the scheduler tick → grant
      survives, next tick re-dispatches exactly once (no double nightly_verify).
    - `audit.rotate`: kill during audit rotation/checkpoint → chain still
      verifies.
    - `wf.replace` double-kill: kill again *during* restart-reconcile →
      invariants of C7a still hold after second restart.
    - concurrent-client kill: client A killed mid-confirm, client B proceeds;
      pending replays; B sees no duplicate dispatch.
11. Run the two config-independent chaos scenarios on Linux nightly too
    (now meaningful after F1) — chaos gains a second OS for free.
12. Chaos logging: append scenario-level timing to the nightly detail file
    so slowdowns (like the 44-minute GREEN) are visible over time.

### P4 — soak (M2) and guardrails

13. F4 fix verified by one full soak night without `[::1]` refusals.
14. Coverage ratchet: security lane uploads `go test -coverprofile` and
    fails only when total coverage drops (start at 45.5%, ratchet up per
    phase: P1 → ~55%, P2 → ~60%). Advisory comment first, gate later.
15. `internal/statetest`: trivial unit test (it exists to test state files).

### Acceptance criteria

- P0: matrix reduced-chaos step actually runs 2 scenarios (visible `=== RUN`
  lines); next RED night leaves a readable per-night log.
- P1: `go test ./internal/state ./internal/executor ./internal/secrets
  ./internal/policy ./internal/configedit ./internal/schemadiff -cover` each
  ≥70% statements; all hermetic (no FBMCP_* env needed).
- P2: attach-server lifecycle and tool-registry drift guarded by unit tests;
  `cmd/fbmcpctl` >60%.
- P3: five new killpoint scenarios green on the host nightly; M3 streak of
  ≥5 consecutive GREEN nights re-established and logged.
- P4: coverage ratchet active and non-regressing; soak week completes with
  zero unhandled panics and no `[::1]` errors in the report.
