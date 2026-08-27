# RB-023: payment-orchestrator OOMKilled

- **Severity:** ticket (P2, `page: "false"`)
- **Alert:** `PaymentOrchestratorOOMKilled`
  ```promql
  increase(kube_pod_container_status_last_terminated_reason{reason="OOMKilled",container="payment-orchestrator"}[15m]) > 0
  ```
- **Triggered when:** any OOM kill of the orchestrator container in the last 15 minutes,
  `for: 0m`.
- **Plane / service:** data · `payment-orchestrator`
- **Related:** `docs/observability.md` §3.5 (why this is one of only two resource alerts),
  `docs/failure-handling.md` F-1, [timeout-unknown.md](timeout-unknown.md),
  `docs/adr/ADR-013-timeout-leaves-payment-processing.md`

## What this means

CPU, memory and pod-restart alerts are **deliberately absent** from this platform's alert
catalogue: they are symptoms with no reliable relationship to user pain, and they belong on
dashboards and in postmortems. There are exactly two exceptions, and this is one of them (the other
is disk fill on stateful nodes).

It is here because **the orchestrator holds in-flight gateway calls**. An OOM kill mid-call
produces `TIMEOUT_UNKNOWN` attempts — money in an unknown state. The kill is not the incident; the
ambiguity it manufactures is.

An OOM is a `SIGKILL`. There is no graceful shutdown, no `preStop`, no drain. Every in-flight
gateway call on that pod is severed at the moment the kernel decides, and each one becomes a
payment where we do not know whether money moved.

## Impact

- **Per kill: one pod's worth of in-flight gateway calls become ambiguous.** With a per-gateway
  bulkhead of 200 in-flight platform-wide, a single pod can be holding tens of calls.
- Each of those payments stays `PROCESSING`, gets no retry and no failover, and resolves via
  webhook, gateway lookup, or the settlement report ([timeout-unknown.md](timeout-unknown.md)).
- Capacity drops until the pod is replaced; the remaining replicas absorb it.
- **No correctness loss and no double charge** — the design holds. What is lost is certainty, and
  operator time to recover it.
- Repeated kills produce a growing unresolved backlog and, at a high enough rate,
  `TimeoutUnknownSpike` and then reconciliation exceptions.

## Immediate triage (first 5 minutes)

1. How many, which pods, and is it ongoing?
   ```promql
   increase(kube_pod_container_status_last_terminated_reason{reason="OOMKilled",container="payment-orchestrator"}[1h])
   ```
   ```bash
   kubectl -n pp-data-plane get pods -l app=payment-orchestrator \
     -o custom-columns='NAME:.metadata.name,RESTARTS:.status.containerStatuses[0].restartCount,REASON:.status.containerStatuses[0].lastState.terminated.reason'
   kubectl -n pp-data-plane describe pod <pod> | sed -n '/Last State/,/Ready/p'
   ```
2. **Measure the money consequence immediately** — this is the reason the alert exists:
   ```promql
   sum(rate(pp_payments_total{outcome="timeout_unknown"}[10m]))
   pp_attempts_unresolved
   pp_reconciliation_exceptions
   ```
3. Memory shape — is this a leak, a spike, or a limit that was always too low?
   ```promql
   container_memory_working_set_bytes{container="payment-orchestrator"}
   kube_pod_container_resource_limits{container="payment-orchestrator",resource="memory"}
   go_memstats_heap_inuse_bytes{service="payment-orchestrator"}
   go_gc_duration_seconds{service="payment-orchestrator"}
   ```
   A sawtooth that climbs between GCs and never returns is a leak. A single vertical spike is one
   request. A flat line touching the limit is a limit that was always wrong.
4. Correlate with a deploy or a load change:
   ```bash
   kubectl -n pp-data-plane rollout history deployment/payment-orchestrator
   ```
   ```promql
   pp:payments:tps5m
   pp_gateway_bulkhead_in_use / pp_gateway_bulkhead_capacity
   ```
5. Confirm whether payments are still flowing on the surviving replicas:
   ```promql
   sum by (status) (rate(pp_http_requests_total{route="/v1/payments"}[5m]))
   ```

## Diagnosis

- **Heap climbs monotonically across GC cycles** → a leak. → *M1* to stop the bleeding, then the
  defect.
- **A vertical spike coinciding with one request** → an unbounded allocation on some input: a large
  payload, an unbounded slice, a response the adapter buffers whole. → *M2*.
- **Working set flat at the limit with no growth** → the limit is simply too low for the workload.
  → *M1*.
- **Started at a deploy** → *M3*.
- **`pp_gateway_bulkhead_in_use / capacity` near 1 and gateway latency high** → slow gateways are
  holding more in-flight calls, each with its buffers. Memory pressure is a *consequence* of
  [gateway-degradation.md](gateway-degradation.md). → fix that first.
- **Kills across every replica at once** → load-driven, not a per-pod defect. → *M1* plus *M4*.
- **`pp_attempts_unresolved` is climbing** → the money consequence is real and accumulating.
  → [timeout-unknown.md](timeout-unknown.md) runs in parallel and takes precedence for escalation.

## Mitigation

**M1 — raise the memory limit and request.** The fastest action that stops manufacturing ambiguity:
```bash
kubectl -n pp-data-plane set resources deployment/payment-orchestrator \
  --limits=memory=<higher> --requests=memory=<higher>
kubectl -n pp-data-plane rollout status deployment/payment-orchestrator --timeout=5m
```
Set request equal to limit for a latency-sensitive, memory-shaped workload so the pod is
`Guaranteed` and is not evicted under node pressure. Expected: kills stop. A rolling update drains
gracefully (`preStop` 15 s, then a 30 s shutdown deadline), so it does **not** create new
`TIMEOUT_UNKNOWN` attempts — unlike the OOM it is replacing. Land the change in Git the same day.

**M2 — bound the input.** If one request shape is responsible, the durable fix is a bound on it,
not a bigger limit. A limit raised to accommodate an unbounded allocation is a limit that will be
reached again.

**M3 — roll back.**
```bash
kubectl -n pp-data-plane rollout undo deployment/payment-orchestrator
```

**M4 — scale out and reduce per-pod pressure.** More replicas share the bulkhead capacity, so each
holds fewer in-flight calls:
```bash
kubectl -n pp-data-plane scale deployment/payment-orchestrator --replicas=<higher>
```
Check the database pool arithmetic first ([db-pool-exhaustion.md](db-pool-exhaustion.md)).

**M5 — shorten the gateway timeout, carefully.** Fewer concurrent in-flight calls means less memory
held:
```bash
kubectl -n pp-data-plane set env deployment/payment-orchestrator PP_GATEWAY_TIMEOUT=4s
```
**This trades memory for ambiguity** — more attempts end `TIMEOUT_UNKNOWN`, which is the exact harm
this runbook is trying to reduce. Use it only when the alternative is continued OOM kills, and
restore `8s` in the same incident.

**M6 — resolve the ambiguity the kills already created.** This is not optional cleanup; it is half
the job. [timeout-unknown.md](timeout-unknown.md), and confirm the resolution paths are healthy:
```bash
./bin/platformctl outbox status
```
```promql
pp:webhook_processing_lag:p99_5m
```

## Rollback / escalation

- **Never `kubectl delete pod --force`** on the orchestrator. It is `SIGKILL` by hand: it creates
  exactly the ambiguity the OOM created, deliberately.
- **Repeated kills with `pp_attempts_unresolved` climbing** → escalate to Sev-2 and run
  [timeout-unknown.md](timeout-unknown.md) in parallel. The money question outranks the memory one.
- **`TimeoutUnknownSpike` fires** → Sev-1 path; that runbook takes precedence.
- **Do not raise the limit past the node's allocatable capacity** — you move an OOM kill into an
  eviction, which is the same severing with a different label.
- **Do not "fix" the unresolved payments by hand.** Nothing about an OOM changes the rule: no
  operator retries, fails, cancels or refunds an ambiguous payment.

## Verification

```promql
increase(kube_pod_container_status_last_terminated_reason{reason="OOMKilled",container="payment-orchestrator"}[1h]) == 0
container_memory_working_set_bytes{container="payment-orchestrator"}
  / kube_pod_container_resource_limits{container="payment-orchestrator",resource="memory"} < 0.7
sum(rate(pp_payments_total{outcome="timeout_unknown"}[10m])) # back to gateway baseline
pp_attempts_unresolved                                        # draining to zero
pp_reconciliation_exceptions{severity="critical"} == 0
```
And confirm every payment caught by a kill reached a terminal state:
```sql
SET LOCAL app.tenant_id = 'ten_…';
SELECT count(*) FROM pp.payment_attempts a JOIN pp.payments p USING (payment_id)
WHERE  a.outcome = 'TIMEOUT_UNKNOWN' AND p.state = 'PROCESSING';
```
Trending to zero. The alert clearing is not sufficient — the ambiguity it created outlives it.

## Follow-up

- Record: number of kills, in-flight calls lost (estimated from bulkhead occupancy at the time),
  `TIMEOUT_UNKNOWN` attempts created, and how each one resolved.
- Derive the memory limit from a measured heap profile under peak bulkhead occupancy, and write the
  derivation next to the number. A limit copied from another service is a limit nobody can defend.
- If a leak: the pprof heap profile belongs in the postmortem, and the fix belongs in a test.
- If gateway slowness drove the memory pressure, the durable fix is in
  [gateway-degradation.md](gateway-degradation.md)'s territory, not here.
- Ask whether the OOM should have been predicted. A working-set trend crossing 80 % of the limit is
  visible days ahead on a dashboard; if nobody looked, that is a review-cadence finding rather than
  an engineering one.
