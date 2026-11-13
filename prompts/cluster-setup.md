# HQbird cluster setup playbook

Set up a master and its replica with fbmcp. Each server has the HQbird 2027
kit (`.run` / setup), 2027.2.29 or later, and `sudo fbmcp ctl hqbird-install`
connected fbmcp to the fbagent of that server. Since kit 2027.2.29 the setup
makes no cluster manager (RCM) and opens no pairing window by default: fbmcp
does both.

Master host (fbmcp of the master):

1. `fb_agent_cluster` — state `free`, the slot and its ports, and
   `hqbirdrcm is staged`. `in_cluster` means the server is already in a
   cluster: stop. An answer 404 means fbagent older than 3.1.0: update it.
2. `fb_agent_cluster_rcm args={cluster_id:"prod"}` (Tier 1) — the request
   shows the state and the refusal the agent will give (`rcm_owned`,
   `in_cluster`, `rcm_not_staged`). The run makes the RCM (up to 10
   minutes; Firebird does not stop) and names the RCM URL, the CA hash and
   the file with the first RCM password. The password is never shown.
   `rcm_not_staged`: hqbirdrcm is not in the agent's product cache, see
   `fb_agent_product_update_status product=hqbirdrcm`.
3. On the host: `sudo fbmcp ctl hqbird-install` again — it writes the `rcm:`
   section and the RCM password secret. Reconnect the MCP client: the
   cluster tools (`fb_cluster_status` and the others) appear.
4. `fb_cluster_status` — the RCM answers; one node (this host).
5. `fb_cluster_rcm_password` (Tier 1) — once: the first RCM password must
   change. fbmcp stores the new one; to use the RCM UI, set your own there
   and store it with `fbmcp ctl secret set FBMCP_RCM_PASSWORD`.

Replica host (fbmcp of the replica):

6. `fb_agent_cluster_pairing args={action:"on", hours:2}` (Tier 1) — opens
   the pairing window on the pairing port of the agent's slot (7450–7459).
   Note the fingerprint in the reply. `in_cluster`: the server is already
   in a cluster, do not link it. Without fbmcp on that host:
   `sudo -u firebird fbagent --cluster-pairing on`.

Master host again:

7. `fb_cluster_probe hosts=[master, replica]` — one row per agent. The
   master is `member` (the RCM host, no fingerprint to compare); the
   replica is `free`: compare its fingerprint with step 6. A host with
   several instances has a row per agent; give `host:port` to take one.
8. `fb_cluster_link args={hosts:"master,replica", master:"master"}`
   (Tier 1) — the request repeats the probe and pins the fingerprints.
   Confirm only when they match. To add a replica to a cluster that has a
   master already: `args={hosts:"replica2", replica_of:MN}` (master member
   id from `fb_cluster_status view=nodes`); without `replica_of` or
   `new_master` the RCM refuses with `lone_master`. An own RCM of the
   replica host stops after the link.
9. `fb_cluster_status view=nodes` — the node ids: master node `MN`, replica
   node `RN`.
10. `fb_cluster_replica_folder args={node:RN}` (Tier 1) — creates
    `/databases/replica/<RN>` and sets it as `databases.root`.
11. `fb_cluster_enroll args={node:MN, root:"/databases/master"}` (Tier 1) —
    the request shows the dry run: check the list of databases.
12. Open a window: `fb_window_open` on a registered database of this host.
13. `fb_cluster_firebird_restart db=<window db> args={node:MN}` (Tier 2,
    operator approves) — loads the replication settings. Every connection
    to this server breaks.
14. `fb_cluster_publication args={node:MN}` (Tier 1).
15. `fb_cluster_status view=databases` — the database id `D` on `MN`.
16. `fb_cluster_reinit db=<window db> args={node:MN, db_id:D, to:RN}`
    (Tier 2) — copies the database to the replica and waits. On a refusal,
    read the RCM code (for example `databases_root_set`, `longtx`,
    `insufficient_space`) and fix that first.
17. `fb_cluster_verify master_node=MN master_db=D replica_node=RN` — verdict
    `ok`, then `fb_cluster_status view=databases` shows the pair.

Replica host:

18. `fb_agent_cluster_pairing` — `closed` (`claimed` or `in_cluster`) after
    the link. If it is still open: `args={action:"off"}`.

Do not change the cluster without the operator: every Tier 1 step needs
`fb_confirm`, every Tier 2 step the operator's `fbmcp ctl approve`.
