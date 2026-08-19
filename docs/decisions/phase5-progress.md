# Phase 5 progress — integration & operations (core)

Date: 2026-08-17. `go test ./...` green; `cmd/fbmcp` and `cmd/fbmcpctl` build.

This is M5 *core*, not the full unattended-week checklist.

## What landed

| Part | Status | Notes |
|---|---|---|
| P5.3 T0 ADRs | done | ADR-016, 023, 024 |
| Scheduler | done | `internal/schedule` + durable `state.Schedule` grant; fire path does not call the gate |
| Tools | done | `fb_schedule_list/create/delete`, `fb_retention_run` |
| K5 nightly_verify | done | backup → test-restore |
| K7 | done | `internal/notify` local log + HMAC webhook |
| Retention | done | ADR-016 keep-everything; canary test |
| P5.2 | done | packaging/, ADR-017, Windows svc stub, P3.7 start/stop refuse-without-posture |
| P5.1 | done | ADR-022, `internal/transport` auth battery, `/healthz` |
| P5.5 | done | `internal/selfobs` + `fbmcpctl status` |
| P5.6 | done | `cmd/fbmcpctl` approve/status/setup/doctor; D5 env residual documented |
| P5.4 | done | `prompts/` + honesty lint; `phase5-gap-notes.md` |

## Still open for the M5 evidence week

- Run a long-lived process with a confirmed `nightly_verify` schedule on spike5
- Last nights as an installed Windows service
- Linux/Docker smoke if containers are brought up
