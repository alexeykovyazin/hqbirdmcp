# Security-audit playbook

1. `fb_effective_access` for the user under review (role-hierarchy expansion is **capped at 200 rows; roles are not fully expanded**).
2. Proposed GRANT/REVOKE: `fb_grant` / `fb_revoke` with `mode=preview` (before/after privilege diff). Previews are informational — not “safe”.
3. Execute only after in-band (Tier 1) or out-of-band (fbmcp-tray popup or `fbmcpctl approve`) confirmation.
4. Lockout guards refuse dropping the last admin / self-kill of registry RO users.
