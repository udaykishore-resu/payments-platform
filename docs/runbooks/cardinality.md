# RB-024: Metric cardinality budget exceeded

- **Severity:** ticket (P3, `page: "false"`)
- **Alert:** `MetricCardinalityBudgetExceeded`
  ```promql
  count by (__name__, service) ({__name__=~"pp_.+"}) > 10000
  ```
- **Triggered when:** any `pp_` metric has more than 10 000 active series on one service, sustained
  for 30 minutes.
- **Plane / service:** observability
- **Related:** `docs/observability.md` §3.2 (label discipline) and §3.3 (the cardinality rule and
  its CI lint), `docs/spec/00-design-baseline.md` §22.2, [otel.md](otel.md)

## What this means

The budget is 10^4 active series per metric per service. The CI lint catches violations at
declaration time; **this alert is the runtime backstop for a label value the code invents at
runtime** — a value derived from a request, an error string, an ID.

Why it matters enough to alert at all: unchecked cardinality is how an observability bill doubles
and, more importantly, how a query times out during an incident. The cost of this is paid exactly
when observability matters most.

The label discipline that prevents it (§3.2): `merchant_id`, `payment_id`, `tenant_id`, tokens,
URLs and error *messages* are never metric labels. They live in logs, traces and exemplars, where
high cardinality is free. Metric labels are bounded, enumerable sets — `gateway`, `operation`,
`outcome`, `currency`, `status`, `route` as a **template** rather than a path.

## Impact

- **No merchant impact. No money at risk.**
- Prometheus/AMP memory and cost rise; ingestion may be throttled.
- **Queries slow down or time out** — including the queries in every other runbook here.
- At the extreme, the affected metric becomes unusable and the alerts built on it stop being
  reliable, which is a silent loss of coverage.

## Immediate triage (first 5 minutes)

This is a P3. Same business day is the right pace.

1. Which metric, which service, how bad:
   ```promql
   topk(10, count by (__name__, service) ({__name__=~"pp_.+"}))
   count by (__name__) ({__name__=~"pp_.+"})
   ```
2. Which **label** is responsible:
   ```promql
   count(count by (gateway)   (pp_payments_total))
   count(count by (route)     (pp_http_requests_total))
   count(count by (outcome)   (pp_payments_total))
   count(count by (currency)  (pp_payments_total))
   ```
   Run one per label on the offending metric; the label whose distinct count is in the thousands is
   the culprit. A bounded label returns single or double digits.
3. Look at actual values — the shape usually names the bug:
   ```promql
   count by (route) (pp_http_requests_total{service="payment-api"})
   ```
   Paths with IDs in them (`/v1/payments/pay_01J…`) mean the route template is not being applied.
4. When did it start?
   ```promql
   count by (__name__, service) ({__name__=~"pp_.+"})[6h:5m]
   ```
   A step change points at a deploy; a ramp points at organic growth.
5. Correlate with deploys:
   ```bash
   kubectl -n pp-data-plane rollout history deployment/<service>
   ```

## Diagnosis

- **`route` has thousands of values** → the path is being recorded raw instead of as a template.
  This is the most common cause by a wide margin. → *M1*.
- **A label holds an error message or a normalized code that is not normalized** → free text became
  a label. → *M1*.
- **A label holds an ID (`merchant_id`, `payment_id`, `tenant_id`)** → a §3.2 violation that reached
  production. → *M1*, and ask why the lint did not catch it.
- **Growth is a smooth ramp with a bounded label set** → organic growth: more gateways, more
  currencies, more merchants in a legitimately-labelled dimension. → *M3*.
- **Step change at a deploy** → *M2*.
- **A `_bucket` metric is the offender** → too many histogram buckets multiplied by too many labels.
  Buckets multiply, so one extra label on a 12-bucket histogram costs 12× per value. → *M4*.
- **Growth started at a new gateway or region rollout** → expected and legitimate. → *M3*.

## Mitigation

**M1 — fix the label at its source and deploy.** The only real fix. Drop the offending label, or
map it to a bounded set (route templates, normalized error codes, an `other` bucket for the long
tail). Verify with the existing check:
```bash
./scripts/check-metrics-cardinality.sh
```
*Note: `docs/observability.md` §3.3 refers to this check as `scripts/metrics-lint.sh`; the script in
this repository is `scripts/check-metrics-cardinality.sh`. Same job, different name — the document
is what is out of date.*

**M2 — roll back the deploy** that introduced it:
```bash
kubectl -n pp-data-plane rollout undo deployment/<service>
```
Note that series already ingested do not disappear; they age out of the active set after the
staleness window (typically ~5 minutes without a sample, then out of the retention window
entirely). Expect the count to fall gradually, not instantly.

**M3 — raise the budget, deliberately.** If the growth is legitimate, the budget is wrong. Change
the alert threshold in `deployments/prometheus/prometheusrule-alerts.yaml` with the reasoning in the
commit message, and validate:
```bash
promtool check rules deployments/prometheus/prometheusrule-alerts.yaml
```
A threshold raised without a reason recorded is a threshold that will be raised again.

**M4 — reduce histogram buckets** on the offending metric, or drop a label from it specifically.
Buckets multiply with labels; that multiplication is usually where a "small" label addition became
10 000 series.

**M5 — drop the series at ingestion, as a stopgap.** A `metric_relabel_configs` rule that drops or
relabels the offending label buys time while the fix ships. It is a stopgap: it makes the data
wrong in a different way, and it hides the defect from the next person.

## Rollback / escalation

- **Do not delete the metric to make the alert go away.** Every metric on this platform has a
  question it answers, and an orphan check enforces that; deleting one removes an answer someone
  relies on.
- **Cost or ingestion throttling** → escalate to whoever owns the observability budget. This alert
  is the early warning for a bill, and the bill is a decision, not an incident.
- **If queries are timing out during another incident** → this becomes urgent by borrowed severity.
  Apply *M5* immediately, fix properly afterwards.
- **If an ID label reached production**, that is a §3.2 violation *and* a potential data-exposure
  question: metric series are retained and exported, and a `merchant_id` in a label is business
  data in a place with different access controls than logs.

## Verification

```promql
count by (__name__, service) ({__name__=~"pp_.+"}) < 10000
topk(5, count by (__name__, service) ({__name__=~"pp_.+"}))
```
The count falls as the old series go stale rather than instantly; confirm the *trend* is down and
that new series are not being created:
```promql
count by (route) (pp_http_requests_total{service="payment-api"})   # bounded again
```
And confirm the dashboards and alerts that use the metric still work — a label removed carelessly
breaks the queries that grouped by it:
```bash
promtool check rules deployments/prometheus/prometheusrule-alerts.yaml \
                     deployments/prometheus/prometheusrule-recording.yaml \
                     deployments/prometheus/prometheusrule-slo-burn.yaml
```

## Follow-up

- File the defect against the code that invented the label value.
- **Ask why CI did not catch it.** `scripts/check-metrics-cardinality.sh` is supposed to fail the
  build on this. Either the rule does not cover the case or the check is not running in the required
  set — both are more valuable to fix than the metric.
- If the budget was raised, record the new number and the reasoning where the next person will find
  it: in the rule file, next to the threshold.
- Reconcile the naming: `docs/observability.md` §3.3 names a script that does not exist under that
  name. Fixing the document is a five-minute change that saves someone an hour.
