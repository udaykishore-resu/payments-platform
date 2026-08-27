# RB-012: Kafka (MSK) under-replicated partitions

- **Severity:** page (P1)
- **Alert:** `KafkaUnderReplicated`
  ```promql
  kafka_cluster_partition_underreplicated > 0
  ```
- **Triggered when:** any partition has fewer in-sync replicas than its replication factor, for
  5 minutes.
- **Plane / service:** data · `kafka` (MSK, 3 brokers, one per AZ)
- **Related:** `docs/failure-handling.md` F-8, `docs/disaster-recovery.md` §4.2,
  `docs/adr/ADR-020-kafka-event-backbone.md`, [outbox.md](outbox.md),
  [consumer-lag.md](consumer-lag.md)

## What this means

Topics are `RF=3` with `min.insync.replicas=2`. One broker down leaves 2 in-sync replicas, which
still satisfies the minimum, so producers keep working — degraded but functional. **One more broker
loss stops producers entirely**, and the outbox begins to back up.

`unclean.leader.election` is disabled. That is why this is an **availability** risk rather than a
**data-loss** risk: the cluster will refuse to elect an out-of-sync replica as leader, preferring
unavailability to silently losing acknowledged writes.

There is deliberately **no MirrorMaker2 and no cross-region replication** of Kafka
(`docs/disaster-recovery.md` §4.2). In a region failover, the event stream is rebuilt from the
outbox in the promoted region, which is the authoritative record. Kafka is transport, not truth.

## Impact

- **Right now, at RF=3 with 2 ISR: nothing user-visible.** Payments are written, the outbox
  accepts, the relay publishes.
- **One more broker loss**: producers stop. The outbox retains everything and backs up
  ([outbox.md](outbox.md)); the payment path still works, because the outbox is inside the
  transaction and the relay is not. Projections, ledger, merchant webhooks and config invalidation
  all freeze.
- **Config invalidation degrades to TTL-based expiry** (≤ 30 s bounded staleness), so the data
  plane keeps working on cached configuration — see [config-staleness.md](config-staleness.md) for
  where that ends.
- **No data loss** in either case. That is the whole point of the outbox plus disabled unclean
  election.

## Immediate triage (first 5 minutes)

1. How many partitions, and are the brokers actually up?
   ```promql
   kafka_cluster_partition_underreplicated
   kafka_controller_active_controller_count      # must be exactly 1
   kafka_server_broker_state
   ```
2. Which broker is missing, and why:
   ```bash
   aws kafka describe-cluster --cluster-arn "$PP_MSK_ARN" \
     --query 'ClusterInfo.{State:State,Brokers:NumberOfBrokerNodes}'
   aws kafka list-nodes --cluster-arn "$PP_MSK_ARN" \
     --query 'NodeInfoList[].BrokerNodeInfo.{Id:BrokerId,Endpoint:Endpoints[0]}'
   ```
3. Is the outbox already backing up? This is the clock on the incident:
   ```bash
   ./bin/platformctl outbox status
   ```
   ```promql
   pp_outbox_backlog
   rate(pp_outbox_published_total[5m])
   pp_consumer_lag
   ```
4. Is this an AZ event rather than a broker event?
   ```bash
   kubectl get nodes -L topology.kubernetes.io/zone
   ```
   If a whole AZ is gone, this is `docs/failure-handling.md` F-16 and the cluster autoscaler is
   already adding capacity in the survivors.
5. Confirm the payment path is genuinely unaffected before reporting that it is:
   ```promql
   sum by (status) (rate(pp_http_requests_total{route="/v1/payments"}[5m]))
   pp:payments:tps5m
   ```

## Diagnosis

- **One broker down, controller count is 1, ISR shrunk to 2** → a single broker failure. MSK
  replaces it automatically. → *M1* (wait, with a deadline), *M3* if the outbox is growing.
- **Controller count is 0 or greater than 1** → a controller election problem, which is more
  serious than a broker loss. → *M4*, escalate to AWS support.
- **All three brokers up but partitions still under-replicated** → replication is falling behind,
  usually disk or network saturation on one broker. → *M2*.
- **Disk usage above ~85 % on any broker** → the broker will stop accepting writes before it
  recovers. This is one of only two resource conditions the platform pages on. → *M2* urgently.
- **A whole AZ is unavailable** → F-16; MSK is multi-AZ and will re-elect leaders. → *M1*.
- **Producers are already failing (`OutboxStalled`)** → this has crossed from risk into outage.
  → [outbox.md](outbox.md), and *M3*.
- **Under-replication started at a topic creation or partition change** → new partitions have not
  finished replicating; usually self-resolving. → *M1*.

## Mitigation

**M1 — let MSK replace the broker, with a deadline.** MSK replaces a failed broker automatically
and the partition catches up. Watch:
```promql
kafka_cluster_partition_underreplicated     # must trend to 0
```
Deadline: 30 minutes. Past that, or if `pp_outbox_backlog` crosses 10 000 and rising, go to *M3*
and open an AWS support case in parallel.

**M2 — relieve broker pressure.** Reduce retention on the highest-volume topic to reclaim disk, or
scale broker storage:
```bash
aws kafka update-broker-storage --cluster-arn "$PP_MSK_ARN" \
  --current-version "$PP_MSK_VERSION" \
  --target-broker-ebs-volume-info 'KafkaBrokerNodeId=ALL,VolumeSizeGB=<larger>'
```
Expected: storage grows without a restart; replication catches up. **Do not reduce `.dlq`
retention** — that is the 30-day window in which a parked message can still be triaged
([dlq-triage.md](dlq-triage.md)).

**M3 — protect the outbox.** The outbox is designed to absorb this, but it is not infinite. If the
backlog is growing:
- Confirm the relay is not making things worse by retrying hard:
  ```bash
  kubectl -n pp-data-plane logs deploy/outbox-relay --since=10m | tail -40
  ```
- Do **not** scale the relay up. More producers against a struggling cluster is a stampede; the
  relay's exponential backoff is the correct behaviour.
- Do **not** stop the payment path to reduce event production. The decoupling exists precisely so
  that this trade never has to be made.

**M4 — AWS escalation.** Controller problems, or under-replication that does not resolve, are an
MSK support case. Include: cluster ARN, the under-replicated partition count over time, broker
node IDs, and the exact window.

**M5 — do not enable unclean leader election.** It will be suggested, because it makes the metric
go green. It makes the metric go green by electing a leader that is missing acknowledged writes —
events the platform has already told itself it published. On a payment platform those are ledger
entries and state transitions. The answer is no.

## Rollback / escalation

- **Two brokers down** → producers stop; this is Sev-1. Notify the incident commander and prepare
  for an extended outbox backlog. The payment path continues; say so clearly, because the instinct
  will be to assume everything is down.
- **Outbox backlog past 100 000 rows or the oldest row past 15 minutes** → Sev-1 per the
  queue-depth table (`docs/failure-handling.md` §5.3).
- **Disk above 90 % on any broker** → Sev-1, immediate storage increase. A full broker is an
  unrecoverable broker.
- **If a region failover is being considered**, note that Kafka is **not** replicated across
  regions by design. The promoted region rebuilds the stream from its own outbox.
  [region-failover.md](region-failover.md) covers the sequence.

## Verification

```promql
kafka_cluster_partition_underreplicated == 0
kafka_controller_active_controller_count == 1
rate(pp_outbox_published_total[5m]) > 0
pp_outbox_backlog < 1000
pp_consumer_lag < 1000
```
```bash
./bin/platformctl outbox status     # "oldest none — the outbox is drained"
```
After a large drain, verify the consumers absorbed it and the ledger balances — see
[consumer-lag.md](consumer-lag.md)'s verification. Ordering per partition key is preserved by the
outbox's stable shard bucket, so a drain does not reorder an aggregate's events; confirm it anyway
on the ledger, which is where reordering would show.

## Follow-up

- Record: how long under-replicated, whether producers ever stopped, peak outbox backlog, and the
  drain time.
- If disk was the cause, retention and volume sizing are the finding, and the number belongs in
  `docs/deployment.md` rather than in someone's memory.
- If `min.insync.replicas` was ever discussed as a lever, write down in the postmortem why it was
  not changed: lowering it trades acknowledged-write durability for availability, on a system whose
  events are money.
- Confirm the chaos coverage: `tests/chaos/kafka_loss_test.go::TestOutboxRetainsAndDrains`
  partitions Kafka for 5 minutes under load and asserts zero event loss, correct per-key ordering
  after the drain, and a balanced ledger. If this incident's shape is not that shape, extend it.
