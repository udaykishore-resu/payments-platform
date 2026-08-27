# RB-014: Config snapshot stale — the fail-static cliff

- **Severity:** ticket (`ConfigSnapshotStale`, P2) · page (`ConfigSnapshotCliff`, P1)
- **Alert:** `ConfigSnapshotStale`
  ```promql
  pp_config_snapshot_age_seconds > 300
  ```
  `ConfigSnapshotCliff`
  ```promql
  pp_config_snapshot_age_seconds > 840
  ```
- **Triggered when:** a service's merchant-configuration snapshot is older than 300 s for
  2 minutes (warning, ten minutes of headroom); or older than 840 s for 1 minute — **60 seconds
  before the 900 s `max_config_staleness` cliff**.
- **Plane / service:** data · label `slo: config_propagation`
- **Related:** `docs/adr/ADR-019-fail-static-configuration.md`,
  `docs/spec/00-design-baseline.md` §15, `docs/failure-handling.md` F-13 and §4 rung 10,
  [control-plane.md](control-plane.md)

## What this means

The data plane never calls the control plane on the payment path. It holds a **snapshot** of
merchant configuration, refreshed on a timer, and it **fails static**: if the control plane is
unreachable, it keeps processing payments on the last-known-good snapshot.

That tolerance is bounded, and the bound has a cliff:

| Age | Behaviour |
|---|---|
| < 30 s | Normal. The propagation SLO is p99 ≤ 30 s |
| 30 s – 300 s | Serving a slightly stale snapshot. Reported unhealthy on the config check, still serving |
| 300 s | `ConfigSnapshotStale` — the warning, with ten minutes of headroom |
| 840 s | `ConfigSnapshotCliff` — **one minute to react** |
| 900 s (`max_config_staleness`) | **Fail closed for new merchants.** Existing merchants continue on the last-known-good snapshot. Ladder rung 10 |

The environment variables behind those numbers are `PP_CONFIG_REFRESH_INTERVAL` (10 s),
`PP_CONFIG_MAX_STALENESS` (30 s, the bounded-staleness window) and `PP_CONFIG_CLIFF_STALENESS`
(the cliff). Note the alert thresholds are written against the documented 900 s production cliff;
if a deployment sets `PP_CONFIG_CLIFF_STALENESS` differently, the alert thresholds must move with
it or the warning stops being a warning.

The cliff is deliberately conservative in one direction only: past it, the platform refuses to
authorize against a configuration nobody can vouch for. Refusing money is the safe failure; taking
money under unknown limits, unknown routing and unknown risk policy is not.

## Impact

- **Below the cliff: nothing merchant-visible.** Payments are processed on a slightly old
  snapshot. A merchant whose configuration changed in the last few minutes is still being served
  by the old one — usually invisible, occasionally a routing decision that has just been changed.
- **At the cliff: newly-onboarded merchants cannot transact.** Their configuration was never in
  the snapshot, so there is nothing to fall back to. Existing merchants keep working.
- **Money at risk: none.** No payment is processed incorrectly; some are refused. That is the trade
  ADR-019 makes explicitly.
- **Degraded, not down** — right up until the cliff, and then only for a slice of merchants.

## Immediate triage (first 5 minutes)

On `ConfigSnapshotCliff` you have **one minute** before the cliff. Do steps 1 and 2, then mitigate.

1. Which services, and how old?
   ```promql
   pp_config_snapshot_age_seconds
   topk(10, pp_config_snapshot_age_seconds)
   max(pp_config_snapshot_age_seconds)
   ```
   All services affected ⇒ the source is down. One service ⇒ that pod's refresh loop.
2. Is the control plane up?
   ```promql
   pp:control_plane_error:ratio_rate5m
   ```
   ```bash
   kubectl -n pp-control-plane get pods -l app=control-plane-api
   kubectl -n pp-control-plane logs deploy/control-plane-api --since=10m | tail -40
   ```
3. Is the invalidation path (Kafka) the problem rather than the control plane?
   ```promql
   kafka_cluster_partition_underreplicated
   pp_outbox_backlog
   pp_consumer_lag
   ```
4. The refreshing service's own view:
   ```bash
   kubectl -n pp-data-plane logs deploy/payment-orchestrator --since=10m \
     | grep -iE 'config|snapshot|staleness|checksum' | tail -30
   ```
5. Are merchants actually being refused yet?
   ```promql
   sum by (status) (rate(pp_http_requests_total{route="/v1/payments"}[5m]))
   ```

## Diagnosis

- **Every service stale, control-plane pods down or erroring** → the control plane is the cause.
  → [control-plane.md](control-plane.md), then *M1*.
- **Every service stale, control plane healthy** → the refresh path between them: network policy,
  the config Kafka topic, or a credential. → *M2*.
- **One service stale, others fine** → that deployment's refresh loop is wedged. → *M3*.
- **One *pod* stale, siblings fine** → that pod. → *M3* (delete it).
- **Age flat and high rather than climbing** → the refresh is succeeding but returning the same old
  document: the publisher stopped publishing, or a checksum mismatch is causing the snapshot to be
  rejected on load. Look for checksum errors in the logs. → *M4*.
- **A bad configuration was published just before this** → L4 validation should have rejected it at
  publish, so a corrupt document should never reach the data plane. If one did, that is a
  validation defect. → *M5*.
- **`kafka_cluster_partition_underreplicated > 0`** → invalidation has degraded to TTL-based
  expiry, which is a *bounded* 30 s staleness and should not reach 300 s. If it has, the TTL path
  is also broken. → [kafka.md](kafka.md).

## Mitigation

**M1 — restore the control plane.** The data plane recovers on its own within one refresh interval
(10 s) once the source answers. See [control-plane.md](control-plane.md). Expected:
`pp_config_snapshot_age_seconds` drops to near zero across the fleet within ~30 s.

**M2 — restore the path.** Re-apply network policy from Git and confirm reachability from a data
plane pod:
```bash
kubectl -n pp-data-plane apply -k deployments/k8s/overlays/<env>
kubectl -n pp-data-plane exec deploy/payment-orchestrator -- \
  sh -c 'wget -qO- http://control-plane-api.pp-control-plane:8080/healthz || echo UNREACHABLE'
```

**M3 — restart the stale consumer of the config.** The cheapest correct action for a wedged refresh
loop, and it forces a synchronous first load at startup:
```bash
kubectl -n pp-data-plane rollout restart deployment/payment-orchestrator
kubectl -n pp-data-plane rollout status deployment/payment-orchestrator --timeout=5m
```
For a single pod: `kubectl -n pp-data-plane delete pod <name>`. Expected: age resets to near zero
for the replaced pods immediately.

**M4 — republish the configuration.** If the publisher stopped, publishing any valid document
refreshes the snapshot chain. Validate first — this is what `platformctl config validate` is for,
and it runs the same L4 checks without publishing:
```bash
./bin/platformctl config validate ./merchant-config.yaml
```
```
PUT /v1/merchants/{merchantId}/configuration
```

**M5 — roll back a bad configuration.**
```
POST /v1/merchants/{merchantId}/configuration/rollback
```
This publishes the previous document as a **new version**; it never deletes (baseline §23). List
versions first:
```
GET /v1/merchants/{merchantId}/configuration/versions
```

**M6 — buy time at the cliff, deliberately and temporarily.** Raising
`PP_CONFIG_CLIFF_STALENESS` extends how long the data plane will serve an old snapshot:
```bash
kubectl -n pp-data-plane set env deployment/payment-orchestrator PP_CONFIG_CLIFF_STALENESS=30m
```
**This is a considered trade, not a fix.** You are choosing to authorize payments against a
configuration that is up to 30 minutes old — old limits, old routing, old risk policy — in exchange
for not refusing new merchants. Do it only with the incident commander's agreement, record the
decision and the time, and revert it in the same incident. Never leave it raised.

## Rollback / escalation

- **`ConfigSnapshotCliff` gives you 60 seconds.** Do not spend them diagnosing. Restart the stale
  consumers (*M3*) — it is fast, it is safe, and if the source is genuinely down you will find out
  immediately because the age will climb straight back.
- **Past the cliff** → new-merchant payments are being refused. Notify the payments product owner
  and onboarding: merchants activated in the last hour are the affected population, and they will
  be calling.
- **Do not disable the staleness check.** The cliff is the control that prevents authorizing under
  unknown limits. Turning it off converts a bounded, visible, partial refusal into an unbounded,
  invisible risk.
- **If a corrupt document reached the data plane**, that is a validation-plane defect and a Sev-2
  in its own right: L4 is supposed to make it impossible.
- **Control plane down for more than 15 minutes** → escalate per [control-plane.md](control-plane.md);
  the data plane's fail-static window is the clock on that escalation.

## Verification

```promql
max(pp_config_snapshot_age_seconds) < 30
pp:config_propagation:p99_5m < 30
sum(rate(pp_http_requests_total{route="/v1/payments",status="5xx"}[5m])) == 0
```
Both alerts must clear on their own. Confirm a *new* merchant can transact — that is the population
the cliff refuses, and it is the only proof the cliff has actually been cleared. If *M6* was used,
verify `PP_CONFIG_CLIFF_STALENESS` is back to its charted value:
```bash
kubectl -n pp-data-plane get deployment payment-orchestrator \
  -o jsonpath='{range .spec.template.spec.containers[0].env[?(@.name=="PP_CONFIG_CLIFF_STALENESS")]}{.value}{"\n"}{end}'
```

## Follow-up

- Record maximum snapshot age reached, whether the cliff was crossed, and for how long.
- If the cliff was crossed, count the merchants refused. That number is the impact, and it belongs
  in the merchant-facing follow-up.
- If the alert thresholds and `PP_CONFIG_CLIFF_STALENESS` disagreed, fix that. A warning that fires
  after the thing it warns about is worse than no warning.
- Confirm the chaos coverage:
  `tests/chaos/config_staleness_test.go::TestDataPlaneServesStaticThenFailsClosedAtCliff` asserts
  continued processing at 14 minutes and cliff behaviour at 16, with existing merchants still
  served. If this incident's shape differs, extend it.
