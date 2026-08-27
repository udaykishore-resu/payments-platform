# RB-022: Webhook processing lag / ingress slow

- **Severity:** page (`WebhookProcessingLagHigh`, P1) · ticket (`WebhookIngressSlow`, P2)
- **Alert:** `WebhookProcessingLagHigh`
  ```promql
  pp:webhook_processing_lag:p99_5m > 300
  ```
  `WebhookIngressSlow`
  ```promql
  histogram_quantile(0.99, sum by (le)
    (rate(pp_http_request_duration_seconds_bucket{service="webhook-ingress"}[5m]))) > 0.05
  ```
- **Triggered when:** webhook processing p99 exceeds 300 s for 5 minutes (the SLO is 60 s; the page
  threshold is 5× it, because between them lies "busy", not "broken"); or `webhook-ingress` p99
  exceeds its 50 ms accept budget for 5 minutes.
- **Plane / service:** data · `webhook-ingress` (label `slo: webhook_lag`)
- **Related:** `docs/failure-handling.md` F-11, F-12, `docs/payment-flow.md`,
  [timeout-unknown.md](timeout-unknown.md), [reconciliation.md](reconciliation.md),
  [consumer-lag.md](consumer-lag.md)

## What this means

Two alerts about two different halves of the same path.

`webhook-ingress` **accepts and persists, nothing more.** It verifies the gateway's signature,
dedupes on `(gateway, gateway_ref)`, writes the row, returns 200. Its budget is 50 ms. Exceeding it
means it is doing work it should not be, or the write path is slow — and the consequence compounds,
because gateways time out and **retry**, which multiplies the load on the thing that is already
slow.

Processing lag is the *other* half: from receipt to the state transition being applied. Gateway
webhooks are how asynchronous payment outcomes reach us, so sustained lag turns directly into
reconciliation exceptions — a webhook that arrives on time but is processed 15 minutes late is,
from the reconciler's point of view, a webhook that did not arrive.

This is why `WebhookProcessingLagHigh` pages: it is the fastest of the three resolution paths for
an ambiguous payment ([timeout-unknown.md](timeout-unknown.md)), and losing it makes every
`TIMEOUT_UNKNOWN` slower and more likely to become an exception.

Duplicates are expected and harmless: the dedup constraint drops them and returns **200**, because
a gateway that receives a non-2xx retries harder and makes things worse.

## Impact

- **Payment creation: unaffected.** New payments are authorized normally.
- **Asynchronous outcomes are late**: 3DS completions, captures confirmed out of band, disputes,
  and — the important one — resolutions for payments sitting in `PROCESSING`.
- **Merchants see stale payment state.** A payment the gateway settled minutes ago still reads
  `PROCESSING`.
- **Reconciliation exceptions accumulate** if lag persists past the reconciler's 15-minute window.
- **On ingress slowness**, gateways retry, so inbound volume multiplies — the classic feedback loop.

## Immediate triage (first 5 minutes)

1. Which half is broken?
   ```promql
   pp:webhook_processing_lag:p99_5m
   histogram_quantile(0.99, sum by (le)
     (rate(pp_http_request_duration_seconds_bucket{service="webhook-ingress"}[5m])))
   rate(pp_webhooks_received_total[5m])
   rate(pp_webhooks_rejected_total[5m])
   ```
2. Rejections deserve immediate attention — a signature-failure spike is a security signal, not a
   performance one:
   ```promql
   sum by (gateway, reason) (rate(pp_webhooks_rejected_total[5m]))
   sum(rate(pp_security_events_total{type="WEBHOOK_SIGNATURE_INVALID"}[5m]))
   ```
   More than 10/min per gateway is the threshold at which `docs/security.md` §9.1 pages security.
   → *M5*.
3. Is the ingress or the processing side the bottleneck?
   ```bash
   kubectl -n pp-data-plane get pods -l app=webhook-ingress
   kubectl -n pp-data-plane logs deploy/webhook-ingress --since=10m | tail -50
   ```
   ```promql
   pp_db_pool_in_use / pp_db_pool_max
   pp_consumer_lag
   pp_outbox_backlog
   ```
4. The queue from the system of record:
   ```sql
   SET LOCAL app.tenant_id = 'ten_…';
   SELECT gateway, count(*) AS pending,
          min(received_at) AS oldest,
          now() - min(received_at) AS max_age
   FROM   pp.inbound_webhooks
   WHERE  processed_at IS NULL
   GROUP  BY gateway ORDER BY pending DESC;
   ```
5. **Measure the downstream consequence**, because that is what makes this a page:
   ```promql
   pp_attempts_unresolved
   pp_reconciliation_exceptions
   ```

## Diagnosis

- **Ingress p99 above 50 ms with high inbound volume** → gateway retries are multiplying load.
  Break the loop. → *M1*.
- **Ingress p99 high with normal volume, database pool saturated** →
  [db-pool-exhaustion.md](db-pool-exhaustion.md), then *M2*.
- **Ingress fast, processing lag high, `pp_consumer_lag` high** → the processing side is behind, not
  the accept side. → [consumer-lag.md](consumer-lag.md).
- **Ingress fast, processing lag high, consumer lag normal** → a slow handler or a stuck partition.
  → *M3*.
- **`pp_outbox_backlog` growing** → downstream of processing, not the webhook path itself.
  → [outbox.md](outbox.md).
- **Signature rejections spiking for one gateway** → they rotated their signing secret, or someone
  is forging. → *M5*.
- **Duplicate rate high** → our acknowledgement is too slow, so gateways are retrying deliveries we
  already have. Check the 50 ms budget; duplicates are a *symptom* of ingress slowness, not a
  problem in themselves. → *M1*.
- **Replay rejections (`WEBHOOK_REPLAY_DETECTED`)** → timestamp skew beyond ±5 min or nonce reuse.
  Check node clocks (F-19). → *M6*.

## Mitigation

**M1 — scale the ingress and cut its per-request work.**
```bash
kubectl -n pp-data-plane scale deployment/webhook-ingress --replicas=<higher>
kubectl -n pp-data-plane rollout status deployment/webhook-ingress --timeout=5m
```
Expected: p99 back under 50 ms, and the gateway retry rate falls as acknowledgements speed up —
which reduces inbound volume, which is the loop closing in the right direction.

**M2 — relieve the write path.** [db-pool-exhaustion.md](db-pool-exhaustion.md). The ingress does
one small insert per webhook; if that is slow, the database is the problem, not the service.

**M3 — increase processing throughput.**
```bash
kubectl -n pp-data-plane set env deployment/event-consumer \
  PP_WORKER_CONCURRENCY=8 PP_WORKER_BATCH_SIZE=200
```
Expected: `pp:webhook_processing_lag:p99_5m` falls within a few minutes.

**M4 — protect the resolution path first.** If `TIMEOUT_UNKNOWN` is elevated, prioritise processing
the webhooks for payments in `PROCESSING` over everything else. Those are the ones where lag turns
into a money question.

**M5 — signature failures.** Verify against the gateway's **rotation state** before assuming an
attack — a rotated signing secret produces exactly this signature. If it is a rotation, complete it
via the dual-run workflow ([security-credential-rotation.md](security-credential-rotation.md)). If
it is not, the source IP is already throttled at the WAF after 10/min and this becomes a security
incident ([security-events.md](security-events.md)). **Never disable signature verification.** An
unverified webhook endpoint is an unauthenticated way to change payment state.

**M6 — fix clock skew.** Signature windows tolerate ±5 min. Beyond 5 s of offset a node is
cordoned; below that, resync NTP (F-19).

## Rollback / escalation

- **Never disable signature verification**, at any severity, for any duration.
- **Never return a non-2xx to make gateways back off.** A non-2xx makes them retry *harder*. If you
  must shed, the honest answer is to fix the accept path.
- **Never mark webhooks processed without processing them.** Each one is a state transition; a
  webhook marked processed and not applied is a payment permanently in the wrong state.
- **Lag above 900 s, or reconciliation exceptions rising** → Sev-1. The resolution path for
  ambiguous money is effectively down. Escalate with [reconciliation.md](reconciliation.md) and
  [timeout-unknown.md](timeout-unknown.md) in the same incident.
- **Signature failures above 10/min per gateway** → page security per `docs/security.md` §9.1.
  Verify against rotation state first, but escalate in parallel rather than after.
- **If a gateway reports delivery failures on their side**, ask them **not** to disable the
  endpoint. Their retry with backoff is the recovery mechanism; disabling it loses the outcomes.

## Verification

```promql
pp:webhook_processing_lag:p99_5m < 60
histogram_quantile(0.99, sum by (le)
  (rate(pp_http_request_duration_seconds_bucket{service="webhook-ingress"}[5m]))) < 0.05
rate(pp_webhooks_rejected_total[5m]) == 0
pp_consumer_lag < 1000
```
```sql
SET LOCAL app.tenant_id = 'ten_…';
SELECT count(*) AS pending, min(received_at) AS oldest
FROM   pp.inbound_webhooks WHERE processed_at IS NULL;
```
`pending` back to a small number with a recent `oldest`. Then confirm the **consequence** cleared,
which is the real test:
```promql
pp_attempts_unresolved                          # draining
pp_reconciliation_exceptions{severity="critical"} == 0
```

## Follow-up

- Record: peak lag, backlog size, and the number of reconciliation exceptions attributable to the
  window. That last number is what justifies the P1.
- If gateway retries amplified the incident, the finding is the 50 ms budget: it exists so that
  amplification cannot start, and it was breached.
- If a signing-secret rotation caused rejections, the dual-run overlap was too short or was not
  used. That is a process fix in `docs/security.md` §5.3's territory.
- Confirm the chaos coverage:
  `tests/integration/webhook_test.go::TestDuplicateWebhookIsDroppedByTheUniqueIndex` (the same webhook
  concurrently, exactly one state transition) and
  `tests/chaos/clock_skew_test.go::TestClockSkewBeyondTheWebhookToleranceFailsClosed`.
