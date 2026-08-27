# 08 — Operational Runbook: payments-api

On-call quick reference. Each procedure assumes access to: `kubectl` (via EKS access entry),
Grafana, the AWS Console/CLI (read for diagnosis, break-glass write access logged and alerted),
and this repo.

## 1. Service is returning elevated 5xx

1. Check the Service Health dashboard — is it isolated to one AZ/pod or global?
2. `kubectl get pods -n payments -o wide` — look for `CrashLoopBackOff` or pods concentrated in
   one AZ (possible AZ issue).
3. Check `CircuitBreakerOpen` alert status — if the DB breaker is open, jump to §3.
4. If a bad deploy correlates with the onset (`kubectl rollout history`), roll back immediately:
   `kubectl rollout undo deployment/payments-api -n payments`, then investigate offline.
5. If isolated to pods, `kubectl logs <pod> --previous` for the panic/error, escalate if unclear
   within 15 minutes.

## 2. `LedgerBalanceCheckFailure` alert fired (SEV-1)

This should be structurally impossible (DB constraint). Treat as a potential data-integrity
incident, not a routine bug.

1. **Do not** attempt to `UPDATE`/`DELETE` any `ledger_entries` rows.
2. Identify the affected `payment_id`(s) from the alert payload / reconciliation job output.
3. Freeze the affected account(s) (`accounts.status = 'frozen'`) to prevent further movement while
   investigating — this blocks new payments against the account but does not touch history.
4. Page the on-call engineering lead + a second senior engineer for a two-person data-integrity
   review before any corrective action.
5. Corrective action is always a new **compensating** ledger entry pair, never a mutation of
   history. Document the incident and file a postmortem regardless of root cause.

## 3. Database circuit breaker open / DB unreachable

1. Check Aurora status in the AWS Console — is a failover in progress (`Failing over` event)?
   If yes, this is expected for up to ~30-45s; confirm requests are getting fast 503s (graceful
   degradation working as designed), not hanging.
2. If failover completed but breaker still open, check connectivity from the pod (`NetworkPolicy`
   change? Security group change?) — check recent infra changes first.
3. If no failover event and DB genuinely down, escalate to AWS Support (Aurora is a managed
   service) while continuing to serve fast-failing 503s to protect client fleets from queuing.
4. Once DB confirmed healthy, breaker half-opens automatically and self-recovers — no manual reset
   needed in the normal case; manual force-reset endpoint exists for edge cases (see
   `internal/middleware/circuitbreaker.go`).

## 4. Outbox backlog growing (`OutboxBacklogGrowing`)

1. Check relay goroutine health — is it running? (`payments_outbox_relay_last_success_timestamp`
   metric should be recent.)
2. Check SQS/SNS status in AWS Console — publish errors in logs?
3. Check DB load — is the relay's polling query itself slow/starved under load? Look at
   `pg_stat_activity` for the relay's query.
4. If SQS is down (rare, AWS-managed), no data loss — backlog will drain once SQS recovers; keep
   stakeholders informed if downstream notification delay becomes customer-visible.
5. If backlog is due to relay bug, roll back to last known-good version; backlog will self-drain
   once healthy relay is running again (idempotent consumers make this safe).

## 5. DLQ depth > 0 on a consumer queue

1. Inspect a sample message body in the DLQ — identify the consumer and the failure reason
   (usually visible in the message's dead-letter metadata or the consumer's own error logs).
2. Determine if the issue is in the event payload (our bug) or the consumer (their bug).
3. If payload bug: fix, then redrive DLQ messages after deploying the fix (avoid a redrive loop —
   don't redrive before the fix is live, or messages return to the DLQ immediately).
4. If consumer bug: coordinate with the owning team; messages remain safely in the DLQ (no
   expiry-driven loss within the queue's retention window) until resolved.

## 6. AZ Failure

1. Confirm via AWS Health Dashboard.
2. Verify ALB has stopped routing to the affected AZ's targets (should be automatic).
3. Verify Aurora failover completed if the writer was in the affected AZ.
4. Confirm pod count and capacity in remaining AZs is sufficient (PodDisruptionBudget +
   `minReplicas` should already guarantee this — this is a verification step, not a fix step).
5. Do not manually rebalance unless capacity is genuinely insufficient; let Kubernetes scheduling
   and HPA handle it.

## 7. Region Failure (Disaster Recovery)

1. Confirm primary region is genuinely unavailable (not a false positive from the health check
   itself) via multiple independent signals (AWS Health, synthetic monitor from a third region,
   direct API probe).
2. Declare a DR event; assemble incident commander + comms lead per incident process.
3. Promote the Aurora Global Database secondary region replica to a standalone writer (AWS
   Console/CLI — this is a deliberate, irreversible-without-re-setup action, requires IC sign-off).
4. Update Route 53 failover record / confirm automatic failover routing has engaged.
5. Verify secondary-region `payments-api` deployment is scaled up to handle full traffic
   (secondary is warm-standby at reduced capacity by default for cost — this step scales it to
   production capacity).
6. Run the synthetic canary against the secondary region to confirm end-to-end health before
   declaring recovery complete.
7. Post-incident: do **not** simply fail back without a deliberate re-sync/cutback plan — the old
   primary region's data may have diverged or be stale; treat fail-back as its own planned
   procedure, not a reflexive action.

## 8. Rolling Back a Bad Deploy

```
kubectl rollout undo deployment/payments-api -n payments
kubectl rollout status deployment/payments-api -n payments
```

Confirm via dashboard that error rate/latency returns to baseline within 2-3 minutes. If not,
the issue may not be the code deploy (check DB/config changes deployed alongside it).

## 9. Database Schema Migration (online, zero-downtime)

Never run a blocking `ALTER TABLE` on `ledger_entries`/`payments` directly against a live table
under production load. Use the expand-contract pattern:

1. **Expand**: add the new column/index as nullable / `CONCURRENTLY`, deploy code that writes to
   both old and new shape.
2. Backfill in batches (rate-limited, monitored, resumable) — never a single giant transaction.
3. Deploy code that reads from the new shape.
4. **Contract**: once fully migrated and baked, drop the old column in a separate, later
   migration.

## 10. Certificate Expiry Emergency (auto-renewal failed)

1. Check `cert-manager` controller logs for the ACME/ACM challenge failure reason.
2. Common causes: DNS validation record propagation delay, rate limit on the CA, IAM permission
   drift for ACM.
3. Manual issuance as a bridge if needed, while fixing root cause of auto-renewal failure —
   auto-renewal failing silently and *then* needing a manual bridge is itself a process gap worth
   a postmortem action item.
