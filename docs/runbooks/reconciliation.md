# RB-007: Reconciliation exceptions — the unresolved-money queue

> **No operator resolves a reconciliation exception by editing a payment.** Resolution means
> finding out what the gateway actually did and letting the platform record it, or recording a
> reviewed, audited human decision through the control plane. `UPDATE pp.payments` is not a
> resolution; it is the destruction of the evidence that a resolution would have been based on.

- **Severity:** page (`ReconciliationExceptionsCritical`, P1) · ticket
  (`ReconciliationExceptionsRising`, P2)
- **Alert:** `ReconciliationExceptionsCritical`
  ```promql
  pp_reconciliation_exceptions{severity="critical"} > 0
  ```
  `ReconciliationExceptionsRising`
  ```promql
  pp_reconciliation_exceptions{severity=~"high|medium"} > 25
  ```
- **Triggered when:** any critical exception has been open for 5 minutes; or more than 25
  high/medium exceptions have been open for 30 minutes.
- **Plane / service:** data · `payment-orchestrator`
- **Related:** `docs/adr/ADR-013-timeout-leaves-payment-processing.md`,
  `docs/spec/00-design-baseline.md` §14.4, `docs/failure-handling.md` F-1,
  [timeout-unknown.md](timeout-unknown.md), `docs/compliance.md` §6

## What this means

A reconciliation exception is an unresolved discrepancy between what we believe happened and what
the gateway's records say happened. The reconciler opens one when a `TIMEOUT_UNKNOWN` attempt
cannot be resolved automatically within 15 minutes, and when a settlement report disagrees with
our state.

The two alerts mean different things:

- **Critical** is *severity*: we do not know whether money moved for a specific payment. One is
  enough to page. A critical exception also **blocks that merchant's transition to `ACTIVE`** —
  there is a partial index on exactly this predicate for that guard — so it is not only a money
  question, it holds up onboarding.
- **Rising** is *volume*: usually one gateway's lookup API is failing, so timeout-unknown attempts
  cannot be resolved automatically and accumulate. The individual exceptions may each be benign;
  the pile is the signal.

Exception identity is `(tenant_id, kind, payment_id, external_ref)` and re-detection **updates**
the row rather than opening a second one, so the count is a count of distinct discrepancies, not
of reconciliation runs.

States are `OPEN → INVESTIGATING → RESOLVED | ACCEPTED`. `ACCEPTED` is a deliberate human decision
that the discrepancy is understood and tolerated; it is not a way to make the number go down.

## Impact

- **Money at risk:** for each critical exception, a specific payment where the cardholder may or
  may not have been charged. Unresolved, these become chargebacks, expired authorizations, or
  settlement disputes.
- **Merchants** whose payments are affected see `PROCESSING` that never resolves. Merchants with a
  critical exception open cannot be activated.
- **Degraded, not down.** New payments are unaffected. What is broken is the platform's ability to
  close the loop on old ones.
- **Compliance:** unresolved exceptions are visible to the auditor. `docs/compliance.md` treats the
  exception queue and its ageing as evidence.

## Immediate triage (first 5 minutes)

1. Severity mix and trend:
   ```promql
   pp_reconciliation_exceptions
   pp_reconciliation_exceptions{severity="critical"}
   deriv(pp_reconciliation_exceptions[30m])
   ```
2. The actual queue, from the system of record. **Read-only.**
   ```sql
   -- read replica; tenant context set, RLS applies
   SET LOCAL app.tenant_id = 'ten_…';
   SELECT severity, kind, state, count(*),
          min(opened_at) AS oldest,
          now() - min(opened_at) AS age
   FROM   pp.reconciliation_exceptions
   WHERE  state IN ('OPEN','INVESTIGATING')
   GROUP  BY severity, kind, state
   ORDER  BY severity, count DESC;
   ```
3. The critical ones individually — there should be few enough to read:
   ```sql
   SET LOCAL app.tenant_id = 'ten_…';
   SELECT exception_id, merchant_id, payment_id, attempt_id, external_ref,
          kind, detail, opened_at, expected, actual
   FROM   pp.reconciliation_exceptions
   WHERE  severity = 'CRITICAL' AND state IN ('OPEN','INVESTIGATING')
   ORDER  BY opened_at;
   ```
4. Is the automatic resolution path working? This decides whether the pile drains by itself:
   ```promql
   pp:webhook_processing_lag:p99_5m
   sum by (gateway, operation, class) (rate(pp_gateway_errors_total{operation="lookup"}[5m]))
   pp_attempts_unresolved
   ```
5. Safe commands — read-only, and the complete permitted set for this incident:
   ```bash
   ./bin/platformctl version
   ./bin/platformctl migrate status
   ./bin/platformctl outbox status          # is payment.reconciliation_required.v1 flowing
   ./bin/platformctl verify-audit-chain ten_…
   ```
   There is no `platformctl reconciliation` command and no command that resolves an exception. The
   resolution path is: find out what the gateway did → let the platform record it.

## Diagnosis

- **Volume rising, one gateway dominant, `operation="lookup"` errors on that gateway** → the
  gateway's status API is down. Nothing is wrong with our data; the resolver has no oracle.
  → *M1* (vendor), *M2* (wait, with a deadline).
- **Volume rising with webhook lag high** → the fast path is blocked.
  → [webhook-lag.md](webhook-lag.md), then re-check; most of the pile usually clears when webhooks
  catch up.
- **Volume rising with `pp_outbox_backlog` growing** → `payment.reconciliation_required.v1` is not
  reaching the reconciler at all. → [outbox.md](outbox.md).
- **Critical exception, `kind` indicates an amount or currency mismatch against settlement** →
  a genuine discrepancy in what moved. → *M3*, finance involvement, do not resolve technically.
- **Critical exception, `expected`/`actual` show the gateway has no record of the attempt** → the
  request never landed; the payment did not charge. → *M4*.
- **Critical exception, gateway shows an authorization we have no terminal state for** → money
  *did* move. → *M4*, and this is the one that must not be left.
- **Exceptions concentrated on one merchant** → their configuration or their connection
  credentials. Check `pp.gateway_connections` status for that merchant.
- **Count is high but ages are all under 15 minutes** → this is the normal working of the
  resolver during a gateway wobble. → *M2*.

## Mitigation

**M1 — vendor escalation, with the keys.** The deterministic gateway idempotency key is what makes
this a five-minute conversation instead of a forensic exercise:
```sql
SET LOCAL app.tenant_id = 'ten_…';
SELECT a.gateway_id, a.gateway_idempotency_key, a.gateway_reference,
       a.request_sent_at, a.amount, a.currency
FROM   pp.payment_attempts a
WHERE  a.attempt_id IN (SELECT attempt_id FROM pp.reconciliation_exceptions
                        WHERE severity = 'CRITICAL' AND state = 'OPEN');
```
Send the keys and the window. Ask exactly one question: *for each of these keys, what did you do?*
Expected: their answer lets the reconciler (or a reviewed control-plane action) close each one.

**M2 — restore the automatic path and let it drain.** Fix whichever of webhook ingest, the outbox
or the consumer lag is broken, then watch the count fall. This is the correct action for the
`Rising` alert in almost every case. Give it a deadline: if the count has not started falling
within 30 minutes of the underlying fix, escalate.

**M3 — engage finance/operations for settlement discrepancies.** An amount mismatch against a
settlement report is not an engineering resolution. The exception moves to `INVESTIGATING`, the
finance contact owns it, and the outcome is either a corrected record or an `ACCEPTED` decision
with a written reason.

**M4 — resolve through the platform, never around it.**
- If the gateway confirms it **never saw** the attempt: the payment did not charge. The
  authoritative record is updated by the platform's own reconciliation path once the gateway's
  answer is available to it — not by an operator's `UPDATE`.
- If the gateway confirms it **authorized**: the payment must reach the corresponding terminal
  state through the same path. The webhook replay from the gateway is the normal mechanism; ask
  the vendor to re-deliver it.
- In both cases the exception is then closed with `resolution`, `resolved_by` and `resolved_at`
  set by the tooling that performed the resolution, so the audit record exists.

**M5 — accept, deliberately and rarely.** `ACCEPTED` is for a discrepancy that is understood and
tolerated (a rounding difference within a documented tolerance, a duplicate external reference
from the gateway). It requires a named `resolved_by` and a written `resolution`. It is dual-
controlled, and an `ACCEPTED` exception is reviewed in the weekly reconciliation review.

### Forbidden

| Action | Why |
|---|---|
| `UPDATE pp.reconciliation_exceptions SET state='RESOLVED'` to clear the alert | The alert is the only thing keeping the money question visible. Closing it does not answer it. |
| `DELETE FROM pp.reconciliation_exceptions` | The unique constraint means re-detection would reopen it — after you have destroyed the investigation history. |
| Refunding a payment "to make it neutral" | A refund against a payment that never captured creates an unbalanced ledger and a real credit to a real cardholder for money that never arrived. |
| Retrying the payment | See [timeout-unknown.md](timeout-unknown.md). This is the double-charge path. |
| Activating a merchant blocked by a critical exception | The guard exists precisely so an unresolved money question cannot be onboarded past. |

## Rollback / escalation

- **Any critical exception open for more than 1 hour** → Sev-1, payments product owner and the
  finance contact. At that age the question is commercial, not technical.
- **More than 100 open exceptions, or the count rising for 2 hours** → declare an incident even
  though the alert is only P2. Volume at that scale means the automatic path is not coming back on
  its own.
- **A discrepancy that implies money moved that we have no record of** → this is a Sev-1 with
  finance and compliance in the channel from the start. Do not attempt a technical fix first.
- **Freeze the affected merchant's activation** if it is not already blocked — the guard should do
  it, and if it did not, that is a second finding.
- **If the audit chain is implicated** (someone edited history to make an exception go away) →
  [audit-tamper.md](audit-tamper.md), immediately, and stop touching the data.

## Verification

```promql
pp_reconciliation_exceptions{severity="critical"} == 0
pp_reconciliation_exceptions{severity=~"high|medium"} < 25
pp_attempts_unresolved                                    # trending to zero
```
And from the system of record — every closed exception must carry its evidence:
```sql
SET LOCAL app.tenant_id = 'ten_…';
SELECT state, count(*) FROM pp.reconciliation_exceptions
WHERE  opened_at > now() - interval '24 hours' GROUP BY state;

-- No exception may be RESOLVED/ACCEPTED without a resolver and a reason.
SELECT exception_id FROM pp.reconciliation_exceptions
WHERE  state IN ('RESOLVED','ACCEPTED')
  AND  (resolved_by = '' OR resolution = '' OR resolved_at IS NULL);
```
The second query must return zero rows. The schema's `recon_resolved_has_time` constraint enforces
the timestamp; the blank resolver and reason are what a rushed close looks like.

Confirm merchant activations that were blocked can now proceed, and that the audit chain is intact:
```bash
./bin/platformctl verify-audit-chain ten_…
```

## Follow-up

- Record, per exception: how it was resolved, how long it took, and which resolution path answered
  it. Anything answered by settlement rather than by webhook or lookup is a gap in the fast paths.
- If the cause was a gateway lookup outage, the durable fix is a second oracle — settlement-report
  ingestion cadence, or the gateway's bulk reconciliation endpoint — not a longer retry.
- Feed the weekly reconciliation review: recurring `kind` values are a missing validation or a
  missing mapping in the anti-corruption layer.
- Check that no exception aged past the SLA silently. If the alert only fired at 25 and the pile
  sat at 24 for a week, the threshold is wrong, and that is worth changing.
