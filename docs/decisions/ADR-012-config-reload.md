# ADR-012 — Live reload of fbmcp.yaml

Status: accepted (2026-08-18) · Implements P1.1 T4

## Decision

The kernel holds an immutable `config.Config` snapshot behind `config.Handle` (`atomic.Pointer`). Reloading `fbmcp.yaml` swaps that pointer after validation and an affected-set drain. Claude and extra stdio clients keep the same D8 kernel (attach); they do not restart.

Triggers:

- Directory watch (`fsnotify`) on the config path, debounced, no-op when the canonical snapshot hash is unchanged.
- Explicit `fb_config_reload` (Tier 0).
- `fb_db_register` after a successful atomic YAML write (add-only apply; does not wait for its own job).

## Drain

Only IDs whose DSN, path, secret-env names, instance `addr`/`bin_dir`, or membership changed are drained. Add-only and `retention.keep_days` skip drain.

Before swap: mark draining (block `Submit`), stop traces, drop matching pending actions, remap/delete schedules and windows, wait for queued/running jobs **excluding the Apply caller’s job id** and for `running|compensating` workflows. Timeout (30s) refuses the reload and keeps the old snapshot.

Then `CloseDB` for those IDs, **then** `Handle.Swap`, then facts invalidate.

Rename is strict: exactly one id disappeared, one appeared, unique shared normalized path. Duplicate paths and path-swaps are not treated as rename.

`fb_db_register` pending is keyed by **instance id** and is kept unless that instance is removed.

## Listener / identities

`state.dir` cannot be reloaded (D8 lock, audit, store, attach files).

`listen` / TLS paths: in-process `http.Server.Shutdown` then `ListenAndServeTLS` after `CheckRemote`. Identities/key env: `transport.Authenticator.Replace` without rebind. Stdio and attach stay up. Cert file bytes with unchanged YAML paths are not watched; run `fb_config_reload` after rotating files.

ADR-022 violations (loopback bind, missing cert/key, zero identities) refuse the reload.

## Consequences

- `notify.Bus.SetWebhook` updates delivery without reopening `events.jsonl`.
- Scheduler skips unknown database ids.
- Historical jobs keep their original `Database` string after remove/rename.
