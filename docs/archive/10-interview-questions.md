# 10 — Interview Questions (design-review / hiring calibration)

Grouped by role, meant to probe whether a candidate/reviewer understands the *why*, not just the
*what*, of this design.

## Architecture / Systems Design
1. Why is the outbox pattern necessary here instead of just publishing to SQS after the DB commit
   in the same request handler? Walk through the specific failure it prevents.
2. The ledger balance invariant is enforced by a DB constraint, not just application code. What
   class of bug does this defend against that a code review or unit test wouldn't reliably catch?
3. Why `SERIALIZABLE` isolation instead of `READ COMMITTED` with explicit row locking for the
   ledger writes? What's the cost, and how is that cost handled?
4. This service chose synchronous API response + asynchronous downstream fan-out. Describe a
   payments system design where the opposite (fully async, poll-for-result) would be the better
   choice, and why.
5. Why is the idempotency check and the ledger write performed in the *same* database transaction
   rather than as two separate steps?

## Reliability / SRE
6. Explain the difference between the SLA (99.9%) and the internal SLO (99.95%) here, and why
   they're set differently rather than identically.
7. Walk through the multi-window burn-rate alerting strategy. Why is "error rate > X% for 5
   minutes" alone considered insufficient?
8. During an AZ failure, what specifically prevents the remaining AZs from being overwhelmed
   (capacity math, not just "Kubernetes handles it")?
9. What would you monitor to detect a *slow* memory leak versus a crash-causing one, and why do
   they need different test strategies (soak test vs. load test)?

## Security
10. Why is mTLS used for in-cluster service-to-service calls when NetworkPolicy already restricts
    which pods can talk to which? What does mTLS add that network segmentation alone doesn't?
11. Why does a replayed idempotency key with a *different* request body get rejected with a 409
    instead of silently returning the original cached response?
12. Why are DB credentials fetched at runtime from Secrets Manager instead of injected as
    Kubernetes Secrets at deploy time?

## Data / Database
13. Why Aurora PostgreSQL over DynamoDB for the ledger, given DynamoDB's superior horizontal
    scalability? Under what future condition would you revisit this decision?
14. Describe the expand-contract migration pattern and why a naive blocking `ALTER TABLE` is
    dangerous on `ledger_entries` in production.
15. Why does the correction of a data-integrity incident always use a compensating ledger entry
    rather than an `UPDATE`/`DELETE`, even when the "correct" fix seems obvious?

## Platform / Kubernetes
16. What's the difference in purpose between the liveness probe and the readiness probe in this
    service, and why does liveness deliberately *not* check DB connectivity?
17. How does `PodDisruptionBudget` interact with a voluntary node drain during a cluster upgrade
    to prevent an availability regression that HPA alone wouldn't catch?
18. Why does the outbox relay use `FOR UPDATE SKIP LOCKED` instead of a leader-election pattern to
    coordinate across multiple pods?

## Behavioral / Judgment
19. The error budget for the month is 90% consumed with two weeks left. A product manager wants to
    ship a new feature that touches the payment path. How do you handle that conversation?
20. You're paged for `LedgerBalanceCheckFailure` — this alert has never fired before and is
    supposed to be structurally impossible. Walk through your first ten minutes.
