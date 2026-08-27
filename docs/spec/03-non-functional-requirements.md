# 03 — Non-Functional Requirements

> Purpose: state the qualities the system must exhibit — latency, throughput, scale, availability, durability, security, privacy, compliance, observability, operability, maintainability, portability, cost and accessibility — as numbered, measurable requirements `NFR-01..NFR-61`, each with the design mechanism that delivers it. Derived from [`00-design-baseline.md`](./00-design-baseline.md) §18, §22 and §24; the baseline's numbers are binding and are elaborated here, never contradicted.

---

## 0. How to read an NFR

Every requirement carries five fields.

| Field | Meaning |
|---|---|
| **Statement** | The quality, stated so it can be falsified. |
| **Target** | A number with a unit and a window. "Fast" is not a target. |
| **Measured by** | The instrument. If nothing measures it, it is not a requirement, it is a wish. |
| **On violation** | What automatically happens. A quality with no consequence is decoration. |
| **Mechanism** | The design element that delivers it, traceable to a baseline section. |

Two planning scenarios are used throughout. All arithmetic is shown in §14.

| | **P1 — Year-1 planning** | **P2 — Design ceiling** |
|---|---|---|
| Merchants / tenants | 50 000 / 500 | 50 000 / 500 |
| Payments per day | 10 000 000 | 100 500 000 |
| Average TPS | 115.7 | 1 163 |
| Diurnal peak TPS | 500 | 5 000 sustained |
| Burst TPS | 1 500 | 15 000 |

P2's sustained figure is baseline §18's 5 000 TPS per region; P1 is the volume the first commercial
year is planned against. Every capacity NFR states which scenario its number belongs to.

---

## 1. Performance and latency

### NFR-01 — Payment API server-side latency
**Statement.** The synchronous payment path completes inside its latency budget excluding time spent inside third-party gateways.
**Target.** p50 ≤ 60 ms, p99 ≤ 250 ms, p99.9 ≤ 600 ms, excluding gateway time; measured at the server, per region, over a 5-minute window.
**Measured by.** `pp_http_request_duration_seconds{service="payment-api",route="/v1/payments"}` with gateway time subtracted using the span attribute `gateway.duration_ms`.
**On violation.** Latency SLI burn-rate alerting: 14.4× over 1 h pages; 6× over 6 h opens a ticket (baseline §22.4). Sustained breach triggers the error-budget policy (NFR-23).
**Mechanism.** The staged pipeline of baseline §12 with a per-stage budget summing to 77 ms (§14.1), connection release before the gateway call, cached merchant context, and pure L5/L7 rules with no network calls.

### NFR-02 — Per-stage latency budgets are individually enforced
**Statement.** Each pipeline stage stays within its allotted budget, so a regression is attributable to a stage rather than to "the API".
**Target.** authn ≤ 2 ms, tenant resolution ≤ 1 ms, authz ≤ 2 ms, rate limit ≤ 2 ms, L1 ≤ 3 ms, idempotency claim ≤ 8 ms, merchant context ≤ 5 ms, L5 ≤ 5 ms, risk ≤ 15 ms, routing ≤ 5 ms, attempt persist+dispatch ≤ 10 ms, L6 ≤ 3 ms, L7+outbox ≤ 10 ms, idempotency completion ≤ 5 ms — all p99.
**Measured by.** A span per stage with a recorded duration; a recording rule per stage p99.
**On violation.** The owning stage's alert fires with its own runbook; the aggregate SLO alert is suppressed as a duplicate so the page names a cause, not a symptom.
**Mechanism.** Explicit instrumentation at stage boundaries; CI performance test asserting each stage's p99 against a fixture load.

### NFR-03 — End-to-end payment latency including gateway
**Statement.** A payment resolves end to end within a budget that leaves room for one failover.
**Target.** p99 ≤ 1.5 s including gateway time. Derived gateway budget: 1 250 ms p99 (§14.1). Failover is attempted only if elapsed wall time < 700 ms.
**Measured by.** Trace-derived duration from ingress span start to response span end.
**On violation.** Gateways whose p99 exceeds 1 250 ms over a 30 s window transition toward `DEGRADED` and are de-prioritised by routing (baseline §10).
**Mechanism.** 8 s adapter hard timeout as a correctness ceiling, not a budget; the 700 ms failover cut-off (FR-64); routing score weighting latency at 0.1 by default.

### NFR-04 — Gateway call isolation
**Statement.** A slow gateway cannot degrade traffic to a healthy one.
**Target.** With one gateway artificially delayed to 8 s on 100 % of calls, p99 latency for merchants routed to other gateways increases by ≤ 10 ms.
**Measured by.** `tests/chaos/gateway_test.go::TestSlowGatewayDegradesLatencyAndTimesOutSafely`, with a per-gateway latency assertion.
**On violation.** Release blocked; the bulkhead sizing is a defect.
**Mechanism.** Per-gateway semaphore bulkhead and circuit breaker in `payment-orchestrator`; separate binaries for `payment-api` and `payment-orchestrator` so ingress connections are not consumed by in-flight gateway calls (baseline §5).

### NFR-05 — Webhook ingress accept latency
**Statement.** Webhook acknowledgement is fast enough that no gateway disables our endpoint.
**Target.** p99 ≤ 50 ms from request receipt to `200`, including signature verification and durable persistence.
**Measured by.** `pp_http_request_duration_seconds{service="webhook-ingress"}`.
**On violation.** Page at p99 > 50 ms for 5 minutes — a slow webhook endpoint is a silent reconciliation outage.
**Mechanism.** Accept-and-persist only; all interpretation is asynchronous (FR-74). Single-statement insert, no joins, no cross-service calls on the accept path.

### NFR-06 — Control-plane latency
**Statement.** Administrative operations are responsive enough for interactive use but are explicitly *not* on the money path.
**Target.** p95 ≤ 300 ms, p99 ≤ 800 ms for reads; p99 ≤ 2 s for configuration publish (which runs full L4 validation).
**Measured by.** `pp_http_request_duration_seconds{service="control-plane-api"}`.
**On violation.** Ticket, not a page. The control plane's 99.9 % target and its exclusion from the payment path are what make this acceptable.
**Mechanism.** Separate deployable, separate scaling driver, separate availability target (baseline §5).

---

## 2. Throughput

### NFR-07 — Sustained payment throughput
**Statement.** The data plane sustains its rated throughput per region without SLO degradation.
**Target.** P2: 5 000 TPS sustained, 15 000 TPS peak for ≥ 10 minutes, per region, with 3× headroom over the provisioned steady state.
**Measured by.** `tests/load` scenarios run against a production-shaped environment before each minor release; `pp_payments_total` rate in production.
**On violation.** Release blocked. In production, autoscaling plus the adaptive concurrency limiter sheds load with `429` rather than degrading latency for everyone.
**Mechanism.** Stateless `payment-api`, horizontally scaled `payment-orchestrator`, 48-partition payment topic, connection pools sized by Little's Law (§14.4).

### NFR-08 — Onboarding throughput
**Statement.** Onboarding volume never competes with payment traffic for resources.
**Target.** 2 000 concurrent onboarding cases; 500 workflow step executions per second; automated portion p95 ≤ 30 min (baseline §18).
**Measured by.** `pp_workflow_instances{state}`, `pp_workflow_step_duration_seconds`, `pp_onboarding_duration_seconds`.
**On violation.** Scale `workflow-worker`; if the backlog persists, the leasing query is the suspect (index on `(state, lease_expires_at)`).
**Mechanism.** `workflow-worker` is a separate deployable on the Automation plane with its own node pool and its own database connection budget.

### NFR-09 — Webhook ingestion throughput
**Statement.** Webhook volume is spiky and must be absorbed without backpressure onto gateways.
**Target.** P2: 7 000 webhooks/s sustained (1.4 per payment), 25 000/s burst for 60 s.
**Measured by.** `pp_http_requests_total{service="webhook-ingress"}` and `pp_consumer_lag{topic="pp.webhooks.inbound.v1"}`.
**On violation.** Horizontal scale on request rate; the accept path is stateless apart from one insert, so scaling is linear until the database write path saturates (24 partitions of the inbound topic bound consumer parallelism, not ingress).
**Mechanism.** FR-74's accept-and-persist; 24-partition topic; batch consumers.

### NFR-10 — Event pipeline throughput
**Statement.** The outbox relay and event consumers keep pace with the payment rate.
**Target.** Outbox backlog p99 ≤ 1 000 rows and ≤ 5 s age; consumer lag p99 ≤ 10 s; webhook processing lag p99 ≤ 60 s (baseline §22.4).
**Measured by.** `pp_outbox_backlog{topic}`, `pp_consumer_lag{topic,group}`.
**On violation.** Backlog > 10 000 or age > 60 s pages; the relay scales on backlog. Kafka unavailability holds rows in the outbox with backoff — no data loss, widening consistency window (baseline §24).
**Mechanism.** `FOR UPDATE SKIP LOCKED` batch polling in `outbox-relay`; per-partition consumer concurrency sized in §14.3.

### NFR-11 — Routing decision throughput
**Statement.** Routing is a pure in-process computation and never becomes a throughput bottleneck.
**Target.** p99 ≤ 5 ms at 15 000 TPS; zero network calls; zero database reads.
**Measured by.** Stage span; a micro-benchmark gate in CI at ≥ 200 000 plans/s/core.
**On violation.** CI benchmark regression > 20 % blocks the merge.
**Mechanism.** Routing evaluates against the in-memory merchant configuration snapshot and the in-memory gateway health map, both maintained by Kafka consumers (baseline §15).

### NFR-12 — Idempotency claim throughput
**Statement.** The idempotency claim is the most contended write on the payment path and must not serialise.
**Target.** p99 ≤ 8 ms at 15 000 TPS; zero lock-wait timeouts; no measurable hot-partition skew.
**Measured by.** `pp_idempotency_outcomes_total`, PostgreSQL `pg_stat_statements` for the claim statement, lock-wait histograms.
**On violation.** Page. A serialising idempotency claim caps the whole platform's throughput.
**Mechanism.** `INSERT … ON CONFLICT DO NOTHING` on a unique index whose leading column is the high-entropy key — never a monotonic column — so B-tree insert points are spread rather than concentrated on the rightmost leaf.

---

## 3. Scalability and data growth

### NFR-13 — Horizontal scalability of stateless services
**Statement.** Every data-plane service scales linearly with added replicas up to the database's limits.
**Target.** Throughput scales ≥ 0.9× linearly from 4 to 32 replicas; no replica holds state that another cannot serve.
**Measured by.** Load test at replica counts 4, 8, 16, 32.
**On violation.** Investigate shared state — the usual culprits are a cache stampede, a shared lease, or a singleton scheduler.
**Mechanism.** All state in PostgreSQL, Redis or Kafka; leases (`workflow_instances`, `outbox_events`) are row-level and use `SKIP LOCKED` rather than a global lock; HPA on CPU and on a custom in-flight-requests metric.

### NFR-14 — Vertical and storage scaling of the primary datastore
**Statement.** The relational tier scales vertically and by read replica within the P1 envelope, and has a defined, tested horizontal path for P2.
**Target.** P1 hot working set 34.8 TB fits within one Aurora cluster (limit 128 TiB ≈ 140.7 TB) with ≥ 4× headroom (§14.2). P2 (348 TB/yr) requires tenant-sharding, which must be exercised in a rehearsal before P1 volumes exceed 40 % of the cluster limit.
**Measured by.** `aurora_volume_bytes_used`; a quarterly capacity review against the growth curve.
**On violation.** At 40 % of the volume limit, the sharding rehearsal becomes a release-blocking task. At 70 %, sharding is executed.
**Mechanism.** `tenant_id` is the leading column of the physical design everywhere and is the shard key; no cross-tenant query exists in the application (enforced by RLS and by `TestCrossTenantAccessIsImpossible`), so tenant-sharding requires no query rewrite.

### NFR-15 — Partitioning and retention mechanics
**Statement.** Data ages out by partition detachment, never by bulk `DELETE`.
**Target.** Partition maintenance completes in ≤ 60 s per operation with zero impact on p99; ≤ 900 leaf partitions in the hot window; partition pruning eliminates ≥ 99 % of leaves for a single-day query.
**Measured by.** `EXPLAIN` assertions in integration tests; maintenance job duration.
**On violation.** Partition count above 900 degrades planning time; the roll-up job is the remediation and is alerted on.
**Mechanism.** `payments`, `payment_attempts`, `payment_events`, `ledger_entries`, `audit_records` are `RANGE` partitioned on their timestamp and `HASH` sub-partitioned 8-way on `tenant_id`. Daily leaves for 92 days, rolled up to monthly leaves thereafter → 92×8 + 10×8 = **816 leaves** in a 13-month hot window (§14.2). Ageing is `DETACH PARTITION` + export to Parquet + `DROP`.

### NFR-16 — Index strategy
**Statement.** Every index earns its write cost; no index exists without a query that requires it.
**Target.** Write amplification from indexes ≤ 0.65× the heap byte cost on hot tables (§14.2 shows 339 B of index per 520 B of heap on `payments`); zero unused indexes in production for > 30 days; every production query plan uses an index (no sequential scan on a partition > 100 MB).
**Measured by.** `pg_stat_user_indexes.idx_scan` review monthly; a CI test asserting plans for the catalogued query set.
**On violation.** An index unused for 30 days is dropped by a scheduled change. A sequential scan on a large partition blocks the release.
**Mechanism.** ULID primary keys give time-ordered B-tree insert locality (baseline §6) — UUIDv4 would fragment every index and roughly double write amplification. Partial indexes for sparse predicates (`WHERE outcome='SUCCESS'`, `WHERE state NOT IN (terminal…)`). Covering indexes only where a measured index-only scan justifies the extra bytes.

### NFR-17 — Data growth budget
**Statement.** Storage growth is predicted, not discovered.
**Target.** P1: 8.8 KB retained relational bytes per payment → **32.1 TB/year** (§14.2). Hot (13-month) 34.8 TB in Aurora; cold 6 years → 192.6 TB raw → **29.6 TB in S3** after Parquet+zstd (6.5:1). Actual must stay within ±15 % of the model.
**Measured by.** Monthly actual bytes/payment recomputed from `pg_total_relation_size` divided by the period's payment count.
**On violation.** Deviation > 15 % triggers a schema review; the usual cause is unbounded `metadata` jsonb, which is why it is capped.
**Mechanism.** Per-payment byte budget enforced by a schema-review checklist; `metadata` capped at 4 KB per payment and 50 keys at L1; response snapshots in `idempotency_records` capped at 16 KB; hot/cold tiering.

### NFR-18 — Event-stream growth budget
**Statement.** Kafka retention and throughput are sized from the event-per-payment fan-out, not guessed.
**Target.** P1: 7.15 events/payment, 327 B compressed on the wire → **0.27 MB/s average, 1.17 MB/s at diurnal peak**. P2: **11.69 MB/s producer, ≈ 93.6 MB/s aggregate broker traffic** (§14.3). `pp.payments.payment.v1` at 30-day retention, RF=3: P1 **1.22 TB**, P2 **12.3 TB**.
**Measured by.** MSK broker metrics (`BytesInPerSec`, `BytesOutPerSec`), topic size.
**On violation.** Topic size above 70 % of broker storage triggers tiered-storage enablement or a retention review, never a silent truncation.
**Mechanism.** zstd compression (~3.6:1 on the JSON envelope); 48 partitions on the payment topic sized for consumer parallelism (§14.3), not for byte throughput; `pp.audit.v1` uses tiered storage to S3 after 7 days because 400-day broker-local retention would need 11.8 TB.

---

## 4. Availability, SLIs, SLOs and error budgets

### NFR-19 — Data-plane availability
**Statement.** The money path meets its availability target measured as a successful-request ratio, not as uptime.
**Target.** 99.99 % monthly → ≤ 4 m 23 s of budget per 30-day window (baseline §18).
**Measured by.** SLI = (total requests − requests returning 5xx or exceeding the latency threshold) ÷ total requests, over a rolling 30-day window, excluding requests rejected as `4xx` client errors other than `429`.
**On violation.** Burn-rate alerts: 14.4× over 1 h pages; 6× over 6 h tickets (baseline §22.4).
**Mechanism.** Multi-AZ everything; PDBs and anti-affinity; readiness gates that shed traffic before failing it; graceful degradation paths (NFR-22).

### NFR-20 — Control-plane availability
**Statement.** Control-plane unavailability must not become data-plane unavailability.
**Target.** 99.9 % monthly for the control plane; **zero** data-plane availability impact from a full control-plane outage of up to `max_config_staleness` (15 min).
**Measured by.** Same SLI form; plus a chaos test that kills the control plane entirely and asserts the payment SLO holds.
**On violation.** Ticket for the control plane; a data-plane impact is a design defect and pages.
**Mechanism.** Fail-static configuration with bounded staleness and a defined cliff (baseline §15, FR-48). The data plane never makes a synchronous call to the control plane on the payment path.

### NFR-21 — Defined SLI set
**Statement.** Five SLIs, and only five, define "working". Everything else is a diagnostic.
**Target.** Payment API availability 99.99 %; payment API latency (p99 ≤ 250 ms) in 99 % of 5-min windows; authorization success rate ≥ merchant baseline − 5 pp; webhook processing lag p99 ≤ 60 s; config propagation p99 ≤ 30 s (baseline §22.4).
**Measured by.** Recording rules producing one time series per SLI per region.
**On violation.** Per baseline §22.4 fast/slow burn thresholds; the authorization-rate SLI pages on a 30-minute drop, because a silent auth-rate drop is revenue loss that no 5xx counter shows.
**Mechanism.** A small SLI set that is reviewed at every incident; new SLIs require deleting one, to prevent alert-set inflation.

### NFR-22 — Graceful degradation with defined cliffs
**Statement.** Every dependency failure has a specified degraded mode and a specified point at which degradation stops and refusal begins.
**Target.** Redis loss → idempotency falls back to PostgreSQL, rate limiting to a local token bucket; latency +≤ 15 ms p99, correctness unchanged. Kafka loss → outbox retains, no data loss, consistency window widens. Control plane loss → fail-static until 15 min, then fail closed for merchants with no cached snapshot. All gateways unhealthy → `503 NO_ELIGIBLE_GATEWAY`, fail closed.
**Measured by.** Chaos suite `tests/chaos` with one scenario per row of baseline §24.
**On violation.** A dependency failure with an undefined mode is a release blocker.
**Mechanism.** Baseline §24's failure-mode catalog is implemented as tests, not as prose. **Fail-static, not fail-open**: processing with no limits is a compliance breach; processing with guessed limits is worse.

### NFR-23 — Error-budget policy
**Statement.** The error budget governs release behaviour automatically, not by negotiation.
**Target.** Burn > 2× the sustainable rate → feature freeze, reliability work only. Burn > 10× within 1 h → incident declared and the most recent change rolled back (baseline §18).
**Measured by.** Burn-rate recording rules; CI queries the burn rate and refuses to promote to production while frozen.
**On violation.** A manual override requires a named accountable engineer and is itself audited.
**Mechanism.** Automated gate in the deployment pipeline (baseline §18); budget consumption is a dashboard on the team's wall, not a quarterly report.

---

## 5. Durability, RPO and RTO

### NFR-24 — Durability of committed money state
**Statement.** A committed payment state change is never lost.
**Target.** Aurora 6-way replication across 3 AZs; S3 11 nines (99.999999999 %) for archives and evidence artifacts; zero acknowledged-then-lost writes across the DR drill suite.
**Measured by.** Provider SLA plus a quarterly DR drill that kills the writer mid-transaction and reconciles acknowledged responses against persisted state.
**On violation.** Any acknowledged-then-lost write is a Sev-1 with a mandatory public-to-customer postmortem.
**Mechanism.** Synchronous quorum commit in-region; no `synchronous_commit=off` anywhere on the money path; the outbox makes state and event atomic so an event can never survive a rolled-back state (baseline §13.4).

### NFR-25 — RPO
**Statement.** Data loss on regional failover is bounded and known.
**Target.** In-region: **RPO 0** (synchronous commit). Cross-region: Aurora Global Database typical ≤ 1 s, **budgeted ≤ 5 s** (baseline §18).
**Measured by.** `AuroraGlobalDBReplicationLag` continuously; measured actual lag at the moment of promotion in each DR drill.
**On violation.** Sustained replication lag > 5 s pages; the platform continues serving (lag is not an outage) but the DR posture is degraded and is treated as such.
**Mechanism.** Aurora Global Database with a single regional writer; active/passive money processing (baseline A9). Active/active was rejected — see §15.

### NFR-26 — RTO
**Statement.** Recovery time is rehearsed, not estimated.
**Target.** AZ failover ≤ 60 s; region failover ≤ 15 min (baseline §18). Rehearsed quarterly; the measured drill time is the number of record, not the theoretical one.
**Measured by.** `scripts/dr-drill.sh` produces a timed report: detection, decision, promotion, DNS propagation, readiness, first successful payment.
**On violation.** A drill exceeding the RTO blocks the next feature release until the gap is closed.
**Mechanism.** Route 53 health-check failover; Aurora Global secondary promotion; pre-warmed standby capacity in the passive region at 30 % of active, scaled by HPA after promotion; `platformctl dr promote` as a single audited operation.

### NFR-27 — Backup and restore
**Statement.** Backups are worthless until a restore has been performed.
**Target.** Continuous PITR with a 35-day window; a full restore-to-new-cluster rehearsal monthly, completing in ≤ 4 h for the P1 hot volume; archive restore from Glacier IR in ≤ 12 h.
**Measured by.** Monthly restore rehearsal report with a data-integrity check (row counts plus ledger balance verification, FR-81) against the restored copy.
**On violation.** A failed or unrehearsed restore is a Sev-2; the backup is presumed non-functional until proven otherwise.
**Mechanism.** Aurora PITR; S3 versioning with Object Lock on audit and evidence buckets; cross-region backup copies for the residency-permitted subset.

---

## 6. Security

### NFR-28 — Zero Trust service-to-service
**Statement.** No service trusts another because of its network position.
**Target.** 100 % of intra-cluster traffic is mTLS with workload identity; 0 network paths that authenticate by source IP or namespace alone; default-deny NetworkPolicy on every namespace.
**Measured by.** Service-mesh policy audit in CI; a chaos test asserting that an unauthenticated pod in the same namespace cannot call `payment-orchestrator`.
**On violation.** Release blocked; an authenticated-by-position path is a critical finding.
**Mechanism.** Service mesh with SPIFFE-style workload identity; IRSA per service account, one IAM role per deployable (baseline §17.2); default-deny egress with an explicit allowlist per service.

### NFR-29 — Defence in depth for tenant isolation
**Statement.** Cross-tenant data access requires the simultaneous failure of three independent controls.
**Target.** Three enforced layers: (1) application tenant guard from the token only; (2) PostgreSQL Row-Level Security with the app connecting as a non-`BYPASSRLS` role; (3) an integration test asserting zero rows at the database level for a cross-tenant query. Zero cross-tenant reads in any test or production audit.
**Measured by.** `TestCrossTenantAccessIsImpossible` in CI; a continuous production canary issuing a deliberate cross-tenant query and asserting zero rows.
**On violation.** Any cross-tenant read is a Sev-1 security incident with regulatory notification assessment.
**Mechanism.** Baseline §16.2. `ErrMissingTenantContext` on a repository call with no tenant in context — the query is not issued at all, so a forgotten `WHERE` clause is a compile-time-shaped failure rather than a data leak.

### NFR-30 — Cryptography standards
**Statement.** Only vetted algorithms and key sizes, with no bespoke cryptography.
**Target.** TLS 1.3 externally (1.2 permitted only for gateway partners that require it, recorded as an exception with an expiry); AES-256-GCM at rest; envelope encryption for credential material; HMAC-SHA256 for gateway idempotency keys and webhook signatures; Argon2id for API client secret hashes; SHA-256 for request fingerprints and the audit hash chain; ULID entropy from a CSPRNG.
**Measured by.** TLS scanner in CI and against production endpoints; a lint forbidding `crypto/md5`, `crypto/sha1`, `math/rand` in non-test code.
**On violation.** Release blocked; a downgrade exception requires a named expiry date and a compliance sign-off.
**Mechanism.** Centralised in `internal/infrastructure/crypto`; no algorithm choice is made at a call site.

### NFR-31 — Key and credential rotation
**Statement.** Every long-lived secret has an automated rotation with a defined maximum age.
**Target.** KMS CMKs: annual automatic. Gateway API credentials: ≤ 90 days, automated, with dual-run overlap (FR-38). JWT signing keys: 30 days with a 2-key JWKS window. API client secrets: 365 days maximum, rotation with overlap (FR-08). Webhook signing secrets: 180 days. Zero credentials above their maximum age.
**Measured by.** `pp_credential_age_seconds` gauge per credential class; a compliance report listing any credential above its maximum.
**On violation.** A credential above its maximum age raises a compliance finding; above 1.5× the maximum, the connection is marked degraded and traffic is routed away.
**Mechanism.** Rotation as a durable workflow with compensation (FR-38, FR-39), so a rotation crash cannot leave a merchant with no valid credential.

### NFR-32 — Secrets never leave the secrets boundary
**Statement.** Credential material cannot be logged, serialised, returned by an API, or captured in a trace or a core dump path.
**Target.** Zero occurrences in logs, traces, metrics, error bodies or API responses, asserted continuously.
**Measured by.** A contract test walking the entire OpenAPI surface asserting no schema exposes credential material; a log-scanner canary; a linter forbidding `%+v` and `%#v` on request and credential types.
**On violation.** Release blocked. A leak in production is a Sev-1 with immediate rotation of every affected credential.
**Mechanism.** The `Secret[T]` wrapper whose `String()`, `MarshalJSON()` and `Format()` return `[REDACTED]`; structured logging with a field **allowlist** — an unregistered field is dropped rather than serialised (baseline §17.2).

### NFR-33 — Sensitive-data ingress prevention
**Statement.** Cardholder data cannot enter the platform, by construction and by detection.
**Target.** 100 % of string fields on every request are scanned by the PAN detector (13–19 digits, Luhn-valid after separator stripping); a detection returns `400 SENSITIVE_DATA_IN_REQUEST`, does not log the value, and raises a security event. Detector false-negative rate ≤ 0.1 % against the test corpus; false-positive rate ≤ 0.5 % (measured against a corpus of legitimate long numeric identifiers).
**Measured by.** Detector unit tests against a labelled corpus; production counter of detections.
**On violation.** A false negative that lets a PAN through is a PCI scope event, not merely a bug: the receiving service and its logs enter CDE scope until proven clean.
**Mechanism.** L1 middleware (baseline §12 stage 7, §17.2). Design intent is SAQ-A/A-EP: cardholder data neither traverses nor is stored by the platform.

### NFR-34 — Least privilege and separation of duties
**Statement.** No principal can both perform a money-moving action and erase its evidence.
**Target.** Zero principals holding both `payments:refund` (or equivalent) and write access to `audit_records`; zero human principals with production database write access outside a break-glass procedure; break-glass sessions are time-boxed to 60 minutes, recorded, and reviewed within 24 h.
**Measured by.** Quarterly IAM and role-binding audit; break-glass session log review.
**On violation.** An unreviewed break-glass session is a compliance finding; a principal holding a forbidden combination is remediated immediately.
**Mechanism.** Audit is write-only from the application's perspective (baseline §3, BC-9); IRSA per deployable; secret paths scoped by prefix condition; four-eyes on the compliance gate (FR-29).

### NFR-35 — Supply chain and vulnerability management
**Statement.** What is built is what is deployed, and what is deployed is known.
**Target.** SBOM generated per build; images signed and verified at admission; zero `CRITICAL` and zero `HIGH` CVEs with a fix available in a production image older than 7 days; base images rebuilt weekly; dependencies pinned by digest.
**Measured by.** SAST, dependency scan and container scan in CI; an admission controller rejecting unsigned images; a production image-age dashboard.
**On violation.** A `CRITICAL` with a fix blocks the next release and triggers an out-of-band patch release within 48 h.
**Mechanism.** Distroless, non-root, read-only-root-filesystem images; `gateway-simulator` is `//go:build` guarded out of production images (baseline §5) — a test double reachable in production is a payment-forgery vector.

### NFR-36 — Rate limiting and abuse resistance
**Statement.** No tenant can consume another tenant's capacity, and no retry storm can take the platform down.
**Target.** Per-tenant and per-merchant token buckets with configurable rates; a global adaptive concurrency limiter; under a 20× synthetic retry storm from one tenant, other tenants' p99 degrades ≤ 20 ms and the platform stays inside its SLO.
**Measured by.** `tests/chaos/retry_storm_test.go::TestAdaptiveLimiterShedsRatherThanQueues`; `pp_http_requests_total{status="429"}` by tenant.
**On violation.** Release blocked; in production, sustained `429` on one tenant opens a capacity conversation rather than a silent throttle.
**Mechanism.** Redis token buckets with a local fallback; per-tenant concurrency bulkheads; `Retry-After` and `RateLimit-*` headers so well-behaved clients back off correctly (baseline §19.3); FR-55's fail-fast on concurrent duplicates, which is what stops a retry storm from exhausting the request pool.

---

## 7. Privacy, residency and compliance

### NFR-37 — Data residency
**Statement.** Personal data stays in the tenant's declared region, and the constraint is enforced in the routing path, not merely documented.
**Target.** Zero personal-data records outside the declared region; zero routing plans containing a gateway whose processing region violates the policy; a per-tenant residency report producible on demand.
**Measured by.** A scheduled scan of personal-data stores by region; every routing plan records its residency exclusions (FR-62), which makes the evidence queryable.
**On violation.** A residency violation is a Sev-1 with a regulatory notification assessment.
**Mechanism.** Residency as a routing eligibility filter (FR-07); region-scoped buckets and KMS keys; the siloed tier gets a dedicated CMK (baseline §16.1).

### NFR-38 — GDPR — lawful basis, minimisation and erasure
**Statement.** Personal data is minimised, purpose-bound, and erasable without destroying records held under a legal obligation.
**Target.** Zero PII in logs, traces or metrics. Right-to-erasure fulfilled within 30 days via crypto-shredding. A documented lawful basis per personal-data field, reviewed annually. Data-subject access requests answerable within 30 days.
**Measured by.** A log-scanner canary asserting no PII fields; erasure request tracking with SLA; the field-level data inventory in `docs/` reviewed in CI against the schema (a new personal-data column without an inventory entry fails the build).
**On violation.** PII found in a log is a Sev-2 and the log stream is purged; a missed erasure SLA is a compliance finding.
**Mechanism.** Crypto-shredding of the per-purpose data key (FR-15) resolves the conflict between GDPR Art. 17 and the 7-year financial-records obligation; hard deletion was rejected — see §15.

### NFR-39 — PCI DSS scope containment
**Statement.** The platform is assessed at SAQ-A/A-EP because cardholder data neither traverses nor is stored by it, and that claim is continuously defended.
**Target.** Zero PAN, CVV or track data in any store, log, trace, backup or event, at any time. The card-vault capability, if a tenant requires it, lives in a separate AWS account, VPC, cluster, HSM/KMS and change-control regime, and outside this repository (baseline §17.1).
**Measured by.** Annual assessment; continuous PAN-detector counters; a quarterly scan of backups and archives for Luhn-valid sequences.
**On violation.** Any confirmed PAN in scope triggers the CDE-expansion incident procedure and a re-assessment.
**Mechanism.** Baseline §17. Vaulting PAN ourselves was rejected — see §15.

### NFR-40 — PSD2 / SCA compliance
**Statement.** Strong customer authentication is applied where required, exemptions are claimed only where justified, and every decision is evidenced.
**Target.** 100 % of in-scope EEA transactions either carry SCA or record a specific exemption with its basis and inputs. Exemption claim rate and the resulting fraud rate are monitored against the TRA thresholds. Zero SCA decisions without a recorded rationale.
**Measured by.** Per-corridor SCA and exemption dashboards; fraud rate per exemption band; an audit query producing the decision record for any payment.
**On violation.** A fraud rate approaching a TRA threshold automatically tightens the exemption policy toward the merchant's configured default.
**Mechanism.** 3DS as a policy outcome of the risk engine, not a client flag (FR-61, baseline §17.3); liability-shift evidence (ECI/CAVV) stored with the payment for dispute defence.

### NFR-41 — AML/KYC evidence retention
**Statement.** Verification decisions and their evidence are immutable and retained for the regulatory period.
**Target.** KYC decisions and evidence retained ≥ 5 years in object storage with Object Lock in compliance mode; zero deletions within the retention period, including by an account administrator.
**Measured by.** Object Lock configuration audit; a deliberate deletion attempt in the DR drill asserting refusal.
**On violation.** A retention gap is a compliance finding requiring regulator notification assessment.
**Mechanism.** Baseline §17.3; Object Lock in compliance (not governance) mode so that even root cannot shorten retention.

### NFR-42 — Records retention and legal hold
**Statement.** Every data class has a stated retention, an enforcement mechanism, and a legal-hold override.
**Target.** Payments and ledger 7 years; audit 7 years WORM; idempotency 7 days then archived with the audit trail; logs 30 days hot / 400 days archive; PII in logs: none (baseline §17.3). A legal hold suspends deletion for the named subject within 1 h of being placed.
**Measured by.** Retention job reports; a legal-hold register reconciled against the deletion jobs monthly.
**On violation.** Deleting data under legal hold is a Sev-1; retaining beyond the stated period without a hold is a GDPR finding.
**Mechanism.** Partition detachment for relational retention (NFR-15); S3 lifecycle policies with Object Lock; a hold flag consulted by every deletion job, and the job fails closed if the hold register is unreachable.

---

## 8. Observability

### NFR-43 — Mandatory telemetry context
**Statement.** Every log line, span and exemplar carries the context needed to correlate it without a join.
**Target.** 100 % of records carry `trace_id`, `span_id`, `correlation_id`, `tenant_id`, `merchant_id`, `payment_id`, `gateway_id`, `service`, `version`, `environment`, `region` where applicable (baseline §22.1).
**Measured by.** A CI test asserting the logger and tracer middleware populate the full set; a production sampler checking field presence.
**On violation.** Release blocked. A log line without correlation context is unusable at 3 a.m., which is the only time it matters.
**Mechanism.** Context propagation through `context.Context`; W3C `traceparent` in and out on every hop (baseline §12 stage 2).

### NFR-44 — Metric cardinality control
**Statement.** Observability cannot become the platform's largest cost or its largest outage.
**Target.** `merchant_id` and `payment_id` are **never** metric labels. ≤ 10⁴ series per metric per service. Total active series budget per region with an alert at 80 %.
**Measured by.** A CI cardinality lint over the metric registry; a production series-count dashboard.
**On violation.** CI fails on a high-cardinality label. In production, breaching the budget triggers automatic relabel-drop of the offending series and a ticket.
**Mechanism.** Baseline §22.3: high-cardinality identity lives in logs, traces and exemplars; metrics carry `tenant_tier`, not `tenant_id`, and exemplars carry the identity for drill-down.

### NFR-45 — RED plus business metrics coverage
**Statement.** Every service exposes rate/errors/duration, and the business outcomes the platform exists to produce are metrics, not reports.
**Target.** 100 % of the metric set in baseline §22.2 is emitted. Authorization rate, routing decisions, idempotency outcomes, workflow state, outbox backlog, consumer lag, config snapshot age, reconciliation exceptions and DLQ depth are all live.
**Measured by.** A CI test asserting every catalogued metric is registered and emitted at least once under the integration suite.
**On violation.** A missing business metric blocks the release — a silent authorization-rate drop is invisible to RED metrics alone, and it is the failure that costs the most money.
**Mechanism.** A central metric registry; the catalog is code, and the test reads the catalog.

### NFR-46 — Trace coverage and sampling
**Statement.** Enough traces exist to diagnose a tail-latency problem without capturing so many that storage becomes the constraint.
**Target.** Head sampling 1 % of successful payments; **100 %** of errors, of requests exceeding the p99 threshold, and of any request touching a `TIMEOUT_UNKNOWN` attempt. Trace retention 14 days. Every trace spans ingress → orchestrator → adapter → gateway with propagated context.
**Measured by.** Sampling-rate metrics; a test asserting an error path is always sampled.
**On violation.** An unsampled error trace is a diagnostic gap; the sampler configuration is treated as production code.
**Mechanism.** Tail-based sampling for error and latency outliers; head sampling for the baseline.

---

## 9. Operability

### NFR-47 — Runbook coverage
**Statement.** Every alert that pages a human has a runbook, and every failure mode in baseline §24 has an owner.
**Target.** 100 % of paging alerts link to a runbook containing: what fired, what it means, what to check first, what to do, and what "resolved" looks like. 100 % of reconciliation exception types have a documented remediation.
**Measured by.** A CI check asserting every alert rule has a `runbook_url` annotation resolving to an existing document.
**On violation.** An alert without a runbook is disabled by the CI check rather than left to page someone at 3 a.m. with no guidance.
**Mechanism.** Runbooks in `docs/runbooks/`, versioned with the code that emits the alert, reviewed after every incident that used them.

### NFR-48 — Deployment cadence and safety
**Statement.** Deployments are frequent, small, and reversible, because large infrequent deployments are the most reliable way to cause an incident.
**Target.** ≥ 5 production deployments per week per service; change failure rate ≤ 15 %; lead time from merge to production ≤ 4 h; every deployment is progressive (canary at 5 % for ≥ 10 min with automated SLI comparison before promotion).
**Measured by.** DORA metrics from the deployment pipeline; canary analysis reports.
**On violation.** A canary whose error rate or p99 diverges beyond threshold auto-rolls-back without human involvement.
**Mechanism.** Progressive delivery with automated analysis; forward-only migrations, each with a tested down script (baseline §25); feature flags for behavioural changes so a rollback of behaviour does not require a rollback of code.

### NFR-49 — Rollback time
**Statement.** Getting back to a known-good state is fast enough to be the first response, not the last resort.
**Target.** Application rollback ≤ 5 min from decision to full fleet. Configuration rollback ≤ 30 s to publish plus ≤ 30 s to propagate (FR-46, NFR-21). Database migrations are expand/contract so that no rollback requires a schema reversal under pressure.
**Measured by.** Rollback drills; the timestamp delta in the deployment pipeline.
**On violation.** A rollback exceeding 5 min is reviewed as an incident in its own right.
**Mechanism.** Immutable image tags; previous ReplicaSet retained; expand/contract migrations mean application version N and N+1 are simultaneously schema-compatible, which is what makes a rollback safe at all.

### NFR-50 — Toil budget
**Statement.** Repetitive manual operational work is capped and actively reduced.
**Target.** ≤ 5 % of SRE capacity on toil; zero manual steps in merchant onboarding beyond the compliance gate; zero database edits as a remediation (every operator action exists as an audited API or `platformctl` command).
**Measured by.** A toil ledger reviewed monthly; a count of production database write sessions by human principals (target: zero outside break-glass).
**On violation.** Exceeding the toil budget converts the excess into automation work in the next cycle.
**Mechanism.** `platformctl` as the single operator surface (migrations, config validation, certification runs, reconciliation, DR drills); every remediation in a runbook must be a command, not a query.

### NFR-51 — Capacity planning cadence
**Statement.** Capacity is planned quarterly against a model that is reconciled against actuals, not against intuition.
**Target.** A quarterly review comparing modelled bytes/payment, events/payment, CPU-ms/payment and connections against measured actuals; each within ±15 % of the model or the model is corrected. Provisioned headroom ≥ 3× the current peak (baseline §24, AZ-loss row).
**Measured by.** The capacity review document; the derived-metrics dashboard (§14).
**On violation.** Headroom below 2× triggers an immediate scale-out; a model deviation > 15 % triggers a model revision before the next planning cycle.
**Mechanism.** §14's arithmetic is implemented as recording rules so the model's inputs are continuously measured rather than re-derived by hand.

---

## 10. Maintainability

### NFR-52 — Test coverage gates
**Statement.** Coverage thresholds differ by layer because the layers carry different risk.
**Target.** `internal/domain` ≥ 90 % line and ≥ 85 % branch; `internal/application` ≥ 80 %; `internal/validation` ≥ 90 %; adapters ≥ 70 % plus a passing contract suite per gateway; repository-wide ≥ 75 %. Mutation score ≥ 60 % on `internal/domain` (the FSMs and invariants) — coverage without mutation testing measures execution, not assertion.
**Measured by.** Coverage report and mutation run in CI, gated per package.
**On violation.** Merge blocked. A coverage drop is treated as a change to the requirement, not as an accident.
**Mechanism.** Domain has no infrastructure dependencies (baseline §4), so 90 % is cheap there and expensive nowhere.

### NFR-53 — Architecture fitness functions
**Statement.** Architectural rules are enforced by the build, not by review diligence.
**Target.** All of the following pass on every commit: the dependency rule (`scripts/check-architecture.sh`, baseline §4 table); `pkg/**` imports stdlib only; no `float64` in any money path; no `%+v`/`%#v` on request or credential types; every validation rule ID documented (`TestEveryRuleIsDocumented`); metric cardinality lint; OpenAPI contract matches handler behaviour; every requirement has ≥ 1 test and every test maps to ≥ 1 requirement (baseline §26); `TestCrossTenantAccessIsImpossible`; no gateway type appears in `internal/domain`.
**Measured by.** CI job `verify`, red or green.
**On violation.** Merge blocked. There is no advisory mode — an advisory architecture rule is not a rule.
**Mechanism.** Baseline §4, §21, §26. Enforcement in CI is the difference between an architecture and an aspiration.

### NFR-54 — Contract stability and versioning
**Statement.** A published contract does not break its consumers.
**Target.** Events are additive-only within a major version; a breaking change is a new `.v2` type published alongside `.v1` until every consumer has migrated (baseline §13.1). REST is URI-major-versioned and additive-only within a major; deprecation uses `Sunset` and `Deprecation` headers with ≥ 180 days notice. Zero unannounced breaking changes.
**Measured by.** A schema-compatibility check in CI against the published registry (backward-compatible mode); a consumer-driven contract suite.
**On violation.** Merge blocked. In production, an unannounced break is a Sev-2 with customer notification.
**Mechanism.** Published Language pattern (baseline §3); the event registry and OpenAPI documents are the contract, and they are versioned artifacts.

### NFR-55 — Code health and dependency hygiene
**Statement.** The codebase stays modifiable.
**Target.** Cyclomatic complexity ≤ 15 per function (≤ 25 for FSM transition tables, which are inherently branchy); no file > 800 lines; no package cycle; direct dependencies ≤ 60 and each justified in an ADR or the dependency register; `go vet`, race detector and lint clean.
**Measured by.** Lint and complexity gates in CI; a quarterly dependency review.
**On violation.** Merge blocked on the mechanical gates; the dependency review produces removal tasks.
**Mechanism.** Baseline §4's layering keeps dependencies at the edges; `pkg/**` being stdlib-only means the extractable core carries no transitive burden.

---

## 11. Portability

### NFR-56 — 12-factor conformance
**Statement.** The application is portable across environments by configuration alone.
**Target.** All twelve factors hold, specifically: strict separation of config from code (config from environment only, zero environment-specific code paths); backing services as attached resources behind ports; stateless processes with no sticky sessions and no local disk state beyond ephemeral scratch; port binding with no assumed reverse proxy; disposability with graceful shutdown draining in-flight requests within 30 s; dev/prod parity with the same container image across all environments, differing only by config; logs to stdout as structured JSON with no application-managed log files; admin tasks as one-off processes (`platformctl`), never as HTTP endpoints.
**Measured by.** A CI test starting each binary with only environment configuration; a container-image digest comparison across environments asserting they are identical.
**On violation.** An environment-specific code path is a release blocker; it is the mechanism by which staging stops predicting production.
**Mechanism.** Composition roots in `cmd/**` wire adapters from config; every external dependency sits behind an application-owned port (baseline §4), which is what makes PostgreSQL, Kafka, Redis and each gateway substitutable.

### NFR-57 — Substitutability of infrastructure and vendors
**Statement.** No single vendor choice is unremovable within a bounded effort.
**Target.** The workflow engine (in-house PostgreSQL engine ↔ Temporal), the KYC vendor, the bank-validation vendor, the risk scorer and every payment gateway are each replaceable by implementing a port, with zero changes to `internal/domain` and `internal/application`. Adding a new gateway is data (a capability descriptor) plus an adapter plus a passing contract suite — no routing-logic change.
**Measured by.** The gateway-simulator adapter and the Temporal adapter both exist and pass the same contract suites as the production adapters — this is the proof, not the claim.
**On violation.** A vendor concept leaking into `internal/domain` fails the architecture check (NFR-53).
**Mechanism.** Anti-corruption layer per external system (baseline §3); the SPI plus contract suite in `internal/adapters/gateway`; capability descriptors as the single source of gateway truth (FR-33).

---

## 12. Cost

### NFR-58 — Unit infrastructure cost per 1 000 payments
**Statement.** Unit cost is a tracked engineering metric with a target curve, because gross margin is a design constraint (business `CC-2`).
**Target.** Volume-banded (§14.6): ≤ $1.20 / 1 000 at 10 M payments/month; ≤ $0.28 at 50 M; ≤ **$0.15 at 100 M**; ≤ $0.07 at 300 M; ≤ $0.05 at 1 B. Modelled P1 actual at 300 M/month: **$0.053 / 1 000**.
**Measured by.** Monthly cost allocation by tag (`service`, `plane`, `tenant_tier`) divided by billable payments from the meter projection (FR-87).
**On violation.** Exceeding a band by > 20 % for two consecutive months opens a cost-reduction task with a named owner; the largest line is examined first (currently Aurora I/O-Optimized storage at 49 % of the P1 bill).
**Mechanism.** Hot/cold tiering (a 7-year hot window would cost $46 980/month in Aurora storage against $7 830 for 13 months — §14.6); pooled tenancy as the default; zstd on the event stream; per-pointer rather than per-body Redis mirroring (§14.5); reserved capacity for the fixed floor.

### NFR-59 — Cost attribution and tenant-level unit economics
**Statement.** Cost is attributable to a tenant tier so that pricing decisions use data.
**Target.** ≥ 95 % of the infrastructure bill is tagged to a service and a plane; siloed-tier tenants' dedicated resources are attributable at 100 %; a per-tenant-tier cost-per-1 000-payments figure is produced monthly.
**Measured by.** Cost allocation report; untagged spend as a percentage.
**On violation.** Untagged spend above 5 % blocks the monthly finance close for infrastructure.
**Mechanism.** Mandatory tags enforced by Terraform policy; siloed resources are physically separate (baseline §16.1), which makes their cost directly measurable rather than modelled.

---

## 13. Accessibility of administrative surfaces

### NFR-60 — WCAG 2.1 AA for admin and operator surfaces
**Statement.** The operator console, tenant admin and merchant admin surfaces are usable by people using assistive technology, including during an incident.
**Target.** WCAG 2.1 Level AA: contrast ≥ 4.5:1 for body text and ≥ 3:1 for large text and UI components; full keyboard operability with a visible focus indicator and no keyboard traps; every form control programmatically labelled; every error identified in text and associated with its control (never colour alone); status changes announced via ARIA live regions; target size ≥ 24×24 CSS px; content reflows at 320 px width without horizontal scrolling; respects `prefers-reduced-motion`.
**Measured by.** Automated axe-core scan in CI on every page (zero violations at `serious` or `critical`); a manual keyboard and screen-reader pass per release on the critical journeys (approve compliance gate, suspend merchant, roll back configuration, override gateway health).
**On violation.** A `serious`/`critical` automated violation blocks the release; a manual finding is a bug at the same priority as a functional defect.
**Mechanism.** A design system with accessible primitives; no custom control without a documented ARIA pattern; incident-critical actions reachable by keyboard alone, because an operator working one-handed at 3 a.m. is a real user.

### NFR-61 — API ergonomics as an accessibility concern
**Statement.** Errors are actionable by a machine and by a human, so integration does not depend on tribal knowledge.
**Target.** 100 % of error responses carry a stable `code`, a machine-readable `retryable` flag, a `requestId`, a `traceId`, a `docsUrl`, and field-level `details[]` where applicable (baseline §20). Every reserved code has a documentation page. Validation failures report **every** failing field in one response, not the first.
**Measured by.** A contract test asserting the error shape on every documented error path; a link-checker over `docsUrl`.
**On violation.** Merge blocked. A first-failure-only validation response is what turns a five-second integration fix into a five-day loop, which is the cost the onboarding product exists to remove.
**Mechanism.** RFC 9457 `application/problem+json` with platform extensions; the error catalog in `api/errors/catalog.yaml` is the single source and is code-generated into both the server and the SDKs.

---

## 14. Capacity arithmetic

Every number in §§1–13 that depends on scale is derived here. The inputs are stated so the outputs
can be recomputed when the inputs change; the derived quantities are also emitted as recording
rules so the model is continuously reconciled against reality (NFR-51).

### 14.1 Latency budget

Summing the per-stage budgets of baseline §12, excluding the gateway call:

| Stage group | Stages | Budget (ms) |
|---|---|---|
| Pre-dispatch | 2 request ID (1) + 3 authn (2) + 4 tenant (1) + 5 authz (2) + 6 rate limit (2) + 7 L1 (3) + 8 idempotency (8) + 9 merchant context (5) + 10 L5 (5) + 11 risk (15) + 12 routing (5) + 13 attempt+dispatch (10) | **59** |
| Post-dispatch | 15 L6 (3) + 16 L7+outbox (10) + 17 idempotency completion (5) | **18** |
| **In-app total** | | **77** |

SLO p99 ≤ 250 ms excluding gateway → **173 ms of slack**, allocated:

| Slack consumer | ms |
|---|---|
| Admission queueing at 60 % utilisation | 60 |
| Go GC + scheduler tail | 40 |
| Intra-cluster network, 3 hops × 10 ms p99 | 30 |
| Unallocated reserve | 43 |

End-to-end p99 ≤ 1.5 s including gateway → gateway budget = 1 500 − 250 = **1 250 ms p99**. The
adapter's 8 s hard timeout is a correctness ceiling (it bounds how long a payment can be
ambiguous), not a latency budget.

**Failover arithmetic.** A failover adds one full gateway call. If attempt 1 consumes the whole
1 250 ms gateway budget, a second attempt cannot fit inside 1.5 s. Hence the rule in NFR-03:
failover is attempted only while elapsed wall time < 700 ms, leaving ≥ 800 ms for the second
attempt. Beyond that cut-off the platform returns the truthful outcome rather than blowing the SLO
to chase an uplift — the uplift is worth 1.5 pp of authorization rate, not a 3 s p99.

### 14.2 Storage per payment, and storage per year

**Row-size derivation** (PostgreSQL, 8 KB pages, ULIDs stored as `char(30)` → 31 B with the varlena
header, `tid` 8 B, B-tree entry overhead ~4 B, index fill factor 0.7).

| Table | Rows per payment | Heap B/row | Index B/row | Total B/payment |
|---|---:|---:|---:|---:|
| `payments` | 1.00 | 520 | 339 | **859** |
| `payment_attempts` | 1.15 | 320 | 294 | **706** |
| `routing_plans` | 1.00 | 618 | 151 | **769** |
| `payment_events` | 4.20 | 402 | 161 | **2 365** |
| `ledger_entries` | 4.20 | 184 | 251 | **1 827** |
| `audit_records` | 3.00 | 597 | 161 | **2 274** |
| **Retained total** | | | | **8 800 B ≈ 8.6 KiB** |
| `idempotency_records` (7-day retention only) | 1.00 | 1 836 | 300 | 2 136 |
| `outbox_events` (pruned at 24 h) | 4.20 | 560 | 140 | 2 940 |

`payments` index breakdown (the 339 B): PK on `payment_id` 61 B; `(tenant_id, merchant_id,
created_at DESC, payment_id)` 161 B; partial `(merchant_id, state)` on non-terminal states, ~5 % of
rows → 3 B amortised; `(gateway_ref)` for webhook resolution 114 B.

**Annual growth.**

```
P1: 10,000,000 payments/day × 365 = 3.65e9 payments/year
    3.65e9 × 8,800 B = 3.212e13 B = 32.1 TB/year retained relational

P2: 100,500,000/day × 365 = 3.67e10 payments/year
    3.67e10 × 8,800 B = 3.23e14 B = 323 TB/year   → exceeds one Aurora cluster
                                                    → mandates tenant sharding (NFR-14)
```

**Hot/cold split (P1).**

```
Hot window 13 months in Aurora : 32.1 TB × 13/12 = 34.8 TB
  → 24.7 % of Aurora's 128 TiB (140.7 TB) limit; 4.0× headroom

Cold 6 years in S3            : 32.1 TB × 6     = 192.6 TB raw
  Parquet + zstd on columnar payment data ≈ 6.5:1
                              : 192.6 / 6.5    = 29.6 TB in S3

Idempotency hot set (7 days)  : 10e6 × 7 × 2,136 B = 1.495e11 B = 149.5 GB
Outbox steady depth (~0); sized for a 4 h Kafka outage:
  4 h × 115.7 TPS × 4.2 rows × 700 B = 4.9 GB
```

**Partition sizing (P1).** Daily range partitions on `created_at`, hash sub-partitioned 8-way on
`tenant_id`:

```
10,000,000 rows/day ÷ 8 = 1,250,000 rows per leaf partition
  heap : 1.25e6 × 520 B = 650 MB
  PK   : 1.25e6 × 61 B  = 76 MB
→ the current day's working set fits comfortably in shared_buffers on an r6g.4xlarge (128 GB)

Leaf count: daily for 92 days + monthly roll-up for the remaining 10 months
  = (92 × 8) + (10 × 8) = 736 + 80 = 816 leaves          [NFR-15 target ≤ 900]
```

Retention is `DETACH PARTITION` → export → `DROP`: O(1), no `DELETE` storm, no bloat, no vacuum
pressure. A `DELETE`-based retention at P1 would need to remove 10 M rows/day from six tables —
about 250 M dead tuples per day — which is a full-time autovacuum job and a latency risk on the
money path.

### 14.3 Event stream throughput

**Fan-out per payment.**

| Event | Count |
|---|---:|
| `payment.created.v1` | 1.00 |
| `payment.attempted.v1` | 1.15 |
| terminal (`authorized`/`captured`/`failed`) | 1.00 |
| `payment.settled.v1` | 1.00 |
| `audit.recorded.v1` | 3.00 |
| **Total** | **7.15** |

Envelope + payload uncompressed p50 = 1 178 B; zstd level 3 on the JSON envelope ≈ 3.6:1 →
**327 B on the wire**.

```
P1 average (115.7 TPS)
  events/s  = 115.7 × 7.15 = 827
  producer  = 827 × 327 B  = 270 KB/s   = 0.27 MB/s
  RF=3 write= 3 × 0.27     = 0.81 MB/s
  5 consumer groups egress = 5 × 0.27   = 1.35 MB/s
  broker aggregate ≈ 2.2 MB/s (17.6 Mbps)

P1 diurnal peak (500 TPS)
  events/s  = 3,575 ;  producer = 1.17 MB/s ;  aggregate ≈ 9.4 MB/s

P2 sustained (5,000 TPS)
  events/s  = 35,750
  producer  = 35,750 × 327 B = 11.69 MB/s
  inter-broker replication (2 followers) = 23.4 MB/s
  consumer egress (5 groups)             = 58.5 MB/s
  broker aggregate ≈ 93.6 MB/s = 749 Mbps
  across 6 brokers = 125 Mbps/broker  → 1.2 % of 10 GbE, 12.5 % of a 1 GB/s gp3 volume

P2 burst (15,000 TPS)
  3× → aggregate ≈ 281 MB/s = 2.25 Gbps ; binding constraint becomes per-broker EBS throughput
  → gp3 provisioned at 500 MB/s per broker
```

**Topic retention sizing.**

```
pp.payments.payment.v1 — 48 partitions, 30 d retention
  payment events only = 4.15/payment × 327 B = 1,357 B/payment
  P1 : 10e6/day × 1,357 B = 13.57 GB/day × 30 × RF3 = 1.22 TB   (25.4 GB/partition)
  P2 : 100.5e6/day        = 136 GB/day   × 30 × RF3 = 12.3 TB   (256 GB/partition)

pp.audit.v1 — 12 partitions, 400 d retention
  3.0/payment × 327 B = 981 B/payment
  P1 : 9.81 GB/day × 400 × RF3 = 11.8 TB  → too large for broker-local storage
  → tiered storage to S3 after 7 days; broker-local = 7 × 9.81 × 3 = 206 GB
```

**Why 48 partitions on the payment topic.** Partition count is set by required *consumer
parallelism*, not by byte throughput (which, at 11.69 MB/s, would be satisfied by 3 partitions):

```
P2 payment events/s = 4.15 × 5,000 = 20,750
per-event consumer cost (dedup INSERT + projection write) ≈ 1.2 ms CPU
required concurrency = 20,750 × 0.0012 s = 24.9 consumers
→ 32 consumers per group at steady state, 48 partitions leaves headroom for
  rebalance skew and for a group to catch up after an outage at 1.5× normal rate
```

### 14.4 Connection pool sizing (Little's Law)

`L = λ × W`, where `L` is required concurrent connections, `λ` the arrival rate and `W` the
*database-occupancy* time — not the request duration.

**The load-bearing design decision.** The database connection is **released before the gateway call
and re-acquired after it**. Holding it across the call would make `W = W_db + W_gateway`:

```
held across gateway call : L = 5,000 × 8.0 s   = 40,000 connections   (impossible)
released before dispatch : L = 5,000 × 0.012 s = 60 connections
```

A factor of 667×. This is why baseline §12 splits stages 13 (persist) and 14 (dispatch) and why
`payment-api` and `payment-orchestrator` are separate binaries.

**`payment-orchestrator`, P2 (5 000 TPS/region).** DB occupancy per payment:

| Operation | ms |
|---|---:|
| Idempotency claim | 2.5 |
| Merchant context (2 % cache miss × 20 ms) | 0.4 |
| Attempt insert | 2.0 |
| L7 transition + outbox (one txn) | 5.0 |
| Idempotency completion | 2.0 |
| **W_db** | **11.9 → 12.0** |

```
L      = 5,000 × 0.012 = 60
×1.5   (service-time tail, not the mean)          = 90
÷ 20 orchestrator pods                            = 4.5
→ per-pod pool = ceil(4.5) + 2 reserve = 7        → 140 cluster-wide
```

**All services, P2:**

| Service | λ (/s) | W_db (ms) | L | ×1.5 | Pods | Per pod | Total |
|---|---:|---:|---:|---:|---:|---:|---:|
| `payment-orchestrator` | 5 000 | 12.0 | 60 | 90 | 20 | 7 | 140 |
| `payment-api` | 5 000 | 3.0 | 15 | 23 | 30 | 3 | 90 |
| `event-consumer` | 20 750 | 1.2 | 25 | 38 | 32 | 3 | 96 |
| `webhook-ingress` | 7 000 | 3.0 | 21 | 32 | 16 | 4 | 64 |
| `outbox-relay` | batch | — | — | — | 4 | 8 | 32 |
| `workflow-worker` | 500 | 8.0 | 4 | 6 | 6 | 4 | 24 |
| `control-plane-api` | 200 | 15.0 | 3 | 5 | 6 | 3 | 18 |
| Migrations / ops / `platformctl` | — | — | — | — | — | — | 20 |
| **Total** | | | | | | | **484** |

```
Aurora db.r6g.4xlarge (128 GB) max_connections
  = LEAST(DBInstanceClassMemory/9531392, 5000)
  = LEAST(128×1024³/9,531,392 ≈ 14,420, 5000) = 5,000

484 / 5,000 = 9.7 % of the cap
3× incident surge = 1,452 = 29 % of the cap
```

RDS Proxy is nonetheless deployed — not for the steady-state count, but to absorb the reconnect
storm at failover, where 484 pods reconnecting simultaneously is what actually threatens the cap.

Read replicas: read traffic ≈ 3× write traffic → 15 000 RPS × 2 ms = 30 concurrent connections.
Two replicas at 64 connections each is ample; the replicas are sized for **failover capacity**, not
for read load.

### 14.5 Cache sizing

**Operations per payment:** 1 idempotency mirror `GET` + 1 `SETEX` + 2 rate-limit token-bucket ops
+ 0.02 config-cache network `GET` (98 % local hit) = **4.02 ops**.

```
P2 : 5,000 × 4.02 = 20,100 ops/s
     single-shard Redis handles ~100,000 ops/s → 20 % of one shard
     sharded 3× anyway, for failure isolation and per-tenant memory quotas
```

**Memory — and why the mirror stores a pointer, not a body:**

```
mirror record = key 96 B + value 120 B + Redis overhead ~80 B = 296 B, TTL 1 h
P2 : 5,000 × 3,600 × 296 B = 5.33 GB
P1 diurnal peak : 500 × 3,600 × 296 B = 533 MB

If the mirror stored the full response snapshot (~1,600 B):
P2 : 5,000 × 3,600 × 1,600 B = 28.8 GB  → 5.4× the memory, for a cache
```

Rate-limit buckets: 50 000 merchants × 2 buckets × 120 B = **12 MB** (negligible).

Provisioned: 3 × `cache.r6g.xlarge` (26.3 GB usable each) = 78.9 GB → **14.8× headroom** over the
P2 working set. PostgreSQL remains authoritative for idempotency (baseline §14.3), so a total Redis
loss costs latency, never correctness.

### 14.6 Compute sizing and unit cost

**CPU per payment** (Go, measured p50 across the data plane):

| Service | ms CPU | × occurrences | Total ms |
|---|---:|---:|---:|
| `payment-api` | 0.9 | 1.00 | 0.9 |
| `payment-orchestrator` | 2.4 | 1.00 | 2.4 |
| `event-consumer` | 1.2 | 4.15 | 5.0 |
| `webhook-ingress` | 0.5 | 1.40 | 0.7 |
| **Total** | | | **9.0 ms** |

```
P2 : 5,000 × 0.009 s = 45 vCPU of pure work
     at 60 % target utilisation          = 75 vCPU
     × 3 headroom (AZ loss + burst)      = 135 vCPU (data plane)
     + control/automation plane          ≈  30 vCPU
     total ≈ 165 vCPU → 22 × m6i.2xlarge across 3 AZs

P1 : 500 × 0.009 = 4.5 vCPU → 13.5 vCPU with headroom
     + control/automation ≈ 30 vCPU → 44 vCPU → 8 × m6i.2xlarge
```

At P1 the cluster is dominated by fixed overhead rather than by payment work. That is precisely why
NFR-58's target is volume-banded rather than a single number.

**Monthly cost model, P1 (300 M payments/month, us-east-1, 1-year reserved/savings applied ≈ 0.72×):**

| Component | Sizing | $/month |
|---|---|---:|
| Aurora compute | 1 × r6g.4xl writer + 2 × r6g.2xl readers | 1 951 |
| Aurora storage (I/O-Optimized) | 34 800 GB × $0.225 | 7 830 |
| MSK brokers + storage | 6 × m5.2xlarge + 1.5 TB gp3 | 1 740 |
| EKS worker nodes | 8 × m6i.2xlarge | 1 614 |
| EKS control plane | 1 cluster | 73 |
| ElastiCache Redis | 3 shards × cache.r6g.large, multi-AZ | 650 |
| S3 + Glacier IR | evidence, archives, reports | 384 |
| Observability (self-hosted on-cluster + S3 backend) | | 900 |
| NAT, ALB, inter-AZ transfer, KMS, Secrets Manager, WAF | | 700 |
| **Total** | | **15 842** |

```
Unit cost = $15,842 ÷ 300,000 thousand-payments = $0.0528 per 1,000 payments
```

**The cost shape.** Approximately $10 900/month of that total is volume-insensitive (the Aurora
compute floor, the MSK floor, the EKS floor, Redis, observability and network). The remainder scales
with volume, which produces the band table in NFR-58:

| Payments/month | Fixed | Variable | Total | $/1 000 |
|---:|---:|---:|---:|---:|
| 10 M | 10 900 | 300 | 11 200 | **1.120** |
| 50 M | 10 900 | 1 400 | 12 300 | **0.246** |
| 100 M | 10 900 | 2 700 | 13 600 | **0.136** |
| 300 M | 10 900 | 4 942 | 15 842 | **0.053** |
| 1 000 M (sharded) | 18 500 | 22 500 | 41 000 | **0.041** |

**Where the money goes, and the lever.** Aurora I/O-Optimized storage is 49 % of the P1 bill. The
13-month hot window is what keeps it at $7 830; retaining the full 7-year obligation in Aurora
would cost:

```
32.1 TB/yr × 7 yr = 224.7 TB × 1,000 GB × $0.225 = $50,558/month
```

against $7 830 hot + roughly $700 for 29.6 TB of Parquet in S3 Standard-IA. **Hot/cold tiering is
worth ~$42 000/month at P1** — an order of magnitude more than any compute optimisation available,
which is why NFR-15's partition-detach retention is a cost requirement as much as a performance one.

Margin check against business `CC-2` (≥ 80 % gross margin at ≥ 100 M payments/month): at $0.136
infrastructure per 1 000 payments and a representative price of $1.20 per 1 000 authorization
attempts, infrastructure is 11.3 % of revenue, leaving ample room for support and amortised
engineering inside the 20 % cost envelope.

---

## 15. Alternatives considered and rejected

| # | Alternative | Why rejected |
|---|---|---|
| NALT-1 | **Active/active multi-region payment processing** | Would improve RTO from 15 min to near zero. Requires either a globally consensused store on the money path (adding tens of milliseconds to every write, blowing NFR-01) or conflict resolution on financial state (which has no correct answer: two regions cannot both decide whether an authorization happened). Baseline A9 chooses active/passive; NFR-26 rehearses the 15-minute RTO instead of pretending to eliminate it. Revisit only if a contractual RTO below 15 min appears. |
| NALT-2 | **`synchronous_commit = off` on the payment path** | Would cut commit latency by roughly 2–4 ms per transaction, about 4 % of the in-app budget. Costs a bounded window of acknowledged-then-lost writes on a crash, which for money state is unacceptable at any latency saving (NFR-24). Rejected without qualification. |
| NALT-3 | **Redis as the authoritative idempotency store** | Sub-millisecond claims against ~2.5 ms in PostgreSQL — a real 2 % improvement in the in-app budget. Rejected because a Redis failover can lose acknowledged writes, and a lost idempotency claim is a double charge (BR-21). Redis mirrors completed records as a latency accelerator only. |
| NALT-4 | **UUIDv4 primary keys** | Ubiquitous and library-supported. Random UUIDs fragment B-tree insert points across the whole index, roughly doubling index write amplification and destroying page locality — at P1's 10 M inserts/day across six tables this is a measurable throughput and WAL-volume cost, and it inflates NFR-16's index budget. ULIDs give time-ordered locality with 80 bits of entropy (baseline §6). |
| NALT-5 | **Retain everything hot in Aurora for the full 7-year obligation** | Operationally simplest: one query surface, no restore latency, no export pipeline. Costs ~$50 558/month against ~$8 530 tiered (§14.6) — approximately $500 000/year for convenience. Rejected; cold data is queried at a rate of roughly 1 request per merchant per year. |
| NALT-6 | **`DELETE`-based retention instead of partition detachment** | No partition-management machinery, no leaf-count ceiling. At P1 it means removing ~250 M dead tuples/day across six tables, which is a permanent autovacuum load and a latency risk on the money path, plus index bloat that inflates NFR-16's budget. `DETACH` is O(1) and touches no live page. |
| NALT-7 | **Hold the database connection across the gateway call** | Simplifies the orchestrator into one linear transaction and removes a class of "state persisted but call not made" reasoning. Would require ~40 000 connections at P2 against 60 (§14.4) — a 667× difference, and simply not provisionable. The two-phase persist-then-dispatch shape is forced by this arithmetic, not chosen for elegance. |
| NALT-8 | **`merchant_id` as a metric label** | Would make per-merchant dashboards trivial and remove a whole class of support questions. At 50 000 merchants × ~18 metrics × their existing label combinations, active series would run to the tens of millions, making the metrics backend the platform's largest cost and its most fragile component. Baseline §22.3; identity lives in logs, traces and exemplars, which support the same drill-down at query time. |
| NALT-9 | **Exactly-once delivery via a transactional broker** | Would remove dedup tables and idempotent-consumer logic. Exactly-once *delivery* is not achievable across process and broker boundaries; the transactional-producer mechanisms available achieve it only within a single broker's scope and break at any external side effect (a gateway call, a webhook). Baseline A8 targets at-least-once delivery with effectively-once *business* effect, backed by database invariants I1–I3 — which is what the business actually needs and what survives a bug in the dedup path. |
| NALT-10 | **Hard deletion for GDPR right-to-erasure** | Direct, verifiable, and what a data subject intuitively expects. It would destroy financial records held under a 7-year legal obligation, creating an unresolvable conflict between GDPR Art. 17 and AML/records law. Crypto-shredding of a per-purpose data key renders personal data unreadable while leaving the ledger intact (NFR-38, FR-15). |
| NALT-11 | **Per-tenant dedicated infrastructure for every tenant** | Trivially satisfies NFR-29 isolation and NFR-59 attribution. The fixed floor of ~$10 900/month per tenant destroys NFR-58 below roughly 80 M payments/month per tenant, and 500 tenants means 500 upgrade pipelines against NFR-48's cadence target. Pooled-with-RLS is the default; the siloed tier exists where isolation is contractual and is priced accordingly (baseline §16). |
| NALT-12 | **Advisory (non-blocking) architecture fitness functions** | Lower friction, fewer blocked merges, and the usual argument that engineers should be trusted. Every advisory rule decays to zero enforcement within two quarters. NFR-53 makes them blocking; the friction is the mechanism. |
| NALT-13 | **A single coverage threshold across the whole repository** | Simpler to state and to enforce. It either under-protects the domain (where an FSM bug moves money) or over-burdens the adapters (where a contract suite is the meaningful test and line coverage is theatre). NFR-52 sets thresholds per layer because the layers carry different risk. |
| NALT-14 | **Kafka partition count derived from byte throughput** | Would give 3 partitions on the payment topic at P2's 11.69 MB/s — technically sufficient for bytes and cheaper to operate. Consumer parallelism, not bandwidth, is the binding constraint: 24.9 consumers are needed to keep up (§14.3), and a group cannot exceed the partition count. Under-partitioning is also effectively irreversible for an ordered topic, since repartitioning changes key-to-partition assignment and breaks per-aggregate ordering. 48 is chosen for headroom against a constraint that cannot be relaxed later. |

---

## 16. Coverage summary

| Category | NFRs | Count |
|---|---|---|
| Performance & latency | NFR-01..NFR-06 | 6 |
| Throughput | NFR-07..NFR-12 | 6 |
| Scalability & data growth | NFR-13..NFR-18 | 6 |
| Availability, SLI/SLO, error budget | NFR-19..NFR-23 | 5 |
| Durability, RPO/RTO, backup | NFR-24..NFR-27 | 4 |
| Security | NFR-28..NFR-36 | 9 |
| Privacy, residency, compliance | NFR-37..NFR-42 | 6 |
| Observability | NFR-43..NFR-46 | 4 |
| Operability | NFR-47..NFR-51 | 5 |
| Maintainability | NFR-52..NFR-55 | 4 |
| Portability | NFR-56..NFR-57 | 2 |
| Cost | NFR-58..NFR-59 | 2 |
| Accessibility | NFR-60..NFR-61 | 2 |
| **Total** | | **61** |

Every NFR above has a number, an instrument, an automatic consequence and a named mechanism. Per
baseline §26, each is traced in `docs/spec/09-traceability.md` to at least one design section, one
package and one test; CI fails on an orphan requirement or an orphan test.
