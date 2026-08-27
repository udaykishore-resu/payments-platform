# RB-006: TIMEOUT_UNKNOWN spike — payments whose money state we do not know

> **Read this line before touching anything.** Every payment counted by this alert is one where
> the platform does not know whether money moved. **No operator may retry, fail, cancel or refund
> one of these payments by hand.** Doing so is how a double charge is created. The safe commands
> are in *Immediate triage*; there is no safe manual write, and none is provided.

- **Severity:** page (P1)
- **Alert:** `TimeoutUnknownSpike`
  ```promql
  sum(rate(pp_payments_total{outcome="timeout_unknown"}[10m]))
    / clamp_min(sum(rate(pp_payments_total{outcome="created"}[10m])), 1e-9) > 0.01
  ```
- **Triggered when:** more than 1 % of created payments ended in `TIMEOUT_UNKNOWN`, sustained for
  5 minutes.
- **Plane / service:** data · `payment-orchestrator`
- **Related:** `docs/adr/ADR-013-timeout-leaves-payment-processing.md`,
  `docs/failure-handling.md` F-1, `docs/spec/00-design-baseline.md` §12.3 and §14.4,
  `docs/disaster-recovery.md` §7 (the duplicate-payment question),
  [reconciliation.md](reconciliation.md), [gateway-degradation.md](gateway-degradation.md)

## What this means

An attempt hit the **8 s hard client-side timeout** to the gateway. The request was written; the
response never arrived. The gateway may have authorized the payment, may have declined it, may
never have seen it. We cannot distinguish these from our side, so the platform does the only
correct thing: it records the attempt as `TIMEOUT_UNKNOWN`, leaves the payment in `PROCESSING`,
and **performs no retry and no failover**.

That is the whole design. A retry after an ambiguous timeout is a second authorization request for
the same money. A failover after an ambiguous timeout is the same request at a *different*
gateway. Either can double-charge a cardholder. ADR-013 chose latency and manual resolution over
that risk, permanently.

Resolution is automatic and comes from three sources, in order of speed:

1. **The gateway's webhook** arrives and carries the outcome.
2. **A status lookup** by the *deterministic gateway idempotency key* (baseline §14.4) — the same
   key the timed-out request used, so the gateway's own idempotency tells us what it did.
3. **The settlement report**, the slowest and the most authoritative.

If the reconciler cannot resolve within 15 minutes, a reconciliation exception opens
([reconciliation.md](reconciliation.md)).

A spike means one of two things: the gateway is timing out a lot (a gateway problem), or *we* are
timing out a lot (an orchestrator problem — an OOM mid-call produces exactly this, see
[orchestrator-memory.md](orchestrator-memory.md)).

## Impact

- **Merchants** see `status: "processing"` and higher latency. That is `202` semantics, and it is
  correct: we genuinely do not know yet. **No correctness loss.**
- **Money at risk:** none is *lost*, but for each affected payment there is an unresolved question
  of whether the cardholder was charged. If resolution fails, some of these become chargebacks and
  some become uncaptured authorizations that expire.
- **Degraded, not down.** New payments keep working. What is degraded is certainty.
- **The compounding risk** is the resolving path failing at the same time — if webhook ingest is
  lagging ([webhook-lag.md](webhook-lag.md)) or the gateway's lookup API is down, the backlog of
  unknowns grows with nothing draining it.

## Immediate triage (first 5 minutes)

1. Size the spike, and check whether it is one gateway or all:
   ```promql
   sum(rate(pp_payments_total{outcome="timeout_unknown"}[10m]))
     / clamp_min(sum(rate(pp_payments_total{outcome="created"}[10m])), 1e-9)
   sum by (gateway) (rate(pp_payments_total{outcome="timeout_unknown"}[10m]))
   pp_attempts_unresolved
   ```
2. **Confirm the resolving path is alive.** This is the most important question in this runbook:
   ```promql
   pp:webhook_processing_lag:p99_5m           # < 60 is healthy, > 300 pages
   rate(pp_webhooks_received_total[5m])
   rate(pp_webhooks_rejected_total[5m])
   pp_reconciliation_exceptions
   ```
3. Us or them? Latency at the ceiling with no errors is them; OOM kills are us:
   ```promql
   pp:gateway_latency:p99_5m
   sum by (gateway, class) (rate(pp_gateway_errors_total[5m]))
   increase(kube_pod_container_status_last_terminated_reason{reason="OOMKilled",container="payment-orchestrator"}[15m])
   ```
4. Count the actual unresolved population from the system of record — a read, nothing else:
   ```sql
   -- read replica; tenant context set, RLS applies
   SET LOCAL app.tenant_id = 'ten_…';
   SELECT a.gateway_id,
          count(*)                                    AS unresolved,
          min(a.request_sent_at)                      AS oldest,
          count(*) FILTER (WHERE a.request_sent_at < now() - interval '15 minutes') AS past_sla
   FROM   pp.payment_attempts a
   JOIN   pp.payments p ON p.payment_id = a.payment_id
   WHERE  a.outcome = 'TIMEOUT_UNKNOWN'
     AND  p.state   = 'PROCESSING'
   GROUP  BY a.gateway_id
   ORDER  BY unresolved DESC;
   ```
5. **The safe `platformctl` commands.** These are read-only or operate on the event plumbing, never
   on payment state. They are the complete list of what an operator may run during this incident:
   ```bash
   ./bin/platformctl version                  # what build is running
   ./bin/platformctl migrate status           # is the schema current
   ./bin/platformctl outbox status            # is payment.reconciliation_required.v1 being published
   ./bin/platformctl workflow list --state=FAILED
   ./bin/platformctl workflow dlq
   ./bin/platformctl verify-audit-chain ten_…
   ```
   **There is deliberately no `platformctl payment` command, and no command that resolves,
   retries, cancels or fails a payment.** That absence is a design decision, not a gap to work
   around with `psql`.

## Diagnosis

- **`pp:gateway_latency:p99_5m` is near or above 8 s for the affected gateway** → the gateway is
  slow and calls are hitting the hard ceiling. → *M1*, and
  [gateway-degradation.md](gateway-degradation.md) in parallel.
- **`OOMKilled` on `payment-orchestrator` in the window** → we are killing our own in-flight
  gateway calls. This is ours and it is the worst version. → [orchestrator-memory.md](orchestrator-memory.md),
  then *M2*.
- **Webhook lag high or webhook rejections rising** → the fastest resolution path is down, so
  unknowns will accumulate. → [webhook-lag.md](webhook-lag.md). This escalates severity even if
  the spike itself is modest.
- **The gateway's status lookup API is failing** (errors on the lookup operation) → the second
  resolution path is down too; expect reconciliation exceptions within 15 minutes.
  → [reconciliation.md](reconciliation.md), vendor escalation.
- **`pp_outbox_backlog` rising and `payment.reconciliation_required.v1` not publishing** → the
  reconciler is not being *told*. → [outbox.md](outbox.md). This is the quiet version of the
  failure and the one worth checking early.
- **Spike coincides with a `PP_GATEWAY_TIMEOUT` change** → someone traded ambiguity for capacity
  during another incident. → *M3*.
- **Spike is confined to one merchant or one corridor** → likely a 3DS challenge flow being held
  open. Check `three_ds_status` on the affected payments before treating it as a gateway fault.

## Mitigation

The honest framing: **there is very little to mitigate here, and that is correct.** The platform's
response is already the right one. Your job is to (a) stop *new* unknowns being created, and
(b) make sure the resolution paths are healthy so the existing ones drain.

**M1 — stop creating new unknowns: shift traffic off the slow gateway.** Not by retrying anything,
but by removing the gateway from routing so new payments go elsewhere:
```bash
./bin/platformctl config validate ./merchant-config.yaml
```
```
PUT /v1/merchants/{merchantId}/configuration
```
Expected: within the config propagation SLO (p99 ≤ 30 s) new payments route to a healthy gateway
and `pp_payments_total{outcome="timeout_unknown"}` stops growing for that gateway. Payments
already ambiguous are unaffected — they resolve on their own path.

**M2 — fix the orchestrator if we are the cause.** Raise the memory limit and roll:
```bash
kubectl -n pp-data-plane set resources deployment/payment-orchestrator \
  --limits=memory=<higher> --requests=memory=<higher>
kubectl -n pp-data-plane rollout status deployment/payment-orchestrator --timeout=5m
```
Expected: OOM kills stop, and the timeout-unknown rate falls to the gateway's own baseline.
A rolling restart drains gracefully (`preStop` 15 s, 30 s shutdown deadline), so it does not itself
create new unknowns — but do not `delete pod --force`, which does.

**M3 — restore the gateway timeout to 8 s** if a previous mitigation lowered it:
```bash
kubectl -n pp-data-plane set env deployment/payment-orchestrator PP_GATEWAY_TIMEOUT=8s
```

**M4 — unblock the resolution paths.** Whichever of [webhook-lag.md](webhook-lag.md),
[outbox.md](outbox.md) or [consumer-lag.md](consumer-lag.md) applies. This is usually the action
that actually shrinks the backlog.

**M5 — nothing.** If the gateway is simply slow, the resolution paths are healthy, and the rate is
falling: the correct action is to watch `pp_attempts_unresolved` drain and say so in the channel.
Recording "we deliberately did nothing, and here is the metric that justifies it" is a mitigation.

### What is forbidden, and why

| Tempting action | Why it is forbidden |
|---|---|
| Re-dispatch the attempt "because it probably failed" | The gateway may have authorized it. A second authorize is a second charge. |
| Mark the payment `FAILED` so the merchant can retry | The merchant then charges again for a payment that may already have succeeded. Baseline §12.3: *no timer may fail a payment* — and no human either. |
| Issue a refund "to be safe" | Refunding a payment that never captured creates a credit from nothing and an unbalanced ledger. |
| `UPDATE pp.payments SET state = …` | State transitions go through the FSM (L7), which rejects invalid ones. A direct write bypasses the guard, the version, the event log, the outbox and the audit chain — five controls at once. |
| Truncate or edit `pp.payment_attempts` | The attempt row *is* the evidence of what we sent. It is what the settlement reconciliation will be compared against. |

## Rollback / escalation

- **Escalate to Sev-1 with the payments product owner if the resolving path is also failing.** A
  growing unknown backlog with no drain is the scenario that ends in chargebacks, and it needs a
  commercial decision-maker, not just an engineer.
- **Escalate to the gateway vendor immediately** with the affected `gateway_idempotency_key`
  values and the window. Their idempotency records are the authoritative answer, and they can
  usually give it in minutes.
- **If more than ~1 000 payments are unresolved past 15 minutes**, or the oldest is over an hour,
  bring in the finance/operations contact: this stops being an engineering incident and becomes a
  reconciliation programme ([reconciliation.md](reconciliation.md)).
- **If anyone has already written to `pp.payments` or `pp.payment_attempts` by hand**: stop, freeze
  further changes, preserve the audit chain range (`./bin/platformctl verify-audit-chain ten_…`),
  and treat it as a data-integrity incident. Say so in the channel immediately — the cost of
  admitting it in minute five is a fraction of the cost of discovering it at settlement.

## Verification

```promql
# The rate must return below the threshold and stay there.
sum(rate(pp_payments_total{outcome="timeout_unknown"}[10m]))
  / clamp_min(sum(rate(pp_payments_total{outcome="created"}[10m])), 1e-9) < 0.01
# The population must actually drain, not merely stop growing.
pp_attempts_unresolved
pp_reconciliation_exceptions{severity="critical"} == 0
```
And from the system of record — this is the query that says the incident is over:
```sql
SET LOCAL app.tenant_id = 'ten_…';
SELECT count(*) AS still_unresolved,
       min(a.request_sent_at) AS oldest
FROM   pp.payment_attempts a
JOIN   pp.payments p ON p.payment_id = a.payment_id
WHERE  a.outcome = 'TIMEOUT_UNKNOWN' AND p.state = 'PROCESSING';
```
`still_unresolved` must trend to zero. Every payment that entered the window must have reached a
terminal state through the normal path — resolved by webhook, by lookup, or by settlement.

## Follow-up

- Record: peak rate, total payments affected, how each one resolved (webhook / lookup /
  settlement / exception), and the time to resolve the last one.
- Any payment resolved by the settlement report rather than by webhook or lookup is a finding: the
  two fast paths did not work for it, and knowing why is the improvement.
- If the cause was gateway latency, the vendor conversation is about their p99.9, with our
  measurements attached.
- If the cause was our OOM, the fix is the memory profile, and the durable fix is the limit being
  derived from measured heap rather than copied from another service.
- Confirm the chaos coverage still holds:
  `tests/chaos/gateway_test.go::TestGatewayTimeoutLeavesPaymentProcessingAndNeverRetries`. If this
  incident had a shape that test does not produce, add it.
- If anyone was tempted by a manual write, that temptation is a documentation gap. Make this
  runbook's forbidden-actions table the thing they find first.
