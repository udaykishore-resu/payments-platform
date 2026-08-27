# RB-025: OpenTelemetry trace export failing

- **Severity:** ticket (P3, `page: "false"`)
- **Alert:** `TraceExportFailing`
  ```promql
  rate(otelcol_exporter_send_failed_spans[5m]) > 100
  ```
- **Triggered when:** the collector has been dropping more than 100 spans/second for 10 minutes.
- **Plane / service:** observability
- **Related:** `docs/observability.md` §2 (SDK configuration, collector topology, sampling),
  §1.5 (the alert→payment path that depends on traces), [cardinality.md](cardinality.md)

## What this means

Spans are being dropped at the collector. Traces are the pillar that answers *"why is this one
payment slow"* — the exemplar → trace → logs path in §1.5 that takes an on-call from an alert to the
exact failing payment in under two minutes.

Losing them **degrades diagnosis, not service**, which is why it is a P3. But it degrades diagnosis
exactly when incidents make diagnosis matter, which is why it is alerted at all rather than left to
a dashboard.

The topology: an `otel-agent` DaemonSet receives from the SDKs, an `otel-gateway` Deployment does
tail sampling and exports. Head sampling keeps 10 % by default
(`PP_TRACE_SAMPLE_RATIO`), with errors and money-path spans kept regardless by the sampler's own
override — so a drop here is not evenly distributed across the traces you care about, it is
disproportionately expensive.

## Impact

- **No merchant impact. No money at risk. Nothing is degraded for anyone outside the team.**
- Incident diagnosis gets slower. The two-minute alert→payment path becomes a log search by
  merchant and timestamp — which is the anti-pattern §1.5 exists to replace, and which requires
  knowing the merchant first, which is exactly what an alert does not tell you.
- If the agent is applying backpressure to the SDK rather than dropping, application latency can
  rise slightly. Check for it rather than assuming it away.

## Immediate triage (first 5 minutes)

P3 pace. Same business day.

1. Where are spans being lost — agent or gateway?
   ```promql
   rate(otelcol_exporter_send_failed_spans[5m])
   rate(otelcol_exporter_sent_spans[5m])
   rate(otelcol_receiver_refused_spans[5m])
   rate(otelcol_processor_dropped_spans[5m])
   otelcol_exporter_queue_size / otelcol_exporter_queue_capacity
   ```
2. Are the collectors healthy?
   ```bash
   kubectl -n pp-observability get pods -l app.kubernetes.io/name=opentelemetry-collector
   kubectl -n pp-observability logs -l app=otel-gateway --since=10m | tail -60
   kubectl -n pp-observability top pods
   ```
3. Is the backend accepting?
   ```promql
   rate(otelcol_exporter_send_failed_spans[5m]) by (exporter)
   ```
   Errors on one exporter and not others isolate it to that backend.
4. Is span volume unusual?
   ```promql
   rate(otelcol_receiver_accepted_spans[5m])
   pp:payments:tps5m
   ```
   Volume up with traffic flat means something started emitting far more spans per request.
5. Is the SDK being back-pressured? Check for a latency effect before dismissing this as harmless:
   ```promql
   pp:payment_api_latency:p99_5m
   ```

## Diagnosis

- **`otelcol_exporter_queue_size / capacity` at 1 with export errors** → the backend is rejecting or
  throttling. → *M1*.
- **Collector pods OOMKilled or CPU-throttled** → the collector is undersized for the current span
  volume. → *M2*.
- **`otelcol_receiver_refused_spans` non-zero** → the collector is refusing at intake; memory
  limiter engaged. → *M2*.
- **Span volume rose sharply with flat traffic** → a deploy added instrumentation, or a loop is
  creating a span per iteration. → *M3*.
- **One exporter failing, others fine** → that backend. → *M1*.
- **Started at a collector config change** → *M4*.
- **Tail sampling is dropping deliberately** → not a failure. `otelcol_processor_dropped_spans`
  rising while `send_failed_spans` stays flat is sampling, not loss. Read the two metrics
  separately before acting.

## Mitigation

**M1 — reduce export pressure.** Lower the head sampling ratio so fewer spans are produced. Errors
and money-path spans are kept by the sampler's override regardless, so this loses the least
valuable traces first:
```bash
kubectl -n pp-data-plane set env deployment/payment-api PP_TRACE_SAMPLE_RATIO=0.01
```
Expected: span volume falls ~10×; the queue drains. Restore `0.1` when the backend recovers —
`PP_TRACE_SAMPLE_RATIO` at 0.01 for a week is a quiet, permanent loss of diagnostic ability.

**M2 — scale the collector.**
```bash
kubectl -n pp-observability scale deployment/otel-gateway --replicas=<higher>
kubectl -n pp-observability set resources deployment/otel-gateway \
  --limits=memory=<higher> --requests=memory=<higher>
```
Expected: refusals stop, queue drains.

**M3 — fix the instrumentation.** A span per loop iteration is an instrumentation bug with the same
shape as a cardinality bug: a bounded thing became unbounded. Roll back the deploy that introduced
it, then fix.

**M4 — roll back the collector configuration.** The local stack's collector configuration is
`deploy/otel/collector.dev.yaml`; production's lives in the observability overlay. Re-apply from
Git rather than editing in place.

**M5 — accept, with a deadline.** If a backend outage is being handled elsewhere and the loss is
bounded, the correct action is to record the window and the sampling reduction. Say so explicitly:
"we are trace-blind for the next N minutes" is information the rest of the incident needs.

## Rollback / escalation

- **Do not disable tracing entirely.** `PP_OTEL_EXPORTER_OTLP_ENDPOINT` empty disables export, which
  is correct for a local run and wrong in production: it converts a partial loss into a total one,
  and nothing will alert you that it happened.
- **If a money-path incident is in progress**, this becomes urgent by association. Escalate to
  whoever owns the observability stack, because the incident's diagnosis speed depends on it.
- **Sustained loss for more than 24 hours** → escalate. At that point the platform's diagnostic
  claims in §1.5 are not true, and people are building habits around log search.
- **If the collector is back-pressuring the application** rather than dropping, that is a
  configuration error worth a P2: telemetry must never be able to slow the money path.

## Verification

```promql
rate(otelcol_exporter_send_failed_spans[5m]) == 0
rate(otelcol_receiver_refused_spans[5m]) == 0
otelcol_exporter_queue_size / otelcol_exporter_queue_capacity < 0.5
rate(otelcol_exporter_sent_spans[5m]) > 0
```
Then verify the thing that actually matters, end to end: take a recent payment, find its
`trace_id` from an exemplar or a log line, and confirm the trace opens with its full span tree —
`payment-api` → `payment-orchestrator` → the gateway span. An exporter reporting success while the
backend indexes nothing is a real failure mode, and the only test that catches it is looking at a
trace.

Confirm `PP_TRACE_SAMPLE_RATIO` is back to its charted value:
```bash
kubectl -n pp-data-plane get deployment payment-api -o jsonpath=\
'{range .spec.template.spec.containers[0].env[?(@.name=="PP_TRACE_SAMPLE_RATIO")]}{.value}{"\n"}{end}'
```

## Follow-up

- Record the window of trace loss. If an incident happened inside it, note in that postmortem that
  diagnosis was degraded — otherwise the timeline looks like slow responders rather than missing
  data.
- Size the collector against measured peak span volume, not against a default.
- If sampling was reduced, put a dated ticket on restoring it. This is the single most common way a
  platform ends up permanently under-sampled.
- If instrumentation volume was the cause, add the span-count expectation to the change's review:
  spans per request is a number that should be known, like allocations per request.
