# RB-004: Gateway degradation — authorization-rate drop, circuit open, errors, latency

- **Severity:** page (`PaymentAuthorizationRateDrop`, P1) · ticket (`GatewayCircuitOpen`,
  `GatewayErrorRateHigh`, `GatewayLatencyHigh`, all P2)
- **Alert:**
  ```promql
  # PaymentAuthorizationRateDrop — 10m
  (pp:payment_authorization_rate:baseline7d - pp:payment_authorization_rate:ratio_rate30m) > 0.05
    and sum by (gateway) (rate(pp_payments_total[30m])) > 0.5
  # GatewayCircuitOpen — 2m
  pp_circuit_breaker_state == 2
  # GatewayErrorRateHigh — 3m
  pp:gateway_error:ratio_rate5m > 0.25
  # GatewayLatencyHigh — 5m
  pp:gateway_latency:p99_5m > 5
  ```
- **Triggered when:** authorization rate is more than 5 percentage points below its 7-day baseline
  for 10 minutes with more than 0.5 payments/s of volume; or a `(gateway, operation)` circuit has
  been OPEN for 2 minutes; or its error ratio has exceeded 25 % for 3 minutes; or its p99 has
  exceeded 5 s for 5 minutes.
- **Plane / service:** data · `payment-orchestrator`
- **Related:** `docs/failure-handling.md` F-2, F-3, F-4 and §2.4 (breaker state machine),
  `docs/spec/00-design-baseline.md` §10 (gateway health FSM),
  `docs/observability.md` §1.5 (the two-minute alert→payment path),
  [no-eligible-gateway.md](no-eligible-gateway.md)

## What this means

Four alerts, one mechanism, so one runbook. The gateway health FSM moves
`HEALTHY → DEGRADED → UNHEALTHY` on error rate and latency; `UNHEALTHY` opens the circuit; an open
circuit removes the gateway from the routing plan and traffic shifts to the next candidate. That
failover is automatic — which is why three of these four are tickets.

Two facts change how you read them:

- **Declines are not errors.** `pp:gateway_error:ratio_rate5m` counts timeouts, transport failures
  and 5xx. A gateway saying *no* cleanly does not open a circuit. So `GatewayErrorRateHigh` means
  the gateway or the network is broken; `PaymentAuthorizationRateDrop` with flat errors means the
  gateway is *declining*, which is a completely different branch.
- **`PaymentAuthorizationRateDrop` is the only alert here that catches money quietly not moving
  while every technical SLI looks fine.** The baseline is offset by 1 h so the incident cannot
  lower its own threshold, and the 0.5 payments/s volume guard stops a low-volume gateway paging
  on three declines.

`pp_circuit_breaker_state`: `0` CLOSED, `1` HALF_OPEN, `2` OPEN. Cool-down is 30 s, doubling on
each failed probe, capped at 5 minutes; three consecutive probe successes close it.

## Impact

- **Circuit open / errors / latency**: traffic shifts gateway. Merchants may see a different
  authorization rate and different 3DS behaviour, and latency rises by the retry budget
  (≈1.3 s worst case). No payment is lost. If it is the *last* healthy gateway, this becomes
  [no-eligible-gateway.md](no-eligible-gateway.md) and merchants get 503.
- **Authorization-rate drop**: merchants are losing sales right now, silently. Every point of
  authorization rate is revenue. This is why it pages while the others do not.
- **Latency high**: every slow call holds a bulkhead slot (200 platform-wide per gateway, 32 per
  tenant), so a latency problem becomes a capacity problem for the orchestrator before it becomes
  a latency problem for the merchant.

## Immediate triage (first 5 minutes)

1. Scope: one gateway, or all of them?
   ```promql
   pp_circuit_breaker_state
   sum by (gateway) (rate(pp_payments_total{outcome="authorized"}[5m]))
     / sum by (gateway) (rate(pp_payments_total{outcome=~"authorized|declined|failed"}[5m]))
   ```
   If **every** `operation="authorize"` circuit is 2, stop here → [no-eligible-gateway.md](no-eligible-gateway.md).
2. Declining or failing? This is the fork the whole runbook turns on:
   ```promql
   sum by (class) (rate(pp_gateway_errors_total{gateway="$gw"}[5m]))
   sum by (reason_code) (rate(pp_payment_declines_total{gateway="$gw"}[5m]))
   pp:gateway_latency:p99_5m{gateway="$gw"}
   ```
3. Narrow it — currency, method, corridor:
   ```promql
   sum by (currency, payment_method) (rate(pp_payments_total{gateway="$gw",outcome="declined"}[5m]))
   ```
4. Bulkhead pressure, because that is what turns this into an outage:
   ```promql
   pp_gateway_bulkhead_in_use / pp_gateway_bulkhead_capacity
   ```
5. Pull one failing payment end to end (exemplar → trace → logs, `docs/observability.md` §1.5),
   then confirm against the system of record:
   ```sql
   -- read replica; tenant context set, RLS applies
   SET LOCAL app.tenant_id = 'ten_…';
   SELECT a.attempt_id, a.gateway_id, a.attempt_number, a.outcome, a.state,
          a.decline_reason_code, a.normalized_error_code, a.gateway_idempotency_key,
          a.request_sent_at, a.response_received_at
   FROM   pp.payment_attempts a
   WHERE  a.payment_id = 'pay_…'
   ORDER  BY a.request_sent_at;
   ```
6. Check the vendor's status page and open a ticket with them **now**, in parallel. Vendor tickets
   have their own latency and starting one costs nothing.

## Diagnosis

- **Errors are `timeout` or `transport` class** → the gateway or the network between us and it.
  Failover has already happened. → *M1* (confirm the fallback is absorbing), vendor ticket.
- **Errors are `contract_violation`** → **we** changed something, or they did without telling us.
  Check the last adapter deploy. → *M4*.
- **Zero errors, latency flat, declines up, concentrated in one BIN range or one issuer** → an
  issuer or scheme problem, not ours and not the gateway's. → *M5*.
- **Zero errors, declines up, concentrated in one merchant** → a merchant configuration change
  (a new descriptor, a changed MCC, a risk-policy edit). Check their configuration versions.
  → *M6*.
- **Zero errors, declines up across every merchant on one gateway** → the gateway's own risk
  engine changed posture. → *M5* plus vendor ticket.
- **Declines up on **all** gateways at once** → look at us: a routing change, a risk-policy
  deploy, or `ConfigSnapshotStale`. → *M4*, [config-staleness.md](config-staleness.md).
- **Latency high and `pp_gateway_bulkhead_in_use / capacity` approaching 1** → the slow calls are
  eating the bulkhead; the orchestrator will start returning 503 before the gateway does. → *M2*.
- **Circuit flapping between 1 and 2** → probes are failing; the cool-down is doubling. Leave it
  alone unless it is the last gateway; that is the FSM working. → *M3* only if the fallback is
  materially worse.

## Mitigation

**M1 — let failover work, and verify it is.** The default action for a single degraded gateway is
*nothing*. Confirm the fallback is absorbing:
```promql
sum by (gateway) (rate(pp_payments_total{outcome="authorized"}[5m]))
sum by (gateway) (rate(pp_routing_decisions_total[5m]))
```
Expected: volume on the fallback rises by roughly what the degraded gateway lost, and the total
authorized rate returns to baseline. If total volume fell instead of shifting, failover is *not*
working and this is now a P1.

**M2 — reduce the gateway timeout so slow calls stop holding bulkhead slots.** The hard per-call
ceiling is `PP_GATEWAY_TIMEOUT` (8 s default, baseline §12 stage 14):
```bash
kubectl -n pp-data-plane set env deployment/payment-orchestrator PP_GATEWAY_TIMEOUT=4s
kubectl -n pp-data-plane rollout status deployment/payment-orchestrator --timeout=5m
```
Expected: slot turnover rises, `pp_gateway_bulkhead_in_use` falls, the circuit opens sooner and
failover happens faster. **Cost: more attempts end `TIMEOUT_UNKNOWN`** — you are trading capacity
for ambiguity, so read [timeout-unknown.md](timeout-unknown.md) before doing this, and put the
value back in the same incident.

**M3 — pin routing away from the gateway.** A merchant's routing policy is a configuration
document; publish a new version with the gateway removed or deprioritised:
```
PUT /v1/merchants/{merchantId}/configuration
```
and if it goes wrong, `POST /v1/merchants/{merchantId}/configuration/rollback`, which publishes the
previous document as a *new* version rather than deleting anything (baseline §23). Validate before
publishing:
```bash
./bin/platformctl config validate ./that-document.yaml
```
Expected: within one config-propagation interval (SLO p99 ≤ 30 s) the routing plans stop naming
that gateway. Do **not** do this for a transient circuit-open — the FSM recovers on its own and a
pin is a thing someone has to remember to remove.

**M4 — roll back the orchestrator or the adapter.**
```bash
kubectl -n pp-data-plane rollout undo deployment/payment-orchestrator
```

**M5 — vendor escalation.** Open the ticket with: the gateway's own reference IDs from
`pp.payment_attempts.gateway_reference`, the window, the affected corridors, and the decline
reason-code histogram. Do not send payloads.

**M6 — merchant contact.** If the drop is one merchant's configuration, tell them what changed and
which version to roll back to:
```
GET /v1/merchants/{merchantId}/configuration/versions
```

## Rollback / escalation

- **Never retry a hard decline on another gateway.** Stolen card, invalid account, pickup and
  restricted are terminal. Retrying them elsewhere is card-testing behaviour and gets the platform
  de-registered from the schemes (`docs/failure-handling.md` F-3, baseline §9.1). No exception,
  no matter what a merchant asks for.
- **The authorization-rate drop persists past 30 minutes** → Sev-1, involve the payments product
  owner: this is revenue leaving, and the decision to route around a gateway commercially is not
  the on-call's.
- **The degraded gateway is the last healthy one for a corridor** → this is now
  [no-eligible-gateway.md](no-eligible-gateway.md). Escalate immediately.
- **Two gateways degrade simultaneously** → suspect **us**, not them. Check the egress path, the
  NAT/egress proxy and DNS before opening two vendor tickets (`docs/failure-handling.md` F-5's
  manual step says exactly this).
- **`TimeoutUnknownSpike` fires alongside** → money is in an unknown state. That runbook takes
  precedence over this one.

## Verification

```promql
pp_circuit_breaker_state{gateway="$gw"} == 0
pp:gateway_error:ratio_rate5m{gateway="$gw"} < 0.05
pp:gateway_latency:p99_5m{gateway="$gw"} < 5
(pp:payment_authorization_rate:baseline7d - pp:payment_authorization_rate:ratio_rate30m) < 0.05
pp_gateway_bulkhead_in_use / pp_gateway_bulkhead_capacity < 0.5
```
The circuit must reach `0` through its own probe sequence (three consecutive successes), not
because traffic stopped. If you set `PP_GATEWAY_TIMEOUT` in *M2*, confirm it is back to `8s` and
that `pp_payments_total{outcome="timeout_unknown"}` has returned to baseline.

## Follow-up

- Record the authorization-rate delta in percentage points and the estimated payment volume lost;
  that number is what makes the vendor conversation concrete.
- If failover did not absorb the shift, the routing plan lacked a viable fallback for that
  corridor. That is a configuration gap, and it is the finding.
- If the cause was `contract_violation`, add the case to the gateway contract suite
  (`make test-contract`) so the next adapter change fails in CI instead of in production.
- If a routing pin was applied, file the ticket to remove it with an owner and a date. Pins that
  outlive their incident are how a platform ends up with one gateway.
