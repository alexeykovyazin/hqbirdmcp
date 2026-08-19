# ADR-020 — Config parameter registry

Status: accepted (2026-08-17) · Fed by: P4.6 T1

## Context
`firebird.conf` / `databases.conf` must not be free-form text editing.
Operators need schema-validated, journaled, atomic edits with
restart-required flags. A from-scratch round-trip parser that preserves
every comment is non-trivial.

## Decision
1. **Curated registry** in `internal/configedit` (name, type, range,
   default, restart-required, security-flag, min FB version). Unknown
   parameters are refused. Security-sensitive keys (WireCrypt, bind
   address, auth plugins) escalate to Tier 2.
2. **Parse → structured model → apply → write `.new` → atomic rename**,
   keeping `.prev` and appending a JSONL change journal under the state
   dir. Kill during write must never leave a half-written conf.
3. **Normalized rewrite with comment preservation best-effort:** lines
   that are comments or blank are kept in order; known `key = value`
   lines are rewritten in place; unknown keys already in the file are
   left untouched (but cannot be *set* through the tool). If a change
   would require dropping comments, the preview says so and execute
   still preserves comment-only lines.
4. Config file path defaults to `{instance.bin_dir}/firebird.conf` and
   `{instance.bin_dir}/databases.conf` (HQbird layout: bin_dir is the
   install root).

## Consequences
`fb_config_get` / `fb_config_diff` / `fb_config_set` are the only mutation
path. Restart-required results advise `fb_service_status` (start/stop
still wait on §4.8 posture). WireCrypt (op 39) is a registry entry.
