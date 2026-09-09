# fbmcp — project review & test implementation plan (2026-09-09, rev 2)

Scope: full codebase review, nightly-chaos lane review, and a prioritized
plan to close the test gaps. Measurements taken from `go test ./...
-coverprofile` (unit suites, live suites skipped — CI runs those separately).
Rev 2 corrections (vs rev 1) came from a line-by-line check of the plan
against the source; they are listed inline and in §5.

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
killpoint, restarts, and asserts recovery invariants. 12 test functions:
10 chaos scenarios plus 2 live-functional ones:

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

The product has **13 armed killpoints** (gate.pending, gate.confirmed,
job.running, job.done, backup.started, backup.finished, restore.started,
restore.finished, exec.pre-commit, exec.post-commit, db.closedb, wf.replace,
wf.shut, state.mid-persist). The harness exercises only 7 of them; 6 are
unexercised (gate.confirmed, job.done, backup.finished, restore.started,
restore.finished, exec.pre-commit, exec.post-commit — see P3).

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
5. **F5 — the harness uses 7 of the 13 armed killpoints.** Unexercised:
   `gate.confirmed`, `job.done`, `backup.finished`, `restore.started`,
   `restore.finished`, `exec.pre-commit`, `exec.post-commit`. The executor
   points already cover migration mid-batch and the atomic-vs-per-statement
   commit contract — they need scenarios, not new product code. Also
   missing: a killpoint in the scheduler tick (dispatch was at-least-once by
   construction — now exactly-once, see P3), one mid-audit-append, a kill during
   restart/reconcile (double-kill), and a two-client scenario with one
   client dying mid-confirm.

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
   machine. The torn-persist atomicity is testable **in-process** (no
   subprocess needed): `state.persist` hits `killpoint.Hit("state.mid-persist")`
   between the fsynced temp write and the rename (state.go:225), and
   `killpoint.Hit` needs BOTH `killpoint.SetEnabled(map[string]bool{...:true})`
   AND the `FBMCP_KILLPOINT_DIR` env var (Hit reads it directly and silently
   no-ops when unset). The test arms the checkpoint, blocks persist on
   another goroutine, asserts `state.json` is still the old snapshot and
   `state.json.tmp` exists, writes `<name>.release`, and asserts the rename
   completes. Lane: unit (hermetic).
2. `internal/executor`: two distinct targets.
   - **Prepare/Prepared is pure** — classify-driven, no DB: tier refusals
     (Tier-0 "use fb_query" message, Tier-3 disabled), MinFB floor
     computation across statements, NeedsExclusive for non-CONCURRENTLY
     REFRESH, statement splitting. Instant table tests, no seam.
   - **Exec paths** (`Exec`/`execAtomic`/`execPerStatement`) take `*sql.DB`
     from `dbpool.Manager` — register a fake `database/sql` driver
     (`sql.Register` + driver.Driver/Conn/Stmt/Tx) to assert: DDL-free
     script ⇒ one atomic tx with rollback on statement N's error (message
     "transaction rolled back"); DDL script ⇒ per-statement commits with
     the "PARTIALLY APPLIED: i of n" message; per-statement 30s and total
     5m timeouts; `shortOf` truncation; killpoints exec.pre-commit /
     exec.post-commit (via the same arm-and-release trick as state).
     Lane: unit (fake driver) + one live case per branch in matrix.
3. `internal/secrets`: source order is env → OS keyring → error (env always
   wins, keyed by env-var NAME). `Get`'s env-first ordering and error text
   are testable as-is. `Set`/`Drop` call `go-keyring` package functions
   directly — on Linux CI there is no Secret Service daemon, so introduce a
   one-var seam (e.g. `var keyringStore = keyring`-style indirection) before
   unit-testing them; otherwise keep Set/Drop live-only. Lane: unit after
   seam; without the seam, Get-only.
4. `internal/policy`: TierForRisk exhaustively over the ops_v3_gen table;
   Tools() listing equals the registered tool surface (pairs with the code
   drift guard in P2.8); WithNow clock seam for expiry paths; toFloat edge
   cases. Lane: unit.
5. `internal/configedit`: golden-file round-trips for firebird.conf /
   databases.conf fixtures (parse → edit → render → re-parse; fixtures from
   the real configs in packaging/ or dev hosts), AppendJournal format,
   ConfPath/DatabasesConfPath resolution. Lane: unit.
6. `internal/schemadiff`: pure helpers first (canonicalType, quotedList,
   firstLine, contains, tableShape) as table tests; Capture/DiffData one
   live case each in the matrix lane (they are SQL-heavy against
   RDB$ tables; DiffData's row-cap refusal and sample streaming are the
   assertions). Lane: unit + live.
7. `internal/schedule` (added in rev 2): the Ticker already has seams
   (`WithNow`, `OnSkip`, `WithGateProbe`, `WithDBExists`). Unit tests pin
   the exactly-once dispatch contract: `consider` persists the slot's
   `LastFiredAt` marker BEFORE the fire call, releases the slot on a
   synchronous fire error, and never re-fires a consumed slot after a
   restart. Landed with the product change — see P3. Lane: unit.

### P2 — kernel wiring (≈2 days)

7. `cmd/fbmcp/http.go`: attach-socket server lifecycle (Start/ReplaceAuth/
   Replace/Stop/Close/Wait) over net.Pipe — auth replacement mid-flight,
   double-start refusal. Lane: unit.
8. `cmd/fbmcp` tool registry drift (code level): `TestToolSurfaceDrift`
   already pins toolMeta against README + docs/tool-reference.md; the
   missing direction is runtime vs policy — drive `registerP4Tools` (and
   siblings) on an in-process MCP server over net.Pipe, call `tools/list`
   (registration touches no pools, so this is hermetic), and assert the
   name set equals `toolMeta` keys and `policy.Tools()` — three-way
   agreement. Lane: unit.
9. `cmd/fbmcp` OOB approval watcher (`startApprovalWatcher`, p3tools.go:453
   — rev 1 wrongly attributed this to fbmcpctl): approval/denial marker
   appears → pending resolves; stale marker; malformed marker; marker for
   an unknown request id. Needs a poll-interval seam or a short poll tick
   in tests. Lane: unit (temp dirs).

### P3 — chaos expansion (M3 continuation, 1 scenario/day)

Rev 2: no NEW killpoints are needed for the first three items — the source
already has 13 armed points and the harness uses only 7. Cover the
unexercised ones first; each scenario reuses the existing C7a/C7b skeleton.

10. New scenarios (existing killpoints unless noted):
    - `exec.pre-commit` on an atomic script: after restart the effects are
      ABSENT (rolled back) and the job is interrupted — proves the atomic
      path's rollback invariant under a real kill, not just engine RO-tx.
    - `exec.post-commit`: effects are PRESENT (commit durable) and the job
      is interrupted — documents the at-least-once outcome honestly (the
      restartAndVerify "succeeded/failed despite kill" check must be
      adjusted for this scenario; the commit deliberately wins).
    - `fb_migration_apply` mid-batch via `exec.pre-commit` with a 2+
      migration manifest: history table stops at the last fully applied
      migration (ADR-030 per-migration atomicity); re-apply completes and
      is idempotent. No new killpoint required — apply routes through the
      executor (rev 1 wrongly proposed a new `migrate.batch-mid` point).
    - `gate.confirmed`: kill after consume, before dispatch — the pending
      action must NOT replay (it was consumed) and no job may exist
      (no orphan dispatch); the client can re-request cleanly.
    - `backup.finished` (C7b extension): kill after the backup, before
      catalog/job bookkeeping — catalog has no phantom verified backup,
      job interrupted, source bytes unchanged.
    - `schedule.mid-dispatch` (new killpoint, scheduler tick): **decision
      made and implemented 2026-09-09 — exactly-once per due slot.**
      `consider` persists the slot's `LastFiredAt` marker BEFORE the fire
      call, so a kill in that window consumes the slot without dispatching;
      a synchronous fire refusal rolls the marker back (a refused
      submission is not a dispatch, the next tick retries). Pinned by
      `TestDispatchMarkerDurableBeforeFire`, `TestDispatchExactlyOnceAcrossRestart`,
      `TestFireErrorReleasesSlot` (unit) and `TestKillScheduleMidDispatch`
      (harness).
    - `audit.mid-append` (new killpoint): kill between the log-line write
      and the head-sidecar update — proves the real torn-write behaves like
      TestCorruptAuditTailRefusesStart's synthetic truncation (fail-closed
      on restart). There is no audit rotation in the source (rev 1 proposed
      an "audit.rotate" point that has nothing to hit).
    - double-kill: kill again DURING restart-reconcile of C7a → invariants
      still hold after the second restart.
    - concurrent-client kill: client A killed mid-confirm, client B
      proceeds; B sees no duplicate dispatch; A's pending replays.
11. Linux coverage of the two config-independent scenarios: already true
    after the F1 fix (the matrix reduced-chaos step runs them, once per FB
    version). Optional cleanup: hoist them into a single dedicated job to
    avoid the 4× duplication.
12. Chaos logging: append scenario-level timing to the nightly detail file
    so slowdowns (like the 44-minute GREEN) are visible over time.
13. Product nit (killpoint): `Hit` silently no-ops when the checkpoint is
    armed but `FBMCP_KILLPOINT_DIR` is unset — a misarmed harness run would
    test nothing and pass. Log once when that combination is seen.

### P4 — soak (M2) and guardrails

14. F4 fix verified by one full soak night without `[::1]` refusals.
15. Coverage ratchet: security lane uploads `go test -coverprofile` and
    fails only when total coverage drops (start at 45.5%, ratchet up per
    phase: P1 → ~55%, P2 → ~60%). Advisory comment first, gate later.

(Rev 1 had a P4 item "test internal/statetest" — wrong: statetest is not
state-file testing, it is the shared test-double package `StubFacts` for
`state.FactsProvider` used by other packages' tests. It needs no tests of
its own; the compile-time `var _ state.FactsProvider` assertion is the
contract.)

### Acceptance criteria

- P0: matrix reduced-chaos step actually runs 2 scenarios (visible `=== RUN`
  lines); next RED night leaves a readable per-night log.
- P1: `go test ./internal/state ./internal/executor ./internal/secrets
  ./internal/policy ./internal/configedit ./internal/schemadiff
  ./internal/schedule -cover` each ≥70% statements; all hermetic (no
  FBMCP_* env, no engine, no keyring service).
- P2: attach-server lifecycle and three-way tool-surface agreement guarded
  by unit tests; approval watcher covered; `cmd/fbmcpctl` >60% or explicitly
  descoped with a note (it is a thin wrapper).
- P3: seven new scenarios green on the host nightly (exec pre/post-commit,
  migration mid-batch, gate.confirmed, backup.finished, schedule
  mid-dispatch, audit mid-append, double-kill, concurrent-client — count
  flexible); M3 streak of ≥5 consecutive GREEN nights re-established.
- P4: coverage ratchet active and non-regressing; soak week completes with
  zero unhandled panics and no `[::1]` errors in the report.

## 5. Rev 2 corrections (plan vs source)

Checked line-by-line; each of these was wrong or incomplete in rev 1:

| # | Rev 1 claim | Source says | Resolution |
|---|---|---|---|
| C1 | "11 scenarios" | 12 test functions (10 chaos + 2 live) | corrected §2 |
| C2 | schedule kill ⇒ "re-dispatches exactly once" | `consider()` fired first, persisted `LastFiredAt` after ⇒ at-least-once | fixed by product change 2026-09-09: marker persists before fire (see P3) |
| C3 | new `migrate.batch-mid` killpoint needed | apply routes through the executor; `exec.pre-commit`/`exec.post-commit` already exist | scenario on existing points |
| C4 | `audit.rotate` killpoint | no rotation exists (append-only + head sidecar) | replaced with `audit.mid-append` |
| C5 | OOB approval watcher in `cmd/fbmcpctl/gate.go` | it is `startApprovalWatcher` in `cmd/fbmcp/p3tools.go:453` | retargeted |
| C6 | `internal/statetest` "tests state files" | it is the shared `StubFacts` test-double package | item dropped |
| C7 | in-process persist test = `SetEnabled` only | `Hit` also requires `FBMCP_KILLPOINT_DIR` env and blocks on `.release`; silently no-ops otherwise | full handshake documented; product nit added (P3.13) |
| C8 | secrets "env → file → missing" | env → OS keyring → error; Set/Drop hit go-keyring directly (no seam, needs dbus on Linux CI) | corrected; seam proposed |
| C9 | executor needs a seam first | `Prepare` is pure (no DB); `Exec` uses `*sql.DB` (fake driver works) | pure tests split out as the cheap win |
| C10 | "run the 2 OS-free chaos scenarios on Linux nightly" | already true post-F1, ×4 (once per matrix job) | marked done; optional dedupe |
| C11 | tool drift covered by tool_surface_test.go | that test covers docs only; runtime `tools/list` vs `toolMeta` vs `policy.Tools()` is untested | P2.8 three-way guard |
| C12 | killpoint inventory not listed | 13 armed points, harness exercises 7 | inventory added; P3 prioritizes the 6 unexercised |
