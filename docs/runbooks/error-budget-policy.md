# RB-003: Error-budget policy — deploy freeze

- **Severity:** ticket (P3, `page: "false"`)
- **Alert:** `ErrorBudgetPolicyFreeze`
  ```promql
  pp:error_budget_remaining:payment_api < 0.25
  ```
- **Triggered when:** less than 25 % of the 30-day availability budget remains, sustained for
  30 minutes. Below 10 % the same alert's summary escalates the tier from SOFT to HARD.
- **Plane / service:** data · `payment-api` (label `policy: freeze`)
- **Related:** `docs/observability.md` §4.4, `docs/deployment.md` §5 (release gates),
  [payment-api-availability.md](payment-api-availability.md)

## What this means

This is not an outage alert. It is the accounting of one: reliability work has been deferred long
enough that the 30-day budget is nearly spent, and the policy converts that fact into a decision
instead of a discussion.

- **Soft freeze, below 25 %**: only bug fixes, reliability work and security patches merge.
- **Hard freeze, below 10 %**: the reliability backlog becomes the sprint.

The budget does not refill on a fix; it refills as the 30-day window slides past the incidents
that spent it. That asymmetry is deliberate — it makes the cost of an incident persist long enough
to be planned around.

## Impact

No merchant impact from this alert itself. The impact already happened: the budget was spent by
real 5xx that real merchants received. What changes now is engineering throughput — feature
merges stop.

If this fires *during* an incident, the incident is the priority and the freeze is a consequence.
If it fires with nothing burning, this is a planning problem, not an on-call problem.

## Immediate triage (first 5 minutes)

This is a ticket. There is no five-minute clock. Do this within the business day:

1. Confirm the tier:
   ```promql
   pp:error_budget_remaining:payment_api
   ```
   `>= 0.10` is SOFT, `< 0.10` is HARD.
2. Confirm nothing is burning *right now* — if it is, this runbook is the wrong one:
   ```promql
   pp:payment_api_error:ratio_rate5m
   pp:payment_api_error:ratio_rate1h
   ```
3. Attribute the spend across the window:
   ```promql
   sum_over_time(pp:payment_api_errors:rate5m[30d])
   topk(5, sum by (route) (increase(pp_http_requests_total{service="payment-api",status="5xx"}[30d])))
   ```
4. Line the spend up against the incident record. Every material burn should map to a postmortem;
   a burn with no postmortem is the finding.

## Diagnosis

- **A single incident spent most of the budget** → the freeze is doing its job. → *M1*, and the
  exit criterion is that incident's postmortem actions being complete.
- **Spend is diffuse — many small burns, no single incident** → this is chronic, and it is the
  case the policy exists for. → *M1* plus *M3*.
- **The budget is being spent by one route or one client** → possibly not a reliability problem at
  all. → *M4*.
- **`pp:error_budget_remaining:payment_api` is negative or the recording rule is stale** → the SLI
  itself is broken; freezing on a wrong number is worse than not freezing. → *M5*.
- **Something is burning right now** → [payment-api-availability.md](payment-api-availability.md)
  first. Come back afterwards.

## Mitigation

**M1 — declare the freeze.** Announce the tier in the engineering channel with the current
remaining percentage and the exit criterion. The gate is enforced as a required check on the pull
request. *Note: `docs/observability.md` §4.4 names `scripts/slo-gate.sh` as the enforcing script;
that script is not present in this repository, so today the freeze is enforced by the reviewing
team rather than mechanically. Treat writing it as a follow-up item, not as a reason to skip the
freeze.* Expected effect: feature pull requests stop merging; fixes continue.

**M2 — record exemptions, do not grant them quietly.** An exemption needs SRE-lead approval
recorded on the pull request, with the reason. An unexplained exemption is indistinguishable from
the policy not existing.

**M3 — convert the diffuse spend into work.** Take the top three routes by 30-day 5xx count and
file one issue each with the budget arithmetic attached. This is the entire mechanism by which
this alert improves anything.

**M4 — reclassify.** If the 5xx are genuinely a client's doing (an SDK retrying into a
rate limit, a tenant hammering a deprecated route), the fix is in the client relationship and the
SLI may need a scoping fix — but only via a reviewed change to the recording rule, never by
excluding an inconvenient route mid-incident.

**M5 — fix the SLI.** Verify the recording rules load and evaluate:
```bash
promtool check rules deployments/prometheus/prometheusrule-recording.yaml \
                     deployments/prometheus/prometheusrule-slo-burn.yaml
```
A broken SLI is a P2 in its own right: it means the availability alerts are also lying.

## Rollback / escalation

- **HARD tier (`< 0.10`)** → notify the engineering manager and the product owner the same day.
  The sprint changes; that decision is not the on-call's to make alone.
- **Budget reaches zero** → the SLO for the month is missed. That is a commitment conversation,
  and it happens with the account and product owners, not in the incident channel.
- **Someone asks to lift the freeze for a launch** → escalate to the SRE lead. It may be the right
  call; it is not a call made under deadline pressure by whoever is on-call.

## Verification

```promql
pp:error_budget_remaining:payment_api > 0.25
```
The alert clears by itself as the 30-day window slides past the spend, typically weeks, not hours.
Do not silence it to make the dashboard green — the alert *is* the freeze.

## Follow-up

- Every burn in the window has a postmortem, or the missing postmortem is the first action item.
- Review whether 99.99 % is still the right target. A budget that is exhausted every month is
  either a target set above what the architecture supports or a system that needs investment; both
  are decisions, and pretending is neither.
- Check that the policy actually held: count pull requests merged during the freeze and how many
  carried a recorded exemption. A freeze nobody honoured is worse than none, because it is
  reported as a control.
- Write `scripts/slo-gate.sh` so the next freeze is enforced by CI rather than by memory.
