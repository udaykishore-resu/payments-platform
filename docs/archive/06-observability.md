# 06 — Observability

## Philosophy

If it isn't measured, it's a rumor. Every failure mode listed in `04-failure-recovery-design.md`
must have a corresponding metric, log, or trace that lets on-call detect it within the SLO's
detection budget — observability is designed *alongside* the feature, not bolted on after an
incident teaches us what we should have measured.

## The Three Pillars

### Metrics (Prometheus, scraped via `/metrics`, visualized in Grafana)

**RED metrics (per endpoint, per client_id label)**
- `http_requests_total{route,method,status}` — Rate
- `http_request_duration_seconds{route,method}` (histogram) — Errors derived from `status`,
  Duration for P50/P95/P99

**Domain / business metrics** (the ones that matter most for a payments system):
- `payments_created_total{status}`
- `payments_amount_minor_sum{currency}` — business volume, not just technical throughput
- `ledger_balance_check_failures_total` — should be permanently zero; any nonzero value is a
  page-immediately alert (would indicate the DB constraint was somehow bypassed)
- `idempotency_conflicts_total` — replayed key with different body; spikes indicate a client bug
- `outbox_unpublished_count` (gauge) — outbox backlog; alert if sustained above threshold
- `outbox_publish_latency_seconds` (histogram) — commit-to-publish lag

**Resource / runtime metrics**
- Go runtime: `go_goroutines`, `go_memstats_heap_inuse_bytes`, GC pause histogram
- DB connection pool: in-use/idle/wait-count, matched against pool size to catch saturation before
  it causes request failures
- Circuit breaker state per dependency (`circuit_breaker_state{dependency}`: closed/open/half-open)

### Logs (structured JSON, `zap`-style, shipped to CloudWatch Logs → optionally ELK/OpenSearch)

- Every log line includes `trace_id`, `span_id`, `client_id` (when authenticated), `payment_id`
  (when applicable) — so a single request's logs are trivially correlatable and joinable with
  traces.
- Log levels used deliberately: `ERROR` reserved for things that page; `WARN` for degraded-but-
  handled (e.g. a retried serialization failure); `INFO` for state transitions
  (payment created/completed); `DEBUG` off in production by default, toggleable per-pod via a
  runtime flag for live debugging without a redeploy.
- **Audit log is separate from operational logs**: written to the `audit_log` DB table (immutable,
  access-controlled, 7-year retention per compliance requirement) — not just to CloudWatch, which
  has its own retention/cost lifecycle unsuited to regulatory audit needs.
- PII/amount scrubbing: operational logs truncate/hash account identifiers; full detail lives only
  in the access-controlled audit trail and the DB itself.

### Traces (OpenTelemetry SDK → OTel Collector → AWS X-Ray or Tempo/Jaeger)

- Every incoming request gets a trace spanning: middleware chain → DB transaction → outbox write →
  (async, linked but not blocking) outbox relay publish span.
- Trace context propagated via W3C `traceparent` header to any downstream call, and injected into
  the outbox event payload so the *eventual* SQS-consumer processing can be linked back to the
  originating request trace — critical for debugging "why did this payment's notification arrive
  late" across the async boundary.
- Sampling: 100% for error traces, adaptive/probabilistic (e.g. 5-10%) for successful requests at
  volume, tunable via the Collector without redeploying the service.

## Dashboards

1. **Service health** (RED metrics, per route, with P50/P95/P99 latency and error-rate SLO burn
   rate overlay).
2. **Ledger integrity** (balance-check failures, idempotency conflict rate, reconciliation job
   status) — the dashboard an auditor or risk officer would want.
3. **Async pipeline health** (outbox backlog depth/age, publish latency, DLQ depth per consumer
   queue).
4. **Infrastructure** (pod count vs. HPA target, node capacity headroom, DB connection pool
   saturation, Aurora replica lag).
5. **Business** (payment volume, success rate, volume by currency) — for product/finance
   stakeholders, not just engineering.

## Alerting (examples; full list lives in the alerting-as-code repo, not duplicated here)

| Alert | Condition | Severity | Response |
|---|---|---|---|
| `LedgerBalanceCheckFailure` | `ledger_balance_check_failures_total` increases | Page (SEV-1) | Immediate — see runbook, this should be structurally impossible |
| `ErrorBudgetBurnFast` | Error-rate SLO burn rate implies exhausting monthly budget in < 2 hours | Page (SEV-1/2) | See `07-reliability-slo.md` multi-window burn-rate policy |
| `OutboxBacklogGrowing` | `outbox_unpublished_count` > N for > 5 min | Page (SEV-2) | Check relay health, SQS status |
| `DLQDepthNonZero` | Any consumer DLQ depth > 0 | Ticket (SEV-3), page if growing | Inspect poison messages, redrive after fix |
| `DBConnectionPoolSaturated` | Pool wait-count sustained > 0 | Page (SEV-2) | Check for slow queries, connection leak, or need to scale pool/replica |
| `CircuitBreakerOpen` | Any breaker open > 2 min | Ticket (SEV-3), page if it's the DB breaker | Check dependency health |
| `CertExpiryImminent` | Cert TTL < 14 days | Ticket (SEV-3) | Verify cert-manager renewal in progress |

## Synthetic Monitoring

- Canary request (`POST /v1/payments` against a dedicated always-funded synthetic test account,
  amount reversed immediately after) run every 60s from outside the cluster (and from the
  secondary region) — validates the full stack including DNS, WAF, ALB, and DB write path, not
  just an in-cluster health check. Feeds the cross-region failover health check used for DR.

## Health Checks

- **Liveness**: process is not deadlocked (lightweight, no DB call — a DB hiccup shouldn't cause
  Kubernetes to kill and reschedule an otherwise-healthy pod).
- **Readiness**: DB connection pool can acquire a connection and run `SELECT 1` within budget, AND
  the circuit breaker to the DB is not open — a pod that can't reach its dependencies stops
  receiving new traffic without being killed, giving the dependency time to recover.
- **Startup probe**: allows slower first-time startup (connection pool warm-up, config validation)
  without the liveness probe prematurely killing a pod that's still initializing.
