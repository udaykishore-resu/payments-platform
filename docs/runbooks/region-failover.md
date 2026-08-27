# RB-019: Region failover

> **Promotion is a human decision. There is no automatic cross-region promotion, and this runbook
> does not contain one.** It is the on-call's entry point: how to recognise a region event, what to
> check, and when to hand over to the incident commander. **The full, authoritative procedure is
> `docs/disaster-recovery.md` §4.1.3 (promotion), §6 (the minute-by-minute walkthrough) and §8
> (failback).** Follow that document, not a summary.

- **Severity:** page
- **Alert:** no single alert declares a region failure. It arrives as Route 53 health-check
  failures plus synthetic probes from three external regions, and as a simultaneous storm of
  `plane=data` alerts — which Alertmanager inhibits behind the region-failover alert so one root
  cause produces one page.
- **Triggered when:** 3 consecutive health-check failures over 90 s (`docs/failure-handling.md`
  F-17). Route 53 then moves **traffic**. It does **not** move write authority.
- **Plane / service:** data (and everything else) · region-wide
- **Related:** `docs/disaster-recovery.md` (the whole document),
  `docs/adr/ADR-021-active-passive-money-active-active-control.md`,
  `docs/failure-handling.md` F-16, F-17, [dr-replication-lag.md](dr-replication-lag.md),
  [aurora-failover.md](aurora-failover.md)

## What this means

Active/passive for money, active/active for the control plane. Route 53 health checks move traffic
to the passive region; **only a human running the promotion procedure moves write authority.**

The reason is stated plainly in §3: a network partition between regions looks identical to a region
failure from the other side. Automating promotion on that signal is how both regions end up
writable, which on a payment platform is how a cardholder is charged twice. Double-charging costs a
chargeback, a fine and trust; fifteen minutes of downtime costs fifteen minutes. The trade is
deliberate.

Three independent barriers stop the old region resuming writes, any one of which is sufficient:

1. **Aurora Global detach** — the old cluster's writer is no longer part of the global cluster.
2. **The epoch fence** — a monotonically increasing `epoch` in the DynamoDB Global Table
   `pp-dr-control` (item `region_authority`), guarded by a conditional write. Promotion increments
   it; every `payment-api` and `payment-orchestrator` pod reads it on startup and every 10 s, and a
   pod whose cached epoch is lower **stops accepting writes and fails readiness within 10 s**.
3. **The procedure's first step** — Region A's data-plane deployments are scaled to zero and its
   Route 53 target removed.

DNS is **not** a safety mechanism. It is a traffic control with a 60 s TTL and client caching that
can outlive it. Correctness depends on the epoch fence and the Aurora topology, never on DNS
having converged.

Commitments: **RTO ≤ 15 minutes, RPO ≤ 5 seconds.** Kafka is deliberately **not** replicated across
regions; the promoted region rebuilds its event stream from its own outbox, which is the
authoritative record.

## Impact

- Up to 15 minutes of unavailability for the money path.
- Up to 5 seconds of committed writes lost — payments accepted and acknowledged in Region A within
  the replication window that never reached Region B. See `docs/disaster-recovery.md` §7 for the
  three windows and why failover specifically adds nothing new to the duplicate-payment risk.
- The control plane is active/active and degrades less.
- In-flight requests during promotion **fail closed**: a transaction against the old writer either
  committed before the writer died or was rolled back by the database. There is no third outcome.

## Immediate triage (first 5 minutes)

Your job in these five minutes is **not** to promote. It is to establish whether the region is
genuinely gone and to hand the incident commander a decision with evidence.

1. Is it the region, or is it us?
   ```bash
   aws health describe-events --filter regions=eu-west-1,eventStatusCodes=open \
     --query 'events[].{svc:service,code:eventTypeCode,start:startTime}'
   kubectl --context eu-west-1 get nodes
   kubectl --context eu-west-1 -n pp-data-plane get pods -l app=payment-api
   ```
2. Is the primary writer reachable at all?
   ```bash
   aws rds describe-db-clusters --region eu-west-1 --db-cluster-identifier pp-prod \
     --query 'DBClusters[0].{Status:Status,Endpoint:Endpoint}'
   psql "$REGION_A_WRITER" -c "SELECT pg_is_in_recovery();"
   ```
3. **Record the pre-promotion lag now.** This is the measured RPO for the event and it is gone the
   moment anyone promotes:
   ```bash
   aws cloudwatch get-metric-statistics --region eu-central-1 \
     --namespace AWS/RDS --metric-name AuroraGlobalDBReplicationLag \
     --dimensions Name=DBClusterIdentifier,Value=pp-prod-secondary \
     --start-time "$(date -u -d '-5 min' +%FT%TZ)" --end-time "$(date -u +%FT%TZ)" \
     --period 60 --statistics Maximum Average | tee evidence/rpo-observed.json
   ```
4. Is this a partition rather than a failure? Check from a third place:
   ```bash
   aws dynamodb get-item --table-name pp-dr-control \
     --key '{"pk":{"S":"region_authority"}}' --consistent-read
   ```
   If Region A is still serving traffic from its own side, you are looking at a partition, and
   promotion is the dangerous option, not the safe one.
5. Is the secondary actually ready to take over?
   ```bash
   aws rds describe-db-clusters --region eu-central-1 \
     --db-cluster-identifier pp-prod-secondary --query 'DBClusters[0].Status'
   kubectl --context eu-central-1 -n pp-data-plane get deploy
   ```
6. **Page the incident commander.** Promotion needs a named decision-maker. This is the one place
   in the platform where a human in the loop is a design decision rather than a gap.

## Diagnosis

- **AWS Health confirms a regional service event, Region A unreachable from every angle** →
  a genuine region loss. → *M1*: the IC authorises promotion, executed per
  `docs/disaster-recovery.md` §4.1.3.
- **Region A is reachable but degraded** → a *planned* failover is available and is lossless
  (`failover-global-cluster`, 60–120 s). Always preferred when Region A responds. → *M2*.
- **Region A is serving traffic but unreachable from the health checkers** → **partition, not
  failure.** Promotion here creates two writers. → *M3*.
- **One AZ, not the region** → `docs/failure-handling.md` F-16; multi-AZ and 3× headroom absorb it.
  No promotion. → *M4*.
- **Aurora failed over in-region and everything else is fine** → [aurora-failover.md](aurora-failover.md).
  Not this runbook.
- **Replication lag was already above budget** → the loss on promotion will exceed the RPO
  commitment. The IC must know the number before deciding.
  → [dr-replication-lag.md](dr-replication-lag.md).

## Mitigation

**M1 — unplanned promotion (Region A gone).** Executed by the IC or under their explicit
authorisation, following **`docs/disaster-recovery.md` §4.1.3** step by step. The order is not
negotiable:

1. Record the pre-promotion lag (step 3 above) — this is the evidence.
2. **Fence the old region first.** The conditional write on `pp-dr-control` is the
   anti-double-promotion guard: it fails if someone already promoted. Nothing below is safe until
   it returns.
3. Promote — `aws rds remove-from-global-cluster` when Region A is gone (45–75 s, lossy up to the
   observed lag).
4. Wait for a writable endpoint: `aws rds wait db-cluster-available`, then confirm
   `SELECT pg_is_in_recovery();` returns `f`.
5. Scale Region B to full and repoint Route 53.

Budget: Aurora ≤ 2 min of a 15 min RTO. **Do not resize the secondary during failover** — it runs
at half class deliberately, resizing costs 5–10 minutes, and the degradation ladder absorbs the
latency. This is documented explicitly so nobody "helpfully" resizes mid-incident.

**M2 — planned failover (Region A reachable).** `aws rds failover-global-cluster` — coordinated,
zero data loss, 60–120 s. Slower and always preferred when Region A responds. **Never**
`remove-from-global-cluster` in this case.

**M3 — partition: do not promote.** Escalate to the IC with the evidence that Region A is serving.
The correct action is to fix the partition or to fence Region A deliberately and verify it has
stopped accepting writes before promoting. Promoting into a partition is the failure mode the whole
design exists to prevent.

**M4 — AZ loss: nothing.** Confirm headroom holds; consider shedding P4 traffic if it does not.
```bash
kubectl get nodes -L topology.kubernetes.io/zone
```

**Rehearsal, not improvisation.** Both of these print the plan without executing anything:
```bash
./bin/platformctl dr-drill --scenario=region-failover           # dry-run is the default
./bin/platformctl dr-drill --scenario=writer-failover
./scripts/dr-drill.sh --dry-run                                 # the RD-1 restore drill
```

## Rollback / escalation

- **There is no rollback from a promotion.** Failback is a separate, planned operation with its own
  procedure (`docs/disaster-recovery.md` §8): rebuild Region A from Git, re-establish replication
  with B as primary, warm A to full **before** promoting, quiesce writes briefly (~30–45 s), then a
  **planned** lossless failover. Never `remove-from-global-cluster` on failback.
- **Escalate to the IC before promoting, always.** If you cannot reach one, escalate up the
  rotation. Fifteen minutes of downtime is the budgeted cost; a wrong promotion is unbounded.
- **If two regions are ever writable simultaneously**: stop all writes immediately, this is a Sev-1
  data-integrity incident, and the reconciliation in §8.2 becomes the priority over availability.
- **Do not scale Region B to zero after failback.** It returns to the warm-passive floor; a cold
  passive region is not a passive region.
- **Do not skip the fence** because it seems slow. It takes under 2 seconds and it is the only
  thing preventing the outcome nobody recovers from.

## Verification

Per `docs/disaster-recovery.md` §5 — and note that every verification step reads the authoritative
store directly rather than a Grafana panel, because the failover procedure must be executable with
dashboards down:
```bash
psql "$REGION_B_WRITER" -c "SELECT pg_is_in_recovery();"                  # f
psql "$REGION_B_WRITER" -c "SELECT nextval('dr_promotion_probe_seq');"    # proves writability
aws dynamodb get-item --table-name pp-dr-control \
  --key '{"pk":{"S":"region_authority"}}' --consistent-read                # epoch incremented, active_region correct
```
```sql
-- the audit chain is unbroken
-- I1: sum(refunds.amount) <= captured_amount
-- I3: at most one successful attempt per payment
SELECT payment_id, count(*) FROM pp.payment_attempts
WHERE  outcome = 'SUCCESS' GROUP BY payment_id HAVING count(*) > 1;
-- the ledger balances
SELECT account_id, sum(amount) FROM pp.ledger_entries
GROUP  BY account_id HAVING sum(amount) <> 0;
```
```bash
./bin/platformctl verify-audit-chain ten_…
./bin/platformctl outbox status
```
Then measure and record the achieved RTO and RPO against the ≤ 15 min / ≤ 5 s commitments.

## Follow-up

- **The post-failover audit (§8.3) is mandatory**, not optional, and so is the data reconciliation
  in §8.2: identify every write that was inside the RPO window and did not survive, and decide per
  payment what happens. Some of those are money that moved and has no record.
- Record the measured RTO and RPO, the fence epoch before and after, and every deviation from the
  runbook. **Each deviation is a documentation defect with an owner** — that is the standing rule in
  §9.3.
- Plan failback deliberately. There is no rush; Region B is a full region.
- Feed the quarterly GD-R drill: the drill uses a designated "cold" IC who has not run one before,
  precisely to test that the procedure works for someone who did not write it. If this incident
  needed knowledge that is not in `docs/disaster-recovery.md`, that is the finding.
