# RB-021: Onboarding stuck — failed workflows, manual gates, SLO breach

- **Severity:** ticket (`WorkflowInstancesFailed`, P2) · ticket (`WorkflowManualGateAging`, P3) ·
  ticket (`OnboardingSLOBreach`, P3). None of the three pages.
- **Alert:**
  ```promql
  # WorkflowInstancesFailed — 15m
  pp_workflow_instances{state="FAILED"} > 0
  # WorkflowManualGateAging — 6h
  pp_workflow_instances{state="AWAITING_SIGNAL"} > 20
  # OnboardingSLOBreach — 30m
  pp:onboarding_duration:p95_1h > 1800
  ```
- **Triggered when:** any instance has been `FAILED` for 15 minutes; or more than 20 instances have
  been waiting on a manual gate for 6 hours; or the automated portion's p95 has exceeded the
  30-minute SLO for 30 minutes.
- **Plane / service:** automation · `workflow-worker`
- **Related:** `docs/automation-plane.md`, `docs/onboarding.md`,
  `docs/adr/ADR-014-owned-workflow-engine-behind-port.md`,
  `docs/failure-handling.md` F-14, [dlq-triage.md](dlq-triage.md),
  [control-plane.md](control-plane.md)

## What this means

Three alerts about the same workflow engine, meaning three different things:

- **`WorkflowInstancesFailed`** — an instance has exhausted its retries **and its compensations**.
  This is terminal for the engine: it will not try again. Each one is a merchant who is not being
  onboarded and does not know why.
- **`WorkflowManualGateAging`** — instances parked in `AWAITING_SIGNAL`, waiting on a human. This
  is **not an engineering problem**. The automated portion has a 30-minute SLO; the manual
  compliance gate has a five-day timeout. The alert exists so that the recurring "onboarding is
  slow" conversation is about staffing when it is about staffing.
- **`OnboardingSLOBreach`** — the *automated* portion is slow. Time parked in external KYC and in
  manual gates is a separate series, deliberately, so the SLO measures what the platform controls.

The engine's mechanics matter for triage. Instances are leased (`PP_WORKFLOW_LEASE`, 60 s default)
with heartbeats (`PP_WORKFLOW_HEARTBEAT`, 15 s); a crashed worker's instances are picked up by
another worker when the lease expires and resumed **from the last checkpoint** — completed steps
are not replayed (F-14, baseline §11). Lease expiry uses the **database's** clock, so it is immune
to node clock skew (F-19).

## Impact

- **No payment impact whatsoever.** This is the automation plane; the money path does not touch it.
- **Merchants are not onboarded.** Each failed instance is a customer waiting, and the commercial
  cost of that is real even though nothing is broken technically.
- A merchant blocked by a **critical reconciliation exception** cannot be activated by design —
  if that is the cause, [reconciliation.md](reconciliation.md) is the runbook, not this one.
- `WorkflowManualGateAging` has no technical impact at all; its impact is on a queue of humans.

## Immediate triage (first 5 minutes)

1. The state distribution, and the failures with their errors — one command:
   ```bash
   ./bin/platformctl workflow list --state=FAILED --limit 50
   ```
   It prints instance counts by state, then the matching instances with their last error.
2. What is stuck rather than failed:
   ```bash
   ./bin/platformctl workflow list --stuck-for=30m --limit 50
   ./bin/platformctl workflow dlq --limit 50
   ```
3. The metrics view:
   ```promql
   pp_workflow_instances
   sum by (workflow, state) (pp_workflow_instances)
   pp:onboarding_duration:p95_1h
   rate(pp_workflow_compensations_total[15m])
   histogram_quantile(0.95, sum by (le, step) (rate(pp_workflow_step_duration_seconds_bucket[15m])))
   ```
   The last query names the slow step, which is the whole diagnosis for an SLO breach.
4. Are the workers healthy?
   ```bash
   kubectl -n pp-automation get pods -l app=workflow-worker
   kubectl -n pp-automation logs deploy/workflow-worker --since=15m | tail -60
   ```
5. From the system of record:
   ```sql
   SET LOCAL app.tenant_id = 'ten_…';
   SELECT state, count(*), min(updated_at) AS oldest_update
   FROM   pp.workflow_instances GROUP BY state ORDER BY 2 DESC;

   SELECT instance_id, workflow_name, current_step, attempt, lease_owner,
          lease_expires_at, run_after, left(last_error, 160) AS last_error
   FROM   pp.workflow_instances
   WHERE  state = 'FAILED' ORDER BY updated_at LIMIT 20;
   ```
6. Are the dependencies the workflow calls up?
   ```promql
   pp:control_plane_error:ratio_rate5m
   rate(pp_kyc_decisions_total[15m])
   ```

## Diagnosis

- **`FAILED` instances share one `last_error`** → one dependency or one bug. Fix it, then resume in
  bulk. → *M1* after the fix.
- **`last_error` names an external KYC/vendor call** → the vendor is down or rejecting. Check
  `pp_kyc_decisions_total`. → *M2*.
- **`last_error` names the control plane** → [control-plane.md](control-plane.md) first; resume
  afterwards.
- **`last_error` names a reconciliation exception blocking activation** → correct behaviour, not a
  bug. → [reconciliation.md](reconciliation.md).
- **Many instances with an expired `lease_expires_at` and no progress** → workers are not picking
  up leases: none running, or all crash-looping. → *M3*.
- **`HeartbeatInterval × 3 > LeaseDuration`** → the worker refuses to start with exactly this
  message. A heartbeat that races the expiry produces an instance claimed by two workers, which is
  the one thing the lease exists to prevent. → *M3*.
- **`AWAITING_SIGNAL` count high, nothing failing** → a staffing queue, not an incident. → *M4*.
- **p95 breach with one step dominating the histogram** → that step. → *M5*.
- **p95 breach with all steps normal** → the workers are not keeping up. → *M6*.
- **Entries in the workflow DLQ** → [dlq-triage.md](dlq-triage.md).

## Mitigation

**M1 — resume, after fixing the cause.**
```bash
./bin/platformctl workflow resume wfr_…
```
Resume restarts from the last checkpoint; completed steps are not replayed, so a resume does not
re-execute a side effect that already happened. Expected: the instance moves out of `FAILED` within
one poll interval. **Resuming into an unfixed cause just burns an attempt** — fix first, resume
second.

**M2 — vendor escalation, and pause intake if needed.** If an external KYC provider is down, the
instances will keep failing. Note that this is not a payment-path dependency, so there is no
pressure to improvise.

**M3 — fix and restart the workers.**
```bash
kubectl -n pp-automation logs deploy/workflow-worker --tail=40    # names the configuration problem
kubectl -n pp-automation rollout restart deployment/workflow-worker
kubectl -n pp-automation rollout status deployment/workflow-worker --timeout=5m
```
`workflow-worker` requires `PP_ENVIRONMENT`, `PP_REGION` and `PP_DATABASE_URL`, and refuses to start
if `PP_WORKFLOW_HEARTBEAT × 3 > PP_WORKFLOW_LEASE`. Expected: expired leases are re-acquired and
instances resume within `PP_WORKER_POLL_INTERVAL`. A restart is safe: the lease is the guard, and it
uses the database's clock.

**M4 — escalate the manual queue to compliance operations.** The mitigation is a person, and the
useful artefact is the number and the age distribution:
```sql
SET LOCAL app.tenant_id = 'ten_…';
SELECT count(*), min(updated_at) AS oldest, now() - min(updated_at) AS max_wait
FROM   pp.workflow_instances WHERE state = 'AWAITING_SIGNAL';
```
Send that, not "onboarding is slow". The five-day gate timeout is the deadline that matters.

**M5 — address the slow step.** Whatever the histogram named. If it is a dependency, that
dependency's runbook applies.

**M6 — scale the workers, or their concurrency.**
```bash
kubectl -n pp-automation scale deployment/workflow-worker --replicas=<higher>
kubectl -n pp-automation set env deployment/workflow-worker PP_WORKER_CONCURRENCY=8
```
One definition per deployment (`PP_WORKFLOW_NAME`), so a slow workflow cannot starve a fast one of
leases — check you are scaling the right deployment.

## Rollback / escalation

- **None of these three alerts pages, and that is correct.** Resist treating a P3 as an outage; the
  money path is unaffected and the appropriate pace is the business day.
- **`FAILED` count above ~20, or any instance failed for more than 24 hours** → escalate to the
  onboarding product owner. These are customers waiting.
- **`replay_count` at 5 on a DLQ entry** → stop resuming. The engine's constraint caps it at five
  deliberately: five is where someone has to look at *why* it keeps failing rather than pressing
  the button again ([dlq-triage.md](dlq-triage.md)).
- **Never activate a merchant to clear a stuck workflow.** The gates — KYC, sanctions, critical
  reconciliation exceptions — exist because activating without them is a licensing and AML problem
  that outlives any onboarding SLO.
- **Never edit `pp.workflow_instances` state directly.** The engine's checkpoint, attempt counter
  and lease are a coherent set; a manual state change produces an instance that is resumed from the
  wrong place or claimed twice.

## Verification

```promql
pp_workflow_instances{state="FAILED"} == 0
pp:onboarding_duration:p95_1h < 1800
pp_workflow_instances{state="AWAITING_SIGNAL"} < 20
```
```bash
./bin/platformctl workflow list --state=FAILED     # "no instances match"
./bin/platformctl workflow dlq                     # "empty — no unreplayed dead-lettered steps"
```
Then confirm the merchants actually got onboarded, rather than the instances merely leaving
`FAILED`:
```sql
SET LOCAL app.tenant_id = 'ten_…';
SELECT status, count(*) FROM pp.merchants
WHERE  created_at > now() - interval '24 hours' GROUP BY status;
```
And check that a compensation did not silently undo the work:
```promql
rate(pp_workflow_compensations_total[1h])
```

## Follow-up

- Record: instances failed, the distinct causes, time to resume, and merchants delayed.
- One recurring `last_error` is a missing retry classification or a missing compensation. Both are
  workflow-definition defects, and `docs/automation-plane.md` is where the definition lives.
- If the manual-gate queue is chronically over 20, that is a staffing finding. Present it with the
  age distribution and the five-day timeout, quarterly, so it is a plan rather than a recurring
  complaint.
- If the SLO breach was caused by a dependency, consider whether that step should be asynchronous
  with its own timeout rather than inline in the duration the SLO measures.
- Confirm the chaos coverage:
  `tests/chaos/crash_test.go::TestWorkerCrashMidWorkflowResumesWithoutRepeatingASideEffect` kills a worker
  mid-step and asserts the side effect occurred exactly once and compensation order is preserved.
