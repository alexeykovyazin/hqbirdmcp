# DDL-review playbook

1. `fb_schema_list` / `fb_describe` for the object.
2. `fb_write` with `mode=preview`. The preview lists impact, compensation advisory, and confirmation channels. **Do not call the preview “safe”.**
3. Human confirms: Tier 1 in-band token, or out-of-band via the fbmcp-tray Approve/Deny popup or `fbmcpctl approve`. Channel is audited.
4. Irreversible / WHERE-less / DROP content escalates (ADR-019). Mixed-tier scripts are denied.
