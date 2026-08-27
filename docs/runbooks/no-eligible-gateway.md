# RB-005: No eligible gateway — every circuit open

- **Severity:** page (both P1)
- **Alert:** `AllGatewaysUnhealthy`
  ```promql
  count(pp_circuit_breaker_state{operation="authorize"} == 2)
    == count(pp_circuit_breaker_state{operation="authorize"})
  ```
  `NoEligibleGatewayErrors`
  ```promql
  sum(rate(pp_http_requests_total{status="5xx",route="/v1/payments"}[5m])) > 0
    and on() count(pp_circuit_breaker_state{operation="authorize"} == 2) > 0
  ```
- **Triggered when:** every configured gateway's authorize circuit has been OPEN for 1 minute; or
  `/v1/payments` has been returning 5xx for 2 minutes while at least one circuit is open.
- **Plane / service:** data · `payment-orchestrator`, `payment-api`
- **Related:** `docs/failure-handling.md` F-5 and §4 rung 8, `docs/spec/00-design-baseline.md`
  §10, [gateway-degradation.md](gateway-degradation.md)

## What this means

The routing engine returned an empty plan and the platform **failed closed**: `503
NO_ELIGIBLE_GATEWAY` with `Retry-After`, before any attempt is dispatched. No payment row is left
in a non-terminal state, because the failure happens before the gateway call.

The platform does not route outside a merchant's configured or residency-permitted gateway set.
That is why "just send it somewhere" is not an option and is not a mitigation below.

Alertmanager **inhibits every other `plane=data` alert** while `AllGatewaysUnhealthy` fires. If
you are holding three pages, this is the one that matters and the others are its shadow.

The single most important prior: **two gateways failing simultaneously is far more often our
network than their platform.** Check our egress before opening two vendor tickets.

## Impact

New payments are rejected platform-wide for the affected `(currency, method, corridor)`. Merchants
get a retryable 503; well-behaved SDKs back off and retry, so the sale is deferred rather than
lost — for a few minutes.

Preserved by rung 8 of the degradation ladder: **refunds, voids, dispute handling and webhook
ingest continue.** Money-out survives longer than money-in, deliberately. Captures of already
authorized payments depend on the same circuits and are affected.

No correctness loss and no money at risk: nothing was dispatched.

## Immediate triage (first 5 minutes)

1. Confirm the scope, and get the count:
   ```promql
   pp_circuit_breaker_state{operation="authorize"}
   count(pp_circuit_breaker_state{operation="authorize"} == 2)
   count(pp_circuit_breaker_state{operation="authorize"})
   ```
2. **Check us before them.** Egress, DNS, and the mesh:
   ```bash
   kubectl -n pp-data-plane exec deploy/payment-orchestrator -- \
     sh -c 'getent hosts api.<gateway-host> || echo DNS_FAIL'
   kubectl -n pp-data-plane logs deploy/payment-orchestrator --since=10m \
     | grep -iE 'egress|proxy denied|x509|certificate|no route|connection refused' | head -20
   kubectl -n pp-data-plane get networkpolicy
   ```
3. Error classes across gateways — a single shared class is the signature of a shared cause:
   ```promql
   sum by (gateway, class) (rate(pp_gateway_errors_total[5m]))
   ```
   All `transport` on every gateway ⇒ our network. Mixed classes per gateway ⇒ genuinely theirs.
4. Credentials, since an expired credential looks like a gateway outage:
   ```promql
   (time() - pp_gateway_credential_created_timestamp_seconds) / 86400
   ```
5. Confirm what merchants are actually receiving:
   ```promql
   sum by (status) (rate(pp_http_requests_total{route="/v1/payments"}[5m]))
   ```
6. Check the last deploy of the orchestrator and of any adapter, and the last config publish.

## Diagnosis

- **DNS resolution failing inside the pod** → our resolver or the egress NetworkPolicy. → *M1*.
- **`egress proxy denied` in the logs** → an allowlist entry was removed or a destination changed.
  → *M1*. Also raise a security event: a denied egress destination is a Page-security signal
  (`docs/security.md` §9.1).
- **x509 / certificate errors** → an expired CA bundle in the image, or the gateway rotated its
  chain. → *M2*.
- **All classes are `timeout`, latency at the ceiling** → the network path is up but slow. → *M3*.
- **One gateway is genuinely down and the others were already pinned away / deprovisioned** →
  the routing configuration has no real fallback. → *M4*.
- **Credentials expired or a rotation half-completed** → → *M5*.
- **An orchestrator or adapter deploy is inside the window** → *M6*.
- **Circuits are open but the gateways answer fine from a laptop** → the breaker state is stale or
  wrong; probes are not running. → *M6* (restart forces re-probing).

## Mitigation

**M1 — restore the egress path.** Re-apply the NetworkPolicy / egress allowlist from Git:
```bash
kubectl -n pp-data-plane apply -k deployments/k8s/overlays/<env>
kubectl -n pp-data-plane rollout status deployment/payment-orchestrator --timeout=5m
```
Expected: transport errors stop within seconds; circuits half-open after their cool-down (30 s,
doubling, capped at 5 min) and close after three successful probes. Typical full recovery 1–5 min.

**M2 — fix the trust chain.** If the gateway rotated its CA, the fix is an image with the current
bundle. Do **not** disable verification. There is no version of this incident that is improved by
an unauthenticated connection to a payment gateway.

**M3 — shorten the gateway timeout so probes can complete.**
```bash
kubectl -n pp-data-plane set env deployment/payment-orchestrator PP_GATEWAY_TIMEOUT=4s
```
Expected: faster failure detection and faster probe cycles. Cost: more `TIMEOUT_UNKNOWN`
attempts — read [timeout-unknown.md](timeout-unknown.md) first, and restore `8s` afterwards.

**M4 — restore a fallback gateway in the merchant configuration.** Validate, then publish:
```bash
./bin/platformctl config validate ./merchant-config.yaml
```
```
PUT /v1/merchants/{merchantId}/configuration
```
Expected: routing plans include the restored gateway within the config propagation SLO (p99 ≤ 30 s).
This is only a mitigation if the gateway is actually healthy — check
`GET /v1/gateways/{gatewayId}/health` first.

**M5 — rotate credentials via the dual-run workflow.**
```
POST /v1/gateways/{gatewayId}/credentials:rotate
```
Do **not** delete the old credential before the audit snapshot
([security-credential-rotation.md](security-credential-rotation.md)).

**M6 — roll back, or force re-probing.**
```bash
kubectl -n pp-data-plane rollout undo deployment/payment-orchestrator     # if a deploy correlates
kubectl -n pp-data-plane rollout restart deployment/payment-orchestrator  # to reset breaker state
```
A restart resets in-memory breaker state, so every gateway is re-probed immediately instead of
waiting out a doubled cool-down. Expected: circuits reach `0` within one probe cycle if the
underlying problem is gone. **If it is not gone, a restart makes things worse**, because every
replica now stampedes the failing gateway at once.

## Rollback / escalation

- **Page the incident commander immediately.** This is a full stop on new payments; it is a Sev-1
  from the first minute, not after a threshold.
- **5 minutes with no identified cause** → bring in the network/platform on-call in parallel. The
  prior says the cause is ours; two people looking at two hypotheses is faster than one serial.
- **15 minutes unresolved** → notify the payments product owner and prepare merchant comms. The
  message is honest and specific: payments are being rejected with a retryable error, refunds and
  voids are unaffected, retries will succeed on recovery.
- **Do not route outside the merchant's configured set.** Residency and licensing constraints are
  not performance settings, and a payment routed to a gateway a merchant has not contracted with
  is a commercial and regulatory problem that outlives the outage.
- **Do not fail the in-flight payments by hand.** There are none to fail — the rejection happens
  before dispatch. If you believe otherwise, you are in [timeout-unknown.md](timeout-unknown.md).

## Verification

```promql
count(pp_circuit_breaker_state{operation="authorize"} == 2) == 0
sum(rate(pp_http_requests_total{status="5xx",route="/v1/payments"}[5m])) == 0
sum by (gateway) (rate(pp_payments_total{outcome="authorized"}[5m])) > 0
pp:payment_authorization_rate:ratio_rate30m
```
Both alerts clear on their own. Confirm the authorization rate returns to its 7-day baseline, not
merely that payments are flowing: a recovery that authorizes at half the usual rate is
[gateway-degradation.md](gateway-degradation.md), not a fixed incident.

Confirm no payment was stranded:
```sql
SELECT state, count(*) FROM pp.payments
WHERE  created_at > now() - interval '2 hours' GROUP BY state ORDER BY 2 DESC;
```
There should be no growth in `PROCESSING` attributable to the window.

## Follow-up

- The core question for the postmortem: **why did every gateway fail at once?** If the answer is
  "shared dependency", that shared dependency is the finding, and the action is to remove it or to
  monitor it directly rather than through its symptoms.
- If the routing configuration had no viable fallback, that is a configuration defect. Add a
  validation rule that a production routing policy names at least two gateways per corridor.
- Verify the inhibition rule worked: exactly one page should have been delivered. If the rotation
  got thirty, the Alertmanager inhibition is broken and that is its own ticket.
- Add or extend the case: `internal/application/payment/service_test.go::TestNoEligibleGatewayIsAnAnswerNotJustARefusal`
  asserts 503, `Retry-After`, and no payment left non-terminal. If this incident's shape is not
  covered there, add it.
