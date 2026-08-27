# RB-018: Aurora Global replication lag — RPO at risk

- **Severity:** page (P1)
- **Alert:** `AuroraReplicaLagHigh`
  ```promql
  aws_rds_aurora_global_db_replication_lag > 5
  ```
- **Triggered when:** cross-region replication lag exceeds 5 seconds for 5 minutes. 5 s **is** the
  RPO budget, so the alert threshold and the commitment are the same number.
- **Plane / service:** data · `aurora`
- **Related:** `docs/disaster-recovery.md` §1.1, §4.1.1, §4.1.2,
  `docs/adr/ADR-021-active-passive-money-active-active-control.md`,
  [region-failover.md](region-failover.md), [aurora-failover.md](aurora-failover.md)

## What this means

Aurora Global replicates at the **storage** layer — the storage tier ships log records directly,
bypassing the database engine. Consequences that shape this runbook:

- Lag is dominated by inter-region network RTT (~12 ms `eu-west-1` → `eu-central-1`), not by write
  volume or by load on the secondary. Typical lag is 200–800 ms; the 5 s budget is roughly 400×
  typical.
- Replication **cannot** fall behind because of a slow query, a vacuum, or a schema migration on
  the secondary — the secondary has no independent write path. So "something is running on the
  replica" is not a valid hypothesis here, which rules out the usual suspects immediately.
- A commit is acknowledged after quorum in the **local** region's storage. In-region RPO is 0;
  cross-region RPO is greater than 0. That asymmetry is the whole reason the DR document has a
  §6 walkthrough.

Above 5 s, a region failover would lose more committed data than the DR plan promises. Nothing is
wrong *right now* — this is a promise about the future becoming unreliable.

There is a companion condition that is worse and quieter: replication having **stopped entirely**.
```promql
changes(aws_rds_aurora_global_db_replicated_write_io[10m]) == 0
  and sum(rate(pp_payments_total{outcome="created"}[10m])) > 0
```
Zero replication with live traffic is silent, which is why it has its own query.

## Impact

- **No merchant impact. No money at risk. Nothing is degraded.** The primary region is serving
  normally.
- What is degraded is the **guarantee**: if the region were lost right now, we would lose more than
  5 seconds of committed payments. Concretely, payments that were accepted, acknowledged and
  authorized — money that moved — whose records would not exist in the promoted region.
- This is why it pages despite having no present-tense symptom.

## Immediate triage (first 5 minutes)

1. Current lag, trend, and whether replication is moving at all:
   ```promql
   aws_rds_aurora_global_db_replication_lag
   avg_over_time(aws_rds_aurora_global_db_replication_lag[15m])
   changes(aws_rds_aurora_global_db_replicated_write_io[10m])
   pp_dr_replication_lag_seconds
   ```
   `pp_dr_replication_lag_seconds` is the **independent end-to-end probe** and is the number the
   RPO SLO is measured against — a CloudWatch metric can be stale or wrong, and the commitment is
   to the business, not to CloudWatch. If the two disagree, believe the probe.
2. Confirm with the heartbeat directly:
   ```sql
   -- on the Region B secondary
   SELECT EXTRACT(EPOCH FROM (clock_timestamp() - written_at)) AS observed_lag_seconds
   FROM   dr_heartbeat WHERE region = 'eu-west-1';
   ```
3. What is the primary doing?
   ```promql
   pp:payments:tps5m
   rate(pp_outbox_published_total[5m])
   ```
   ```sql
   SELECT count(*) FROM pg_stat_activity WHERE state = 'active';
   SELECT query, now() - query_start AS runtime FROM pg_stat_activity
   WHERE  state = 'active' AND now() - query_start > interval '1 minute'
   ORDER  BY runtime DESC LIMIT 10;
   ```
4. Is a bulk operation running? These are the three named causes:
   ```sql
   SELECT pid, phase, blocks_done, blocks_total FROM pg_stat_progress_create_index;
   SELECT pid, phase, tuples_done, tuples_total FROM pg_stat_progress_vacuum;
   ```
5. AWS-side events:
   ```bash
   aws rds describe-events --source-type db-cluster --duration 60 \
     --source-identifier pp-prod --query 'Events[].{t:Date,m:Message}'
   aws cloudwatch get-metric-statistics --region eu-central-1 \
     --namespace AWS/RDS --metric-name AuroraGlobalDBReplicationLag \
     --dimensions Name=DBClusterIdentifier,Value=pp-prod-secondary \
     --start-time "$(date -u -d '-60 min' +%FT%TZ)" --end-time "$(date -u +%FT%TZ)" \
     --period 60 --statistics Maximum Average
   ```

## Diagnosis

- **A large index build is running** → the write volume of the build is being replicated. → *M1*.
- **A bulk backfill or migration is running** → same cause, different origin. → *M1*.
- **`pp:payments:tps5m` at a record high** → a genuine write burst. → *M2*.
- **`changes(aws_rds_aurora_global_db_replicated_write_io[10m]) == 0` with live traffic** →
  replication has **stopped**, not slowed. This is the worse case and it is silent. → *M3*,
  immediately.
- **CloudWatch says fine, the heartbeat probe says lagging** → believe the probe; CloudWatch is
  stale. → *M3*.
- **Lag rose at an AWS maintenance event** → AWS-side. → *M3*.
- **Lag correlates with nothing on our side and is steady around 5–8 s** → an inter-region network
  issue. → *M3*, AWS case.
- **A region failover is already being considered** → record the lag first; it *is* the measured
  RPO for the event. → [region-failover.md](region-failover.md).

## Mitigation

**M1 — pause or throttle the bulk operation.** This is the fix in the majority of cases:
```sql
-- identify it
SELECT pid, left(query, 160), now() - query_start AS runtime FROM pg_stat_activity
WHERE  state = 'active' AND query ~* 'create index|reindex|update|insert into .* select'
ORDER  BY runtime DESC;
-- stop it
SELECT pg_cancel_backend(<pid>);
```
Expected: lag returns to the 200–800 ms band within a few minutes. Reschedule the operation for a
low-traffic window and, if it is a migration, chunk it — `./bin/platformctl migrate status` shows
what is pending and `./scripts/check-migrations.sh` enforces the static rules on migration shape.

**M2 — accept the burst, with a deadline.** If lag is caused by legitimate peak traffic, the
correct action is to watch it, tell the incident channel the RPO is temporarily at risk, and set a
30-minute deadline. Shedding payments to protect a DR promise is the wrong trade at 6 s of lag; it
becomes the right conversation somewhere north of a minute.

**M3 — AWS escalation.** Stopped replication, or lag with no local cause, is an AWS case. Include:
the global cluster identifier, both cluster identifiers, the CloudWatch series from step 5, our
independent probe's series, and the exact start time. Ask specifically whether replication is
progressing at the storage layer.

**M4 — do not fail over because of lag.** Promotion is the thing lag makes *worse*, not better: an
unplanned promotion loses everything inside the lag window. If the primary region is healthy, the
correct action is to reduce lag, not to move.

## Rollback / escalation

- **Lag above 30 seconds** → Sev-1. The DR promise is materially broken and the business owner
  should know before, not after, a region event.
- **Replication stopped** → Sev-1 immediately regardless of the lag figure. A stopped replica is a
  region with no viable failover target, and it looks fine on a dashboard until you need it.
- **Lag above budget for more than 1 hour** → notify the business owner. RPO is a commitment in
  contracts; a sustained breach is a disclosure question, not just an engineering one.
- **If a region event occurs while lag is high**: record the pre-promotion lag first — that number
  is the measured RPO for the event, it is evidence, and it is gone the moment you promote. Step 1
  of the promotion procedure exists for exactly this.
- **Never disable the lag alert to reduce noise.** It is the only warning that the DR plan has
  stopped being true.

## Verification

```promql
aws_rds_aurora_global_db_replication_lag < 1
avg_over_time(aws_rds_aurora_global_db_replication_lag[15m]) < 2
changes(aws_rds_aurora_global_db_replicated_write_io[10m]) > 0
pp_dr_replication_lag_seconds < 1
```
And confirm with the independent probe rather than only with CloudWatch:
```sql
SELECT EXTRACT(EPOCH FROM (clock_timestamp() - written_at)) AS observed_lag_seconds
FROM   dr_heartbeat WHERE region = 'eu-west-1';
```
Typical is 200–800 ms. Anything persistently above 2 s is not "recovered", it is "less bad".

## Follow-up

- Record peak lag, duration above budget, and the cause. The DR game-day report cites these
  numbers, so they need to be written down while they are known.
- If a bulk operation caused it, the finding is the process: large index builds and backfills belong
  in a scheduled window with lag monitored, and that belongs in `docs/deployment.md`.
- If CloudWatch and the probe disagreed, that is worth its own ticket. The probe exists precisely
  because the metric can be wrong, and a confirmed divergence is evidence for trusting it more.
- Verify the next quarterly GD-R drill covers the shape of this incident. `./scripts/dr-drill.sh
  --dry-run` prints every command the RD-1 restore drill would run;
  `./bin/platformctl dr-drill --scenario=region-failover` prints the failover plan without
  executing it.
