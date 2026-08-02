# 04 — Failure & Recovery Design

Every row below follows: **Failure → Detection → Automatic Recovery → Manual Recovery → Prevention**.
Scoped to the payments-api feature and its immediate dependencies.

## Compute / Process Failures

| Failure | Detection | Automatic Recovery | Manual Recovery | Prevention |
|---|---|---|---|---|
| Pod crash (panic, OOM) | Kubernetes liveness probe fails 3x (15s) | Kubelet restarts container; if `CrashLoopBackOff`, HPA/ReplicaSet keeps desired count via other healthy pods | On-call inspects `kubectl logs --previous`, panic stack trace shipped to error tracker | `recover()` middleware on every handler converts panics to 500s instead of crashing the process; resource limits sized from load-test memory profile to avoid OOM |
| Node failure | Node `NotReady` for 40s (kubelet lease) | Kubernetes reschedules pods to healthy nodes in other AZs (pod anti-affinity spreads replicas across AZs so a single node loss never drops below quorum) | SRE cordons/drains node, files infra ticket if hardware fault | PodDisruptionBudget + `minReplicas` ensures capacity headroom for one node's worth of pods to be missing without breach |
| AZ failure | Multiple node `NotReady` in one AZ, ALB health checks fail for that AZ's targets | ALB stops routing to unhealthy AZ; EKS reschedules pods to remaining AZs; Aurora auto-fails-over if the writer was in the affected AZ (~30s) | SRE confirms AZ status via AWS Health Dashboard, executes AZ-failure section of runbook | Every tier (ALB, EKS nodes, Aurora replicas) is provisioned across ≥3 AZs by default; capacity planning assumes N-1 AZ available |
| Region failure | Cross-region synthetic monitoring fails; Route 53 health check fails | Route 53 failover routing shifts traffic to secondary region (DNS TTL-bound, not instant) | SRE promotes Aurora Global Database secondary to standalone writer, verifies replication lag was within RPO budget, executes region-failover runbook, communicates to stakeholders | DR drill run quarterly (game-day) to keep RTO/RPO numbers honest, not theoretical |
| Memory leak | Prometheus alert on steadily rising `go_memstats_heap_inuse_bytes` over hours, not spiky | HPA may add pods to spread load, delaying but not fixing | Rolling restart of affected pods; root-cause via pprof heap profile captured before restart | Load testing includes long-duration soak tests specifically to catch slow leaks before prod |
| CPU spike | HPA CPU-based scaling target breached | HPA scales out additional pods within ~60-90s | If sustained and scaling maxed out, on-call investigates hot code path (pprof CPU profile), may need to shed load via rate limiter | Rate limiting + circuit breakers prevent a single bad client or dependency from driving unbounded CPU |
| Deadlock/goroutine leak | `go_goroutines` metric climbs unboundedly; readiness probe eventually times out | Kubernetes restarts pod once liveness fails | On-call captures `pprof` goroutine dump before restart if reproducible | Every DB call and outbound HTTP call has an explicit `context.Context` timeout — nothing blocks forever by construction |

## Data & Storage Failures

| Failure | Detection | Automatic Recovery | Manual Recovery | Prevention |
|---|---|---|---|---|
| Database failure (writer down) | RDS/Aurora event, connection errors spike, circuit breaker opens | Aurora automated failover promotes a reader (~30s); app's circuit breaker returns fast 503s instead of queuing requests during the gap | SRE verifies failover completion, checks replica lag before declaring green | Connection pool configured with sane timeouts + retry-with-backoff on transient connection errors, not infinite retry |
| Disk / storage failure | Aurora storage layer is self-healing (6-way replication); surfaced only as an AWS Health event | Transparent — Aurora repairs from healthy copies automatically | None typically required; SRE monitors AWS Health Dashboard | N/A — this is why we chose Aurora over self-managed storage (ADR-002) |
| Data corruption (application bug writes bad data) | Reconciliation job detects ledger entries for a `payment_id` not summing to zero (should be structurally impossible due to DB constraint, but reconciliation exists as defense-in-depth), or audit-log anomaly alert | None — this requires a human decision | Incident: freeze affected accounts, restore from point-in-time recovery to a scratch instance to diff, write a compensating ledger entry (ledgers are corrected via new offsetting entries, **never** via `UPDATE`/`DELETE` on historical rows) | DB-level `CHECK` constraint makes the specific "unbalanced ledger" corruption class structurally impossible; append-only audit log; least-privilege DB role has no `UPDATE`/`DELETE` grant on `ledger_entries` or `audit_log` |
| Backup/restore failure | Automated backup-verification job periodically restores latest snapshot to a scratch instance and runs a smoke query | If verification fails, page on-call immediately (a backup you haven't tested restoring is not a backup) | Investigate snapshot pipeline, re-run backup | Point-in-time recovery enabled (5-min granularity); quarterly full DR restore drill |

## Messaging / Queue Failures

| Failure | Detection | Automatic Recovery | Manual Recovery | Prevention |
|---|---|---|---|---|
| SQS/SNS outage | Outbox relay publish calls start erroring; `outbox_unpublished_count` climbs | Relay retries with exponential backoff + jitter; events remain safely durable in `outbox_events` (not lost) since they're only marked published on confirmed success | If prolonged, SRE monitors AWS Health Dashboard; no data-loss risk since source of truth is the DB row, not the queue | Outbox pattern by design decouples "payment committed" from "event delivered" — a queue outage delays but never loses financial events |
| Duplicate messages (SQS at-least-once) | Expected, not a failure | Consumers are contractually idempotent on `event_id` | N/A | Event envelope includes stable `event_id`; consumer idempotency is a documented, enforced contract (ADR-003) |
| Lost messages | Reconciliation job cross-checks `payments.status=completed` count vs. downstream-acked event count | Outbox relay retries until DLQ threshold | On-call inspects DLQ, redrives after fixing root cause | DLQ + redrive policy on every queue; alert on any DLQ depth > 0 |
| Out-of-order events | Not expected within a single `payment_id` (SNS→SQS-standard doesn't guarantee order) | Consumers key on `event_id`/`occurred_at` and are designed to tolerate reordering for this event type (idempotent upsert-by-latest-timestamp) | N/A | Event schema includes `occurred_at`; consumers documented to not assume ordering unless using a FIFO queue |
| Retry storm / thundering herd | Sudden spike in identical requests after a client-side outage resolves | Per-client rate limiter + circuit breaker shed load predictably (fast 429/503) rather than falling over | On-call identifies offending client via metrics labels, can apply temporary tighter limit | Clients required (by API contract) to use exponential backoff + jitter on retries; server-side rate limiting as the backstop regardless of client behavior |

## Dependency / Config / Human Failures

| Failure | Detection | Automatic Recovery | Manual Recovery | Prevention |
|---|---|---|---|---|
| Downstream dependency slow/unavailable | Circuit breaker trip alert; latency histogram P99 spike | Circuit breaker opens after threshold failures, fails fast instead of exhausting connection pool / goroutines waiting | On-call checks dependency status, may manually keep breaker forced-open longer via feature flag | Every external call wrapped in circuit breaker + bulkhead (bounded worker pool) + timeout, from day one — not bolted on later |
| Certificate expiration | cert-manager auto-renews at 30 days-to-expiry; Prometheus alert if cert TTL < 14 days as a backstop | cert-manager renews automatically via ACME/ACM integration | If auto-renewal fails, on-call manually rotates via runbook | Automated renewal + expiry alerting as defense-in-depth, never manual-only certs |
| Secret rotation | Scheduled rotation via AWS Secrets Manager; app picks up new DB credentials via short-TTL cache + refresh | App reloads credentials without restart (credential provider polls Secrets Manager) | If app doesn't pick up rotation in time, rolling restart forces refresh | DB credentials never baked into images/config; always fetched at runtime from Secrets Manager |
| Bad deployment / config mistake | Automated canary analysis (error rate / latency SLO comparison, new version vs. baseline) fails post-deploy | CD pipeline automatically halts rollout and rolls back on canary failure | On-call can force manual rollback (`kubectl rollout undo` / re-deploy last known-good tag) | Progressive delivery (canary %, then full), not big-bang deploys; every deploy gated by the production checklist |
| Race condition on concurrent ledger writes to same account | `SERIALIZABLE` isolation surfaces as a `40001` serialization-failure error | App retries the transaction automatically (bounded retries with backoff) | If retries exhausted, request fails with 409 to client, who retries (idempotency key makes this safe) | `SERIALIZABLE` isolation chosen specifically so concurrency bugs are caught by Postgres, not by hoping application logic is race-free |
| Deadlock | Postgres detects and aborts one transaction automatically (`deadlock_detected`) | App retries the aborted transaction | On-call investigates if deadlock rate climbs (may indicate a lock-ordering bug) | Consistent lock acquisition order (always lock lower account ID first) documented and enforced in code review |
| Split brain (two "writers" believing they're primary) | Not applicable to Aurora (single-writer architecture with managed failover prevents this by design) | N/A | N/A | Choosing a managed single-writer database (ADR-002) specifically avoids having to solve distributed consensus ourselves |
| Leader election failure (outbox relay) | N/A by design — the relay doesn't use leader election | `FOR UPDATE SKIP LOCKED` lets every pod's relay goroutine safely compete for rows without needing a leader | N/A | Deliberately avoided needing leader election for this component (simpler failure mode) |
| Clock skew | NTP sync via EKS-managed nodes (chrony); alert if node clock drift > 1s | Kubernetes/AWS-managed nodes keep NTP sync automatically | On-call investigates node if drift alert fires | Application logic never trusts wall-clock ordering across nodes for correctness (uses DB-generated `created_at`/sequence, not app-generated timestamps, for anything ordering-sensitive) |

## Recovery Objectives Summary

| Scenario | RTO | RPO |
|---|---|---|
| Pod/node failure | < 1 min (automatic) | 0 |
| AZ failure | < 2 min (automatic) | 0 |
| Region failure | ≤ 60 min (manual promotion + verification) | ≤ 5 sec (Aurora Global Database typical lag) |
| Data corruption incident | Varies — hours (investigation-bound) | 0 for ledger (compensating entries, never data loss since original rows are immutable) |
