# HQbird cluster setup playbook

Set up a master and its replica with fbmcp on the **master** host. Each
server has the HQbird 2027 kit (`.run` / setup) with the cluster manager:
the installer prints a `Pairing:` line with the certificate fingerprint and
opens the pairing window for 24 hours. `fbmcp ctl hqbird-install` wrote the
`rcm:` section and the RCM password secret.

0. On **every** server, with the fbmcp of that server:
   `fb_agent_cluster_pairing` — the pairing window. When it is `closed`
   (`window_ended` or `window_closed`: the 24 hours of the setup are over),
   `fb_agent_cluster_pairing args={action:"on", hours:2}` (Tier 1) opens it
   again. `in_cluster` means the server is already in a cluster: do not link
   it. The fingerprint in the reply is the one the probe must show. Without
   the fbagent pairing API: `sudo -u firebird fbagent --cluster-pairing on`
   on the host.
1. `fb_cluster_status` — the RCM answers; no nodes yet.
2. `fb_cluster_rcm_password` (Tier 1) — once: the first RCM password must
   change. fbmcp stores the new one; to use the RCM UI, set your own there
   and store it with `fbmcp ctl secret set FBMCP_RCM_PASSWORD`.
3. `fb_cluster_probe hosts=[master, replica]` — every host `free`; compare
   each fingerprint with the installer's `Pairing:` line on that host.
4. `fb_cluster_link args={hosts:"master,replica", master:"master"}` (Tier 1)
   — the request repeats the probe and pins the fingerprints. Confirm only
   when they match. The RCM of the replica host stops after the link.
5. `fb_cluster_status view=nodes` — the node ids: master node `MN`, replica
   node `RN`.
6. `fb_cluster_replica_folder args={node:RN}` (Tier 1) — creates
   `/databases/replica/<RN>` and sets it as `databases.root`.
7. `fb_cluster_enroll args={node:MN, root:"/databases/master"}` (Tier 1) —
   the request shows the dry run: check the list of databases.
8. Open a window: `fb_window_open` on a registered database of this host.
9. `fb_cluster_firebird_restart db=<window db> args={node:MN}` (Tier 2,
   operator approves) — loads the replication settings. Every connection to
   this server breaks.
10. `fb_cluster_publication args={node:MN}` (Tier 1).
11. `fb_cluster_status view=databases` — the database id `D` on `MN`.
12. `fb_cluster_reinit db=<window db> args={node:MN, db_id:D, to:RN}`
    (Tier 2) — copies the database to the replica and waits. On a refusal,
    read the RCM code (for example `databases_root_set`, `longtx`,
    `insufficient_space`) and fix that first.
13. `fb_cluster_verify master_node=MN master_db=D replica_node=RN` — verdict
    `ok`, then `fb_cluster_status view=databases` shows the pair.

Do not change the cluster without the operator: every Tier 1 step needs
`fb_confirm`, every Tier 2 step the operator's `fbmcp ctl approve`.
