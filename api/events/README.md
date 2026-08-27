# Event schema registry

The binding contract for every event this platform publishes. **Git is the source of
truth**; a Confluent-compatible registry is populated from this directory by CI, so the
registry can never disagree with the code. Runtime lookups resolve the envelope's
`dataschema` URI and are cached.

- Envelope: [`envelope.schema.json`](./envelope.schema.json) — CloudEvents 1.0
  structural compatibility plus the required platform extensions.
- Payloads: one file per event type, defining the `data` object only. The envelope is
  identical for all of them and is not repeated.
- Format: **JSON Schema 2020-12**, not Avro. Our payloads are read by humans during
  incidents, our topics are archived to S3 as JSON, and the evolution rules we need are
  policy rules that a registry's `BACKWARD` mode does not express — it would happily
  allow a semantic change that keeps the shape identical.

File naming and URI: the file is `<type>.schema.json`; it is served at
`https://schemas.example.com/events/<type>.json`, which is exactly what the envelope's
`dataschema` field carries. The two differ only in extension, and CI asserts the mapping.

---

## Registry index

| # | Event type | Schema file | Topic | Partition key | Context / producer | Consumers |
|---|---|---|---|---|---|---|
| 1 | `merchant.created.v1` | `merchant.created.v1.schema.json` | `pp.merchants.merchant.v1` | `merchant_id` | BC-2 · `control-plane-api` | Onboarding, Audit, Analytics |
| 2 | `merchant.validated.v1` | `merchant.validated.v1.schema.json` | `pp.merchants.merchant.v1` | `merchant_id` | BC-3 · `workflow-worker` | Onboarding, Audit |
| 3 | `merchant.kyc_approved.v1` | `merchant.kyc_approved.v1.schema.json` | `pp.merchants.merchant.v1` | `merchant_id` | BC-3 · `workflow-worker` | Onboarding, Audit, Compliance |
| 4 | `merchant.kyc_failed.v1` | `merchant.kyc_failed.v1.schema.json` | `pp.merchants.merchant.v1` | `merchant_id` | BC-3 · `workflow-worker` | Onboarding, Audit, Notification |
| 5 | `merchant.bank_validated.v1` | `merchant.bank_validated.v1.schema.json` | `pp.merchants.merchant.v1` | `merchant_id` | BC-3 · `workflow-worker` | Onboarding, Audit |
| 6 | `merchant.gateway_provisioned.v1` | `merchant.gateway_provisioned.v1.schema.json` | `pp.merchants.merchant.v1` | `merchant_id` | BC-4 · `workflow-worker` | Onboarding, Configuration, Audit |
| 7 | `merchant.certified.v1` | `merchant.certified.v1.schema.json` | `pp.merchants.merchant.v1` | `merchant_id` | BC-3 · `workflow-worker` | Onboarding, Audit |
| 8 | `merchant.activated.v1` | `merchant.activated.v1.schema.json` | `pp.merchants.merchant.v1` | `merchant_id` | BC-2 · `control-plane-api` | Data plane cache, Audit, Notification |
| 9 | `merchant.suspended.v1` | `merchant.suspended.v1.schema.json` | `pp.merchants.merchant.v1` | `merchant_id` | BC-2 · `control-plane-api` | Data plane cache (**priority invalidation**), Audit |
| 10 | `merchant.terminated.v1` | `merchant.terminated.v1.schema.json` | `pp.merchants.merchant.v1` | `merchant_id` | BC-2 · `control-plane-api` | All |
| 11 | `configuration.published.v1` | `configuration.published.v1.schema.json` | `pp.config.configuration.v1` | `merchant_id` | BC-5 · `control-plane-api` | Data plane cache, Audit |
| 12 | `configuration.rolled_back.v1` | `configuration.rolled_back.v1.schema.json` | `pp.config.configuration.v1` | `merchant_id` | BC-5 · `control-plane-api` | Data plane cache, Audit |
| 13 | `payment.created.v1` | `payment.created.v1.schema.json` | `pp.payments.payment.v1` | `payment_id` | BC-6 · `payment-orchestrator` | Ledger, Analytics, Audit |
| 14 | `payment.attempted.v1` | `payment.attempted.v1.schema.json` | `pp.payments.payment.v1` | `payment_id` | BC-6 · `payment-orchestrator` | Analytics, Routing feedback |
| 15 | `payment.authorized.v1` | `payment.authorized.v1.schema.json` | `pp.payments.payment.v1` | `payment_id` | BC-6 · `payment-orchestrator` | Ledger, Notification, Analytics |
| 16 | `payment.captured.v1` | `payment.captured.v1.schema.json` | `pp.payments.payment.v1` | `payment_id` | BC-6 · `payment-orchestrator` | Ledger, Notification, Analytics |
| 17 | `payment.failed.v1` | `payment.failed.v1.schema.json` | `pp.payments.payment.v1` | `payment_id` | BC-6 · `payment-orchestrator` | Ledger, Notification, Routing feedback |
| 18 | `payment.voided.v1` | `payment.voided.v1.schema.json` | `pp.payments.payment.v1` | `payment_id` | BC-6 · `payment-orchestrator` | Ledger, Notification |
| 19 | `payment.refunded.v1` | `payment.refunded.v1.schema.json` | `pp.payments.payment.v1` | `payment_id` | BC-6 · `payment-orchestrator` | Ledger, Notification |
| 20 | `payment.settled.v1` | `payment.settled.v1.schema.json` | `pp.payments.payment.v1` | `payment_id` | BC-8 · `event-consumer` | Ledger, Reconciliation |
| 21 | `payment.disputed.v1` | `payment.disputed.v1.schema.json` | `pp.payments.payment.v1` | `payment_id` | BC-8 · `event-consumer` | Ledger, Risk, Notification |
| 22 | `payment.reconciliation_required.v1` | `payment.reconciliation_required.v1.schema.json` | `pp.payments.payment.v1` | `payment_id` | BC-6 · `payment-orchestrator` | Reconciler (**alerting**) |
| 23 | `webhook.received.v1` | `webhook.received.v1.schema.json` | `pp.webhooks.inbound.v1` | `gateway_ref` | BC-7 · `webhook-ingress` | Webhook processor |
| 24 | `gateway.health_changed.v1` | `gateway.health_changed.v1.schema.json` | `pp.gateways.health.v1` | `gateway_id` | BC-4 · `payment-orchestrator` | Routing, Control plane, Alerting |
| 25 | `audit.recorded.v1` | `audit.recorded.v1.schema.json` | `pp.audit.v1` | `tenant_id` | BC-9 · `event-consumer` | Audit sink, SIEM |

Twenty-five types. `TestCatalogMatchesSchemas` asserts that this table, this directory
and the design baseline's catalogue list exactly the same set — an event that exists in
code but not in the catalogue fails the build.

### Topic configuration

| Topic | Partitions | Retention | Cleanup | RF | `min.insync` |
|---|---:|---|---|---:|---:|
| `pp.merchants.merchant.v1` | 12 | 30 d | delete | 3 | 2 |
| `pp.config.configuration.v1` | 12 | 7 d + compact | compact | 3 | 2 |
| `pp.payments.payment.v1` | 48 | 30 d | delete | 3 | 2 |
| `pp.gateways.health.v1` | 6 | 1 d + compact | compact | 3 | 2 |
| `pp.webhooks.inbound.v1` | 24 | 7 d | delete | 3 | 2 |
| `pp.audit.v1` | 12 | 400 d → S3 | delete | 3 | **3** |
| `*.retry.<tier>` | as parent | 7 d | delete | 3 | 2 |
| `*.dlq` | as parent | 30 d | delete | 3 | 2 |

Two entries carry most of the reasoning. `pp.config.configuration.v1` is **compacted**,
which is why `configuration.published.v1` carries the complete document rather than a
diff: a consumer rebuilding a lost cache must reach a usable state from one record.
`pp.audit.v1` runs `min.insync.replicas=3`, so a single broker loss stalls audit
production — the correct failure mode there, because an audit gap is a compliance
finding and the audit write path is asynchronous and WAL-buffered, so a stall degrades
nothing user-facing.

---

## Compatibility policy

### The rules

| Rule | Statement |
|---|---|
| **V1** | The major version lives **in the type name** (`.v1`). There is no minor version and no version header. |
| **V2** | Within a major version, changes are **additive only**: new **optional** fields with safe absent-semantics. Nothing else. |
| **V3** | A breaking change is a **new type** (`.v2`) published **alongside** `.v1` on the same topic until every consumer has migrated. The `.v1` producer is not removed on the day `.v2` ships. |
| **V4** | Never removed within a major: a field, an enum value's meaning, a field's type, a field's optionality (optional→required is breaking), the partition key, or the semantics of an existing value. |
| **V5** | Adding an **enum value** is breaking unless every consumer's handling of unknown values is specified. Each schema therefore declares `x-unknown-behaviour` per enum (`ignore` \| `reject` \| `route-to-dlq`), and consumers must implement the declared behaviour. Most are `route-to-dlq`: silently ignoring an unrecognised payment state is how money gets lost. |
| **V6** | Consumers **must ignore unknown fields**. A strict-decode consumer turns V2 — which is safe by construction — into an outage. |

### Classification table

| Change | Classification | Action |
|---|---|---|
| Add optional `data.settlementCurrency` | Additive | Ship in `.v1`, bump `x-revision` |
| Add required `data.settlementCurrency` | **Breaking** | New `.v2` |
| Rename `data.gatewayRef` → `data.gatewayReference` | **Breaking** | New `.v2` (a rename is a delete plus an add) |
| Widen `int32` → `int64` | Additive for JSON, **breaking** for typed consumers | Treat as breaking; new `.v2` |
| Add an enum value | **Breaking** unless `x-unknown-behaviour` is `ignore` | Usually new `.v2` |
| Tighten validation (max length, pattern) | **Breaking** for producers of the old shape | New `.v2` |
| Change the partition key | **Breaking**, and worse than most — it silently destroys ordering | New topic, not just a new type |
| Add a new event type | Additive | New schema, registered, no consumer impact |

### Migration protocol

| Phase | Gate to leave it |
|---|---|
| **T0 — register** | `.v2` schema merged, compatibility check green, registry index updated. |
| **T1 — dual publish** | The producer emits both from the **same transaction and the same outbox batch**, so a consumer can never see one without the other. Both carry the same `correlationid`; the `.v2` carries the `.v1`'s `id` as `causationid`. |
| **T2 — consumer migration** | Each consumer declares its supported versions in the registry on startup. The producer may not proceed until the registry reports **zero** consumer groups on `.v1` for **14 consecutive days** — longer than any consumer's maximum outage plus its replay window. |
| **T3 — retire** | `.v1` production stops and the type is marked `DEPRECATED` with a sunset date. Retention expiry removes the last `.v1` records. |

Dual-publishing costs storage and doubles the outbox row count for the affected type.
That is the price of never coordinating a lockstep deploy across nine deployables owned
by five teams.

---

## Delivery and consumption

**At-least-once delivery, effectively-once business effect.** Exactly-once delivery is
not achievable across process and broker boundaries; exactly-once *effect* is, and is
what the business actually needs.

```
receive → dedup INSERT (consumer_group, event_id) ON CONFLICT DO NOTHING
        → 0 rows affected: ACK and drop, already processed
        → else: handle inside the same transaction as the dedup row
        → commit → ACK
```

Database-level business invariants (refunded ≤ captured, captured ≤ authorized, at most
one successful attempt per payment) are the last line of defence, because a bug in the
dedup path must still not be able to move money twice.

**Ordering is per partition key only.** All events for one payment are ordered; all
events for one merchant are ordered. There is no global order, within a topic or across
topics, and no consumer may assume one. `aggregateversion` is monotonic per aggregate
but **not dense** — a consumer that does not subscribe to every type of an aggregate
will legitimately see gaps, so treat it as monotonic-increasing and never as a loss
detector.

Retry topics break ordering by construction. A consumer that fails on version 3, parks
it on `.retry.1m`, and then processes version 4 from the main topic will apply capture
before authorization. Three defences, in order: a `last_applied_version` check per
aggregate that discards the late arrival; idempotent commutative projections keyed on
`source_event_id`; and, for consumers that genuinely cannot tolerate reordering, not
using retry topics at all and blocking the partition instead.

---

## CI checks

| Check | What it does |
|---|---|
| `scripts/verify-events.sh` | Every file parses as JSON, is a valid 2020-12 schema, and **every `examples` entry validates against its own schema**. |
| `TestEveryPublishedEventValidatesAgainstItsSchema` | Every event a producer can emit is generated in a table test and validated. A producer that *can* emit an invalid event fails the build. |
| `scripts/check-event-compat.sh` | Diffs each schema against `main`. Fails on a removed field, optional→required, a type change, an enum removal, an enum addition where `x-unknown-behaviour` ≠ `ignore`, a partition-key change, or a `dataschema` URI reused with different content. |
| `TestCatalogMatchesSchemas` | The baseline catalogue, this directory and the index table above list exactly the same types. |
| PAN detector | Runs over every fixture and example in this directory. A Luhn-valid 13–19 digit sequence fails the build. |
| Consumer contract tests | Golden fixtures per consumed type, plus an unknown-field-injection test that proves V6 compliance. |

Run the schema half locally:

```bash
python3 -m pip install --user jsonschema
python3 scripts/verify_event_schemas.py api/events
```
