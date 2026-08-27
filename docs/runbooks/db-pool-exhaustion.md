# RB-017: Database connection-pool exhaustion

- **Severity:** ticket (no dedicated alert; surfaces as latency burn and 503s)
- **Alert:** none of its own. Detected by the queue-depth signal in
  `docs/failure-handling.md` §5.3 and by the pool gauges:
  ```promql
  pp_db_pool_in_use / pp_db_pool_max > 0.9
  ```
  It usually reaches you as `PaymentAPILatencyFastBurn` or `PaymentAPIFastBurn`.
- **Triggered when:** pool wait time exceeds 10 ms (warn), 50 ms (alert), 200 ms (page) per the
  queue-depth table. The action at the page threshold is "scale readers or shed".
- **Plane / service:** data · any binary embedding `runtime.PostgresEnv`
- **Related:** `docs/failure-handling.md` §5.1 and §5.3, `docs/data-plane.md`,
  [payment-api-latency.md](payment-api-latency.md), [aurora-failover.md](aurora-failover.md)

## What this means

Each pod holds a pool sized by `PP_DATABASE_MAX_CONNS` (default 20) and `PP_DATABASE_MIN_CONNS`
(default 2). When every connection is checked out, requests queue for one. A queue converts a
throughput problem into a latency problem and then into a timeout problem, which produces retries,
which makes the throughput problem worse — the exact chain `docs/failure-handling.md` §5.2 exists
to prevent.

The arithmetic that governs this, and that is violated more often than any other number in the
platform:

```
PP_DATABASE_MAX_CONNS  ×  replicas  ×  deployments-sharing-the-cluster
        must stay below the instance's max_connections,
        with headroom for migrations and for a human with psql
```

Two server-side bounds contain the damage: `PP_DATABASE_STATEMENT_TIMEOUT` (5 s default; 250 ms for
data-plane reads and 2 s for writes in the documented production budget) kills a pathological query
in the database rather than letting it hold a connection until the client gives up, and
`PP_DATABASE_LOCK_TIMEOUT` (2 s) stops a money-path transaction queueing behind a long one.

## Impact

- Latency rises sharply at the p99 while p50 stays flat — the signature of a queue.
- At full exhaustion, handlers return `503` and the shedder starts dropping by priority class
  (ladder rungs 2, 4, 6). Refunds and voids have reserved capacity and survive longest.
- **No money at risk.** A payment that cannot reach the database fails closed and is retryable.
- The compounding risk is scaling out to fix it: more pods means more pool connections against one
  writer, which is how an API capacity problem becomes a database outage.

## Immediate triage (first 5 minutes)

1. Which pool, and how bad:
   ```promql
   pp_db_pool_in_use / pp_db_pool_max
   topk(5, pp_db_pool_in_use / pp_db_pool_max)
   pp_http_inflight_requests
   ```
2. Server side — is the instance itself near its limit?
   ```sql
   SELECT count(*) AS total,
          count(*) FILTER (WHERE state = 'active')              AS active,
          count(*) FILTER (WHERE state = 'idle in transaction') AS idle_in_txn
   FROM   pg_stat_activity;
   SHOW max_connections;
   ```
   `idle in transaction` is the one to look at hardest: those connections are held by application
   code that opened a transaction and went away.
3. What is holding them?
   ```sql
   SELECT pid, usename, application_name, state,
          now() - query_start  AS runtime,
          now() - xact_start   AS txn_age,
          left(query, 120)     AS query
   FROM   pg_stat_activity
   WHERE  state <> 'idle'
   ORDER  BY xact_start NULLS LAST
   LIMIT  20;
   ```
4. Blocking locks:
   ```sql
   SELECT blocked.pid AS blocked_pid, blocking.pid AS blocking_pid,
          left(blocked.query, 80) AS blocked_query, left(blocking.query, 80) AS blocking_query
   FROM   pg_stat_activity blocked
   JOIN   pg_stat_activity blocking ON blocking.pid = ANY(pg_blocking_pids(blocked.pid))
   WHERE  cardinality(pg_blocking_pids(blocked.pid)) > 0;
   ```
5. Do the arithmetic, now, before anyone suggests scaling:
   ```bash
   kubectl -n pp-data-plane get deploy -o custom-columns=\
   'NAME:.metadata.name,REPLICAS:.spec.replicas,MAXCONNS:.spec.template.spec.containers[0].env[?(@.name=="PP_DATABASE_MAX_CONNS")].value'
   ```
6. Rule out a failover as the cause: `changes(pg_writer_instance_changed_total[10m])`.

## Diagnosis

- **`idle in transaction` count is high and growing** → application code holds transactions open
  across a slow call. This is a defect, not capacity. → *M1*.
- **Long-running `active` queries, mostly reporting shapes** → control-plane or analytical work on
  the wrong pool. Its statement timeout is 30 s by design, off the hot path — but it shares the
  cluster. → *M2*.
- **Blocking locks with a long-lived blocker** → a migration or a manual transaction. → *M3*.
- **Pools full, no long queries, throughput at a record high** → genuine load. → *M4*, and check
  the arithmetic before scaling.
- **`total` in `pg_stat_activity` near `max_connections`** → the *instance* is exhausted, not one
  pod's pool. Adding replicas now makes it strictly worse. → *M5*.
- **Started at a deploy** → the deploy changed `PP_DATABASE_MAX_CONNS`, replica count, or query
  shape. → *M6*.
- **Started at an Aurora failover** → the herd re-establishing.
  → [aurora-failover.md](aurora-failover.md); wait rather than act.

## Mitigation

**M1 — end the abandoned transactions.** Cancel the query, do not terminate the connection, unless
the session itself is stuck:
```sql
SELECT pg_cancel_backend(<pid>);        -- ends the query, keeps the session
SELECT pg_terminate_backend(<pid>);     -- last resort: ends the session and its transaction
```
Expected: connections return to the pool within seconds. The durable fix is in the code path that
held the transaction, and it belongs in the postmortem.

**M2 — move or kill the reporting load.** Cancel the long queries as above, and route reporting at
the reader endpoint rather than the writer. A report is not worth a payment.

**M3 — resolve the lock.** Identify the blocker from the query above. If it is a migration, let it
finish — killing a migration mid-way is a worse problem than the lock. If it is a human session,
find the human.

**M4 — shed, before scaling.** Lower the concurrency ceiling so the shedder drops P3/P4 first:
```bash
kubectl -n pp-data-plane set env deployment/payment-api PP_CONCURRENCY_MAX_LIMIT=128
```
Expected: pool pressure falls, single-payment reads and all writes continue. This is the correct
first move, because it reduces demand rather than multiplying supply.

**M5 — scale the *database*, not the pods.** Add reader instances and point read traffic at the
reader endpoint, or resize the writer in a change window. Increasing `PP_DATABASE_MAX_CONNS` is
only safe if the arithmetic above still holds; if it does not, you are trading a queue in the
application for a connection refusal in the database, which is strictly worse because it is not
graceful.

**M6 — roll back.**
```bash
kubectl -n pp-data-plane rollout undo deployment/payment-api
```

## Rollback / escalation

- **Do not scale replicas out of a pool-exhaustion incident.** Every new pod opens
  `PP_DATABASE_MIN_CONNS` immediately and can reach `PP_DATABASE_MAX_CONNS`. If the instance is the
  bottleneck, scaling out is how you take it down.
- **Do not raise `PP_DATABASE_STATEMENT_TIMEOUT` to make queries succeed.** The timeout is what
  keeps a pathological query from holding a connection indefinitely; raising it converts one slow
  query into a fleet-wide connection leak.
- **`pg_stat_activity` count within 10 % of `max_connections`** → Sev-2. Below that headroom,
  migrations cannot run and no operator can connect to fix anything.
- **Refunds failing** → Sev-1 regardless of pool metrics. Money-out is the last thing to shed.
- **30 minutes without improvement** → escalate to the data-platform owner; the conversation is
  about instance sizing, and that is a spend decision.

## Verification

```promql
pp_db_pool_in_use / pp_db_pool_max < 0.7
pp:payment_api_latency:p99_5m < 0.25
sum(rate(pp_http_requests_total{status="5xx"}[5m])) == 0
```
```sql
SELECT count(*) FILTER (WHERE state = 'idle in transaction') AS idle_in_txn,
       count(*)                                              AS total
FROM   pg_stat_activity;
```
`idle_in_txn` back to its normal small number, `total` comfortably below `max_connections`.
If *M4* was used, confirm `PP_CONCURRENCY_MAX_LIMIT` is back to its charted value.

## Follow-up

- Write the arithmetic down: pool size × replicas × deployments against `max_connections`, with the
  headroom reserved for migrations and operators. Put it in `docs/deployment.md` so the next
  capacity change is checked against a number rather than a memory.
- A transaction held open across a network call is a code defect with a name. File it, and add the
  lint or the test that catches the shape.
- If reporting queries were the cause, the durable fix is routing them to the reader endpoint, and
  the check is that they cannot reach the writer at all.
- Consider whether a pool-saturation alert should exist. This runbook currently has no alert of its
  own and reaches on-call as a latency burn; naming the cause directly would save minutes.
