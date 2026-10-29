# fbagent: findings to report upstream (phase 11 survey, 2026-10-29)

Found while building agent discovery (`fb_agent_discover`). fbmcp does not
use either weakness: discovery never opens backup copies and never reads
token values. Both items belong to the fbagent repo.

## F1. Local API token readable through backup copies (security)

- The live `agent_config.json` in `C:\HQbird\fbagent-p<port>\` is
  ACL-locked: the interactive account gets "access denied".
- `.bak` copies of the same file in the same directory are readable by
  that account. They are full config copies, so they carry
  `local_api.token` (the Bearer token for the Local API) and the other
  secrets in the same form as the live file. (Survey checked readability
  and key names only; token values were not read.)
- Effect: any local user who can list the directory can get the token and
  drive engine restarts, job runs and config reloads through the Local
  API. The ACL on the live file gives a false sense of protection.
- Suggested fix: write every backup / archive copy
  (`*.bak`, `agent_config_backup_*`, `agent_config_applied_*`,
  `agent_config_rejected_*`, `agent_config_superseded_*`) with the same
  owner-only ACL as the live file (`storage.WriteSecretFileAtomic`), and
  tighten the ACL of copies that already exist at upgrade time.

## F2. `password_encrypted` is base64, not encryption (security)

- `firebird.credentials.password_encrypted` (and other `*_encrypted`
  fields) are decoded with `DecodeFirebirdPassword`, which is a base64
  decode (`internal/config/agent_config_snapshot.go`, `sealablePlaintext`).
- Effect: anyone who reads a config copy (see F1) gets the SYSDBA password.
  The field name suggests protection that does not exist.
- Suggested fix: rename the field (`password_b64`) or encrypt it with a
  host-bound key (DPAPI on Windows), and say so in the config reference.

## Note for operators

Until F1 is fixed, keep the agent directories readable only by the service
account and administrators, or delete old `.bak` copies after an upgrade.
