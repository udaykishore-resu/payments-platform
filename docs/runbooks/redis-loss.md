# RB-020: Redis unavailable — and why correctness is unaffected

- **Severity:** ticket (P2, `page: "false"`)
- **Alert:** `RedisUnavailable`
  ```promql
  redis_up == 0
  ```
- **Triggered when:** Redis has been unreachable for 1 minute.
- **Plane / service:** data · `redis`
- **Related:** `docs/adr/ADR-009-postgres-authoritative-idempotency.md`,
  `docs/spec/00-design-baseline.md` §14.3, `docs/failure-handling.md` F-7 and F-20,
  [idempotency.md](idempotency.md), [payment-api-latency.md](payment-api-latency.md)

## Why this is a P2 and not a P1

**Redis is an accelerator, never a source of truth.** That is ADR-009, and it is the reason this
alert does not page. Concretely, every use of Redis has an authoritative fallback:

| Use | Fallback when Redis is gone | Correctness |
|---|---|---|
| Idempotency claim cache | **Postgres**, which is authoritative anyway | Unchanged. A duplicate still replays the stored response |
| Distributed rate limiting | Local per-pod token bucket sized `global_limit / replicas × 1.2` | Coarser. The ×1.2 over-admits to account for uneven load balancing |
| Configuration / snapshot cache | Local in-process snapshot, then Postgres | Unchanged, within the staleness bound |
| Velocity counters (L5 risk) | Degrade to the **risk policy's posture**, not to "allow" | Unchanged in intent; the database still holds every guard that protects money |

The client's own circuit breaker opens after 10 failures in 5 s and probes every 5 s, so the
fallback engages within seconds rather than on every request timing out.

Nothing in the platform reads a value from Redis and treats it as the truth about money. That is
not a convention — it is the property ADR-009 exists to guarantee, and it is what makes a Redis
outage a latency event rather than a correctness event.

## Impact

- **p99 latency up roughly 15–30 ms.** Real, and visible on the latency SLO if it was already close
  to the 250 ms budget.
- **Higher database load**, because the idempotency read now goes to Postgres every time. Watch the
  pool.
- **Rate limits slightly coarser.** The per-pod bucket over-admits by design.
- **No correctness loss. No money at risk. No merchant sees a wrong answer.**
- The compounding risk is second-order: added latency can provoke client retries, and client
  retries provoke an in-progress storm ([idempotency.md](idempotency.md)).

## Immediate triage (first 5 minutes)

1. Confirm it is really unreachable, and from where:
   ```promql
   redis_up
   ```
   ```bash
   kubectl -n pp-data-plane exec deploy/payment-api -- \
     sh -c 'nc -z -w2 $PP_REDIS_ADDR_HOST $PP_REDIS_ADDR_PORT && echo OK || echo UNREACHABLE'
   aws elasticache describe-replication-groups \
     --query 'ReplicationGroups[].{Id:ReplicationGroupId,Status:Status,Nodes:MemberClusters}'
   ```
2. **Confirm the fallbacks engaged** — this is what makes it a P2 rather than a P1:
   ```promql
   sum by (outcome) (rate(pp_idempotency_outcomes_total[5m]))    # still flowing
   sum by (status) (rate(pp_http_requests_total{route="/v1/payments"}[5m]))
   ```
   Idempotency outcomes continuing at normal rates means Postgres is carrying it.
3. Watch what the fallback costs:
   ```promql
   pp:payment_api_latency:p99_5m
   pp_db_pool_in_use / pp_db_pool_max
   ```
4. Rule out TLS as the cause, since it is the most common misconfiguration here:
   ```bash
   kubectl -n pp-data-plane get deploy payment-api -o jsonpath=\
   '{range .spec.template.spec.containers[0].env[?(@.name=="PP_REDIS_TLS")]}{.name}={.value}{"\n"}{end}'
   ```
   `PP_REDIS_TLS` defaults to `true`; a plaintext endpoint with TLS on, or a private-CA endpoint
   without `PP_REDIS_TLS_CA_FILE` and `PP_REDIS_TLS_SERVER_NAME`, both look like unreachability.
5. Check for an in-progress storm forming:
   ```promql
   sum(rate(pp_idempotency_outcomes_total{outcome="in_progress"}[5m]))
     / clamp_min(sum(rate(pp_idempotency_outcomes_total[5m])), 1e-9)
   ```

## Diagnosis

- **ElastiCache reports a failover or a node replacement** → transient; multi-AZ handles it.
  → *M1*.
- **The cluster is healthy but pods cannot reach it** → network policy, security group, or DNS.
  → *M2*.
- **TLS/handshake errors in the logs** → `PP_REDIS_TLS` disagrees with the endpoint, or the CA
  material is missing. → *M3*.
- **Auth errors** → the credential rotated and the pods have the old one. → *M4*.
- **Memory eviction or OOM on the node** → sizing. → *M5*.
- **`pp_db_pool_in_use / pp_db_pool_max` climbing toward 1** → the fallback is loading Postgres
  more than it can take. This is the one way a Redis outage becomes serious.
  → [db-pool-exhaustion.md](db-pool-exhaustion.md).
- **Latency SLO burning** → [payment-api-latency.md](payment-api-latency.md).
- **A spike in `pp_config_loads_total`-style cache reload with flat request volume** → cache
  stampede (F-20). Single-flight per key, TTL jitter and serve-stale should prevent it; if they
  did not, that is the finding.

## Mitigation

**M1 — wait, with a deadline.** For an ElastiCache failover the correct action is to confirm the
fallbacks are working and wait. Set a 15-minute deadline; the client breaker will close on its own
when the node returns.

**M2 — restore reachability.** Re-apply network policy from Git:
```bash
kubectl -n pp-data-plane apply -k deployments/k8s/overlays/<env>
```

**M3 — fix the TLS configuration.** The right fix is to supply the CA material, not to turn TLS
off:
```bash
kubectl -n pp-data-plane set env deployment/payment-api \
  PP_REDIS_TLS=true PP_REDIS_TLS_CA_FILE=/etc/pp/redis-ca.pem PP_REDIS_TLS_SERVER_NAME=<name>
```
Those two variables exist precisely so that "turn the encryption off" is never the only workaround
— a missing knob that leaves disabling TLS as the only path is how TLS gets disabled.

**M4 — rotate credentials properly** via the dual-run workflow
([security-credential-rotation.md](security-credential-rotation.md)).

**M5 — resize or scale ElastiCache.** A planned change; the fallback holds meanwhile.

**M6 — protect Postgres while Redis is gone.** If the fallback load is threatening the pool, shed
rather than scale:
```bash
kubectl -n pp-data-plane set env deployment/payment-api PP_CONCURRENCY_MAX_LIMIT=128
```

## Rollback / escalation

- **Do not disable TLS to restore Redis.** An unencrypted cache connection inside the VPC is still a
  control removed under pressure, and it will outlive the incident.
- **Do not fail payments because Redis is down.** There is no code path that requires it, and if you
  believe you have found one, that is a Sev-1 design defect — report it as such rather than working
  around it.
- **Do not point idempotency at anything other than Postgres.** Postgres *is* the authority; Redis
  was only ever in front of it.
- **Redis down for more than 1 hour, or pool pressure rising** → escalate to Sev-2. The latency and
  database cost compound.
- **If the risk engine's velocity counters are unavailable and the policy posture is "block"**,
  merchants will see declines. That is the policy working (ladder rung 5: fall back to the policy
  default, *not* to "allow"), but the payments product owner should know.

## Verification

```promql
redis_up == 1
pp:payment_api_latency:p99_5m < 0.25
pp_db_pool_in_use / pp_db_pool_max < 0.7
sum by (outcome) (rate(pp_idempotency_outcomes_total[5m]))
```
**Verify the fallback is actually off**, not merely that Redis is up — the client breaker closes
after successful probes, and a service still on its local token bucket is still over-admitting:
```bash
kubectl -n pp-data-plane logs deploy/payment-api --since=5m \
  | grep -iE 'redis.*(breaker|fallback|degraded)' | tail
```
Then confirm the invariant that matters:
```sql
SET LOCAL app.tenant_id = 'ten_…';
SELECT payment_id, count(*) FROM pp.payment_attempts
WHERE  outcome = 'SUCCESS' GROUP BY payment_id HAVING count(*) > 1;
```
Zero rows — no duplicate execution happened while the cache was gone. This is the thing the design
promises, and it is worth checking rather than assuming.

## Follow-up

- Record the outage duration, the p99 delta, and the peak database pool utilisation. The last one
  is the number that decides whether the fallback is sized for a longer outage.
- If TLS configuration was the cause, the chart is the fix. A `kubectl set env` that is not in Git
  disappears at the next sync.
- Confirm the chaos coverage: `tests/chaos/redis_loss_test.go::TestIdempotencyCorrectWithoutRedis`
  kills Redis mid-burst with duplicate keys and asserts every duplicate replays the identical
  stored response and no operation executes twice. That test is the evidence behind this runbook's
  central claim; keep it passing.
- If the fallback load nearly exhausted the database pool, size the pool against the
  Redis-unavailable case, not the happy path.
