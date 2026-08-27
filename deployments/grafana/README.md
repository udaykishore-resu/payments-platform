# Grafana dashboards

Four dashboards, matching `docs/observability.md` §6. Anything not on one of them
is a query in a runbook, not a panel — dashboards nobody reads are
indistinguishable from dashboards that lie.

| File | UID | Audience | Refresh / range |
|---|---|---|---|
| `dashboard-executive.json` | `pp-executive` | Leadership, product, on-call during a customer-visible incident | 1 m / 24 h |
| `dashboard-service-health.json` | `pp-service-health` | On-call. RED per deployable, repeats per `$service` | 30 s / 6 h |
| `dashboard-gateway-health.json` | `pp-gateway-health` | On-call, payments ops. Repeats per `$gateway` | 30 s / 6 h |
| `dashboard-onboarding-funnel.json` | `pp-onboarding-funnel` | Onboarding ops, customer success, product | 5 m / 30 d |

Template variables on all four: `$env`, `$region`, `$service`, `$gateway`,
`$tenant_tier`, `$interval` (plus `$datasource`, so the same JSON works against
AMP in prod and a local Prometheus in dev).

Every dashboard carries two annotation queries — Argo CD deploy markers and Argo
Rollouts canary steps — overlaid on every panel. Half of all "when did this
start" questions are answered by the purple line, and asking them without deploy
markers is guessing.

## Two panels that are easy to misread

**Service health, panel 7 — CPU throttling.** It must be flat zero for
`payment-api`, `payment-orchestrator` and `webhook-ingress`. Those three have no
CPU limit on purpose. A non-zero line means someone added one, and the latency
SLO is now partly being spent on the CFS scheduler stopping every thread in the
cgroup for the remainder of a 100 ms period. Throttling is invisible in average
CPU utilisation — a pod throttled 20 % of periods can report 40 % CPU — which is
why it gets a panel of its own rather than a footnote.

**Executive, panel 15 — unresolved `TIMEOUT_UNKNOWN`.** The count of payments
where we do not currently know whether money moved. Its correct value is a small
number trending to zero within minutes. It is the honest headline number for this
platform, and it is deliberately on the executive dashboard rather than buried in
an operations view.
