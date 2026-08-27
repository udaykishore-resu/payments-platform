# 12 — Event Architecture

## What this shows and why it matters

Every asynchronous edge in the platform runs through one mechanism: a transactional outbox drained
by a single relay into Kafka topics named `pp.<context>.<aggregate>.v1`. Diagram A shows that
publish path and the topic set; Diagram B shows the consumption path with its retry and DLQ
topology, and distinguishes the one consumer group `event-consumer` ships today from the ones
baseline §13.2 lists as consumers. The reason this is a single, boring, uniform mechanism is that the alternative — services
publishing to Kafka directly alongside their database writes — is the dual-write failure mode, and
in a payment system a lost `payment.captured.v1` means a payment that is captured at the gateway
and invisible in the ledger.

## Diagram A — Outbox, relay and topics

```mermaid
flowchart LR
  subgraph TX["One database transaction"]
    ST["State row - payment, merchant, configuration, connection"]
    EV["payment_event_log append, aggregate version increments"]
    AU["audit_records row - hash chained, written by the same UoW"]
    OX["outbox_events row - full CloudEvents envelope"]
  end

  RLY["outbox-relay - the only Kafka publisher"]
  POLL["Poll FOR UPDATE SKIP LOCKED, publish, mark published"]
  BACKLOG["pp_outbox_backlog gauge per topic"]

  subgraph TOPICS["MSK topics"]
    T1["pp.merchants.merchant.v1 - 12 parts, 30 d, key merchant_id"]
    T2["pp.config.configuration.v1 - 12 parts, compacted, key merchant_id"]
    T3["pp.payments.payment.v1 - 48 parts, 30 d, key payment_id"]
    T4["pp.gateways.health.v1 - 6 parts, compacted, key gateway_id"]
    T5["pp.webhooks.inbound.v1 - 24 parts, 7 d, key gateway_ref"]
    T6["pp.audit.v1 - 12 parts, 400 d then S3, key tenant_id"]
  end

  ST --> OX
  EV --> OX
  AU --> OX
  OX --> POLL
  POLL --> RLY
  RLY --> T1
  RLY --> T2
  RLY --> T3
  RLY --> T4
  RLY --> T5
  RLY --> T6
  RLY -.->|"Kafka unavailable, rows retained, backoff, no data loss"| BACKLOG
```

## Diagram B — Consumers, retry and DLQ topology

```mermaid
flowchart TB
  T3["pp.payments.payment.v1"]
  T1["pp.merchants.merchant.v1"]
  T2["pp.config.configuration.v1"]
  T4["pp.gateways.health.v1"]
  T5["pp.webhooks.inbound.v1"]
  T6["pp.audit.v1"]

  subgraph CGB["Built - the projection event-consumer ships today"]
    CWHK["Webhook processor - webhook.received.v1 runs webhook.Processor, which applies the transition and posts the ledger inside one transaction"]
    CACK["Every other type on the subscribed topics is acknowledged with a DEBUG line, never failed"]
  end

  subgraph CGS["Specified in baseline 13.2, not yet a consumer group"]
    CREC["Reconciler"]
    CNOT["Notification dispatcher"]
    CANA["Analytics projections"]
    CCAC["Data plane config cache invalidator"]
    CRTE["Routing feedback"]
    CAUD["Audit sink and SIEM export"]
  end

  DEDUP["INSERT consumer_group and event_id ON CONFLICT DO NOTHING"]
  ZERO["0 rows affected - already processed, ack and drop"]
  HANDLE["Handle inside the same transaction as the dedup row, then commit, then ack"]
  INV["Database invariants I1 to I3 as the last line of defence"]

  RETRY["Sibling .retry topic - bounded attempts, exponential jitter"]
  DLQ["Sibling .dlq topic - 30 d retention, same partition count"]
  ALERT["pp_dlq_depth and pp_consumer_lag alerts"]
  OPSR["Operator replays or discards via platformctl"]

  T5 --> CWHK
  T3 -.-> CREC
  T3 -.-> CNOT
  T3 -.-> CANA
  T3 -.-> CRTE
  T1 -.-> CCAC
  T1 -.-> CNOT
  T2 -.-> CCAC
  T4 -.-> CRTE
  T6 -.-> CAUD
  T3 --> CACK

  CWHK --> DEDUP
  DEDUP --> ZERO
  DEDUP --> HANDLE --> INV
  HANDLE -->|"transient failure"| RETRY
  RETRY -->|"attempts remain"| HANDLE
  RETRY -->|"exhausted or poison message"| DLQ
  DLQ --> ALERT --> OPSR
  OPSR -.->|"replay after fix"| HANDLE
```

## Legend and notes

- **The envelope is CloudEvents 1.0 plus required platform extensions**: `tenantid`,
  `merchantid`, `correlationid`, `causationid`, `traceparent`, `aggregateid`, `aggregateversion`,
  `partitionkey`. `causationid` and `correlationid` together make the full causal chain of a
  payment reconstructible from the event log alone (§13.1).
- **Versioning is in the type name and changes are additive-only within a major.** A breaking
  change is a new `.v2` type published *alongside* `.v1` until every consumer has migrated. There
  is no in-place edit of an event schema, ever (§13.1).
- **Ordering is guaranteed per partition key only, and the key is the aggregate ID.** All events
  for one payment are ordered; there is no global order and no consumer may assume one. This is
  why `pp.payments.payment.v1` has 48 partitions — throughput scales with partitions, and
  correctness does not depend on cross-partition order (§13.3).
- **`pp.config.configuration.v1` and `pp.gateways.health.v1` are compacted.** A cold-starting
  data-plane pod rebuilds its full configuration and health picture by replaying the compacted
  log, which is exactly why the data plane needs no synchronous control-plane call (§15).
- **Kafka being down is not data loss.** The relay simply stops marking rows published; the outbox
  retains them and backs off. `pp_outbox_backlog` is the alert. The eventual-consistency window
  widens; nothing is lost (§24).
- **At-least-once delivery, effectively-once business effect.** Exactly-once delivery is not
  achievable across process and broker boundaries; exactly-once *effect* is, via the dedup table
  inside the handler's transaction plus database invariants underneath (A8, §13.5).
- **`.retry` and `.dlq` are siblings with the same partition count and key**, so a parked message
  replays into the same partition and preserves its ordering relationship with its aggregate's
  other events.
- **`pp.audit.v1` has 400-day retention and then flows to S3** with Object Lock for the 7-year
  WORM requirement (§17.3).
- **An event type a group does not project is acknowledged, not failed.** Returning an error for
  an unhandled type blocks the partition for every *other* type on it, turning "somebody published
  a new event" into an outage of an unrelated projection. `event-consumer` logs it at DEBUG and
  moves on, which makes the unhandled traffic visible instead of fatal.
- **The event catalogue is registered in code, and the registry is the contract.**
  `internal/events/registry.go` binds each of the 25 types to its topic and partition-key field —
  `merchant_id`, `payment_id`, `gateway_id`, `gateway_ref`, `tenant_id` — so a type published
  without a registry entry fails at the publisher rather than landing on the wrong partition.
- **The dotted consumer edges are specified, not built.** `event-consumer` currently registers one
  handler. The ledger is posted by the webhook `Processor` inside the transaction that applies the
  webhook, not by a separate projector; the reconciler is a library type in
  `internal/application/payment` rather than a running consumer group. The dotted edges are drawn
  because §13.2 names those consumers and the topics are keyed for them, and they are dotted
  because nothing subscribes yet.

## Related

- [Design baseline §13 event catalog, envelope, topics, outbox, effectively-once](../spec/00-design-baseline.md)
- [11 — Webhook flow](11-webhook-flow.md), [18 — Observability architecture](18-observability-architecture.md)
- [docs/events.md](../events.md)
