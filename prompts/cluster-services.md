# HQbird cluster services playbook

Move the master of a cluster service with fbmcp. A service is one logical
database on several hosts; RCM moves its master with an operation
(switchover, failover, rejoin). fbmcp runs on the RCM host and calls the
operator API of that RCM; RCM runs every check and every step. Needs RCM
2027.5.11 or later.

The same actions are on the Services pages of the RCM UI. Use fbmcp when an
MCP client drives the cluster.

Who confirms:

- Tier 0 (`fb_cluster_service`): reads and the checks of RCM. No
  confirmation.
- Tier 1: the client shows the request to the user and calls `fb_confirm`
  with the token after the user agrees.
- Tier 2: an open maintenance window on a registry database of the host
  (`fb_window_open`, the `db` of the call), then the operator approves on
  the host: `sudo -u firebird fbmcp ctl approve <request id>`. A client
  cannot approve its own Tier 2 request.

Look first:

1. `fb_cluster_service` — every service: state, epoch, master, replicas,
   routers, problems, and `allowed now` (what the state lets you start).
2. `fb_cluster_service view=show service=shop` — the hosts, the rule of the
   automatic failover, the history of the masters, `updated_at`.
3. `fb_cluster_service view=operations service=shop` — the last operations;
   `view=operation id=<id>` — the steps of one, with the line of the point
   of no return.

Planned switchover (no data loss):

4. `fb_cluster_service view=check_switchover service=shop` — the checks of
   RCM. `not_in_sync`: wait for the replica. `service_busy`: an operation
   runs, the answer names it.
5. `fb_window_open db=<registry db> args={hours:2}` (Tier 1).
6. `fb_cluster_switchover db=<registry db> args={service:"shop"}` (Tier 2).
   With more than two hosts add `target_host`. The request shows the checks
   again. After the approval the tool waits for the operation and prints
   its steps. Applications cannot write while the old master is shut down
   (usually under a minute); a router keeps their connection strings.
7. To stop it before the point of no return:
   `fb_cluster_operation_cancel args={id:"<operation id>"}` (Tier 1). The
   old master comes online again and the service stays on it. After that
   point the cancel is refused.

The master host is lost (failover, data may be lost):

8. `fb_cluster_service view=check_failover service=shop` — the checks and
   the data loss report: the last segment of the master RCM saw, what the
   replica received and applied, and the segments lost at least.
   `master_reachable`: the master node answers; use a switchover.
9. `fb_cluster_failover db=<registry db> args={service:"shop",
   accept_data_loss:true}` (Tier 2). `accept_data_loss` must be `true`: the
   user accepts that the data the master did not ship is lost. Show the
   report of step 8 to the user before the request.
10. When the old host is back, RCM fences its database (`fb_cluster_service`
    shows `old master waits for its fence` until then). Then
    `fb_cluster_rejoin db=<registry db> args={service:"shop"}` (Tier 2): the
    old host becomes the replica. Its old file is kept beside it as
    `<name>.pre-reinit-<time>.fdb`.

A failed service:

11. A service is `failed` after an operation that failed past the point of
    no return, or when the Firebird of its master stopped answering.
    `fb_cluster_service view=check_resume service=shop` says whether the
    master serves again. Then `fb_cluster_resume args={service:"shop"}`
    (Tier 1).
12. If the Firebird of the master cannot be restarted: stop the cluster node
    of that master on its host, then run the failover of steps 8–10. A
    failed service may fail over.

The rule of the automatic failover:

13. `fb_cluster_service_policy args={service:"shop", auto_failover:true}`
    (Tier 1). The request shows the rule now and then. It needs a
    `router_port` of the service and routers in `rcm.json` (`no_witnesses`
    otherwise). After one automatic failover the rule turns itself off:
    run step 10, then turn it on again.

Definitions and routers:

14. `fb_cluster_service_put args={service:"shop", members:[{host,
    master_node, replica_node, path, priority}], alias, router_port}`
    (Tier 1). The request asks RCM to check the definition and lists its
    errors. `new:true` only creates; `if_match:"<updated_at>"` refuses when
    the service changed since step 2.
15. `fb_cluster_service_delete db=<registry db> args={service:"shop"}`
    (Tier 2). Only the record of RCM goes. The request says what stays and
    what is lost, and warns when an old master still waits for its fence.
16. `fb_cluster_service view=routers`, and `fb_cluster_router_sync` (Tier 1)
    to send a new version of the routing table to every router.
