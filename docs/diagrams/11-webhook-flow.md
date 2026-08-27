# 11 — Webhook Ingestion Flow

## What this shows and why it matters

Gateway webhooks are the platform's asynchronous truth channel: they close the loop on 3DS
completions, asynchronous payment methods, settlements, disputes and — most importantly —
`TIMEOUT_UNKNOWN` attempts that nothing else can resolve. They are also public-internet traffic
from an unauthenticated-until-verified source, arriving in unpredictable spikes and with
duplicates guaranteed by every gateway's at-least-once delivery. The design answer is a strict
split: `webhook-ingress` has a ≤ 50 ms budget and does **accept-and-persist only**; all
interpretation is asynchronous.

## Diagram A — Ingestion sequence

```mermaid
sequenceDiagram
    autonumber
    participant GW as Gateway
    participant WAF as WAF and ALB
    participant WI as webhook-ingress
    participant SM as Secrets Manager
    participant DB as Aurora writer
    participant OR as outbox-relay
    participant KF as pp.webhooks.inbound.v1
    participant EC as event-consumer
    participant PO as payment-orchestrator
    participant DQ as Retry and DLQ topics

    GW->>WAF: POST /v1/webhooks/gateway with signature header
    WAF->>WI: forward
    WI->>SM: fetch webhook signing secret, cached
    WI->>WI: verify signature per the gateway signature scheme
    alt Signature invalid
        WI-->>GW: 401 WEBHOOK_SIGNATURE_INVALID, security event raised
    end
    WI->>WI: replay check, timestamp skew within 5 min and nonce unused
    alt Skew exceeded or nonce reused
        WI-->>GW: 401 WEBHOOK_REPLAY_DETECTED, security event raised
    end
    WI->>DB: dedup insert on webhook_dedup, gateway_ref unique
    alt Duplicate delivery
        DB-->>WI: 0 rows affected
        WI-->>GW: 200 OK, dropped silently, counter incremented
    end
    WI->>DB: persist raw envelope to inbound_webhooks plus outbox row, one transaction
    WI-->>GW: 200 OK within the 50 ms budget
    Note over WI,GW: acknowledged before any interpretation, so a slow consumer never causes gateway retries

    DB->>OR: outbox row
    OR->>KF: webhook.received.v1 keyed by gateway_ref
    KF->>EC: consume
    EC->>EC: dedup insert on consumer_group plus event_id
    EC->>EC: ACL translates the gateway payload into a domain intent
    EC->>PO: apply intent
    PO->>PO: L6 payload validation then L7 state transition guard
    alt Transition legal
        PO->>DB: new payment state plus outbox row, one transaction
        DB->>KF: payment.authorized.v1 or captured or settled or disputed
    else Transition illegal or out of order
        PO-->>EC: 409 INVALID_STATE_TRANSITION
        EC->>EC: classify, late or duplicate signal is dropped, genuine conflict is retried
    end
    EC->>DQ: retries exhausted, park on the dlq with the full error chain
```

## Diagram B — Ordering, lateness and the reconciliation tie-in

```mermaid
flowchart TB
  IN["Inbound webhook persisted"]
  KEY["Kafka key is gateway_ref, not payment_id"]
  REKEY["Consumer resolves gateway_ref to payment_id via the attempt row"]
  ORD["Per-payment ordering is re-established by the aggregate version, not by the topic"]
  VER["Compare event aggregateversion to the stored payment version"]
  OLD["Stale or already-applied signal"]
  DROP["Drop, increment a counter, no state change"]
  NEW["Newer signal"]
  APPLY["Apply through the FSM"]
  UNK["Attempt was TIMEOUT_UNKNOWN"]
  RESOLVE["Webhook resolves the unknown, payment leaves PROCESSING"]
  NOWH["No webhook within the SLA"]
  POLL["Reconciler polls the gateway lookup API with the derived idempotency key"]
  SETL["Still unresolved, settlement report resolves it"]
  EXC["Unresolved past threshold, open a reconciliation_exception and alert"]

  IN --> KEY --> REKEY --> ORD --> VER
  VER --> OLD --> DROP
  VER --> NEW --> APPLY
  APPLY --> UNK --> RESOLVE
  UNK --> NOWH --> POLL
  POLL -->|"gateway has no record"| SETL
  SETL --> EXC
  POLL -->|"gateway confirms outcome"| RESOLVE
```

## Legend and notes

- **Four independent defences run before persistence**, in this order and for this reason:
  signature (is this really the gateway?), replay window (is this a captured-and-replayed
  request?), dedup (have we already seen this exact delivery?), then persist. Reordering them
  would let an attacker's replayed body consume a dedup slot or reach storage.
- **The 200 is returned before interpretation.** Gateways retry aggressively on non-2xx; a
  webhook endpoint that does work inline turns a slow database into an exponentially growing
  redelivery storm. Accept-and-persist keeps the p99 inside 50 ms regardless of downstream health
  (§5).
- **Duplicates are dropped silently and counted, not errored.** Every gateway delivers
  at-least-once. A duplicate is normal operation, not an incident (§24).
- **The topic key is `gateway_ref`, so the topic does not give per-payment ordering.** Ordering is
  re-established at apply time by comparing the event's `aggregateversion` against the stored
  payment version, and by the FSM itself refusing illegal transitions. No consumer may assume a
  global order (§13.3).
- **A late webhook is not an error.** `SETTLED → PROCESSING` is explicitly invalid (§9), so a
  webhook that arrives after a later signal already advanced the payment is dropped by L7 rather
  than corrupting state. The classification step in the `else` branch distinguishes "harmlessly
  late" from "genuine conflict worth retrying".
- **This is resolution path (a) for `TIMEOUT_UNKNOWN`.** Diagram B shows the full ladder — webhook,
  then reconciler polling with the deterministic gateway idempotency key, then settlement report,
  then a human-visible reconciliation exception (§12.3).
- **Effectively-once, not exactly-once.** The consumer inserts `(consumer_group, event_id)` into
  the dedup table inside the same transaction as its handler; a conflict means "already processed,
  ack and drop". Database invariants I1–I3 remain the last line of defence in case the dedup path
  itself has a bug (A8, §13.5).

## Related

- [Design baseline §13.5 effectively-once, §12.3 timeout rule, §19.2 webhook endpoint, §24 failure catalog](../spec/00-design-baseline.md)
- [08 — Payment flow](08-payment-flow.md), [12 — Event architecture](12-event-architecture.md)
- [docs/events.md](../events.md), [docs/failure-handling.md](../failure-handling.md)
