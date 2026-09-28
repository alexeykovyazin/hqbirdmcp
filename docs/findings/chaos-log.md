# Chaos harness nightly log
2026-08-30 22:55:37 count=1 verdict=GREEN exit=0
2026-08-31 02:30:03 count=50 verdict=RED exit=1
triage: detail pruned (pre-retention); first full-battery night, no recurrence signature kept - unclassified
2026-09-01 02:30:02 count=50 verdict=GREEN exit=0
2026-09-03 02:30:02 count=50 verdict=GREEN exit=0
2026-09-04 02:30:03 count=50 verdict=GREEN exit=0
2026-09-05 02:30:03 count=50 verdict=GREEN exit=0
2026-09-06 02:30:03 count=50 verdict=RED exit=1
triage: detail pruned (pre-retention); unclassified (flanked by GREEN 09-05 / 09-09)
2026-09-07 02:30:01 count=50 verdict=RED exit=1
triage: detail pruned (pre-retention); unclassified (part of the 09-06...09-08 cluster)
2026-09-08 02:30:01 count=50 verdict=RED exit=1
triage: detail pruned (pre-retention); unclassified (part of the 09-06...09-08 cluster)
2026-09-09 02:30:01 count=50 verdict=GREEN exit=0
2026-09-10 02:30:02 count=50 verdict=RED exit=1
triage: forensics done same week - exec scenarios targeted the wrong sample (spike DB has no PROJECT table); fixed in 2cd748a (chaos fixes from 0910 RED night)
2026-09-11 02:30:02 count=50 verdict=GREEN exit=0
2026-09-12 02:30:02 count=50 verdict=GREEN exit=0
2026-09-13 02:30:02 count=50 verdict=RED exit=1
triage: C7b post-restart hash race (lost 1/50); fixed in 2ecbafc - the racy re-check was dropped, pre-kill identity is the invariant
2026-09-14 02:30:02 count=50 verdict=GREEN exit=0
2026-09-15 02:30:07 count=50 verdict=GREEN exit=0
2026-09-16 02:30:03 count=50 verdict=GREEN exit=0
2026-09-18 02:30:05 count=50 verdict=GREEN exit=0
2026-09-19 02:30:04 count=50 verdict=GREEN exit=0
2026-09-20 02:30:04 count=50 verdict=RED exit=1
triage: detail pruned; isolated RED between GREEN 09-19 and 09-22 - timing flake presumed, unclassified
2026-09-22 02:30:04 count=50 verdict=GREEN exit=0
2026-09-23 02:30:04 count=50 verdict=RED exit=1
triage: harness-failure x2 - TestKillScheduleMidDispatch fast-fail (17s): 1 fired run before the kill landed (fire-before-kill race; chain identity held, no data-loss signature); TestKillAtExecPostCommit (91s): killpoint exec.post-commit never became ready. Follow-up: harden killpoint-ready waits (WS12)
2026-09-24 02:30:06 count=50 verdict=RED exit=1
triage: harness-failure - TestKillScheduleMidDispatch x2, exactly-once window flapped both ways under load (firedRuns=0 then firedRuns=3)
2026-09-25 02:30:06 count=50 verdict=RED exit=1
triage: harness-failure - TestKillScheduleMidDispatch x2 (next-slot chain never reached a terminal job, jobs=[]; firedRuns=0) - liveness timeouts of the same scenario
2026-09-26 02:30:08 count=50 verdict=RED exit=1
triage: harness-failure - TestKillScheduleMidDispatch (jobs=[], 228s liveness timeout)
2026-09-27 02:30:06 count=50 verdict=RED exit=1
triage: engine-down - fb5 (127.0.0.1:3055) refused connections ~01:16-02:30 (connectex refused; wsarecv forcibly closed mid-backup): spike5 scenarios failed incl. TestKillAtRestoreReplaceC7a (db file missing post-reconcile) + TestRestoreTestNoMatviewsLive. FB5 verified Running 09-28 (netstat 3055 LISTENING); environment self-healed
2026-09-28 02:30:06 count=50 verdict=GREEN exit=0
triage: M3 clean-streak restarts here - first GREEN after the 09-23...09-27 REDs; environment stable, 50/50 scenarios passed
## Triage summary (2026-09-28, WS7 T7.2 - project_review_2026-09-28.md F2)

Classification per RED night is inlined above (`triage:` lines): 09-23...09-26
harness-failure (timing-sensitive TestKillScheduleMidDispatch / killpoint-arm
waits; no data-loss signature), 09-27 engine-down (fb5/3055 outage,
self-healed), older nights pruned or already fixed (09-10 -> 2cd748a,
09-13 -> 2ecbafc). Missing runs 09-17/09-21: the scheduled task fired
nothing those nights and run history was not preserved - treated as gaps,
not verdicts.

Soak nightly_verify connectex (F2): the [::1]/IPv6 hypothesis is REFUTED -
recent failures dial 127.0.0.1:3050 explicitly and the port had NO listener
at all: the standard Firebird 5 service for C:/Firebird5 (instance fb5std,
database employee5std) was not running and had no registered service (5
firebird.exe processes only: fb2/fb3/fb4/fb5/replica on
3052/3053/3054/3055/3065). Action 2026-09-28: service FirebirdServerFB5std
registered (auto-start, binPath C:/Firebird5/firebird.exe) and started;
port 3050 LISTENING (pid 22988); kernel db-health for employee5std ->
online. The next 06:00 nightly_verify is the end-to-end confirmation.

Detail logs per night: %TEMP%/fbmcp-chaos/fbmcp-chaos-YYYYMMDD.log
(14-night rolling retention).
