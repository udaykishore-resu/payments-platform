# ADR-021: Active/passive multi-region for money state, active/active for the control plane

- **Status:** Accepted
- **Date:** 2026-08-26
- **Deciders:** Platform Architecture
- **Baseline reference:** §1.3 ambiguities A4 and A9, §15 (consistency model), §18 (RPO ≤ 5 s, RTO ≤ 15 min), §24 (F-17 region loss) of docs/spec/00-design-baseline.md
- **Supersedes / Related:** Related to ADR-007 (plane independence), ADR-008 (tenancy), ADR-020 (event backbone)

## Context

Region loss is rare and catastrophic. The design question is not "will we survive it" but "what
guarantee do we give up in order to survive it", because for financial state the two candidate
answers are mutually exclusive.

The forces:

1. **Money writes are CP by decision (A4).** A payment write that cannot reach the regional
   primary fails closed with `503`. Under partition, refusing a payment costs one lost sale;
   double-charging costs a chargeback, a fine and trust.
2. **Active/active money movement requires either global consensus or conflict resolution on
   financial state.** Global consensus (Spanner-class, or a multi-region quorum) costs
   cross-region round-trips on every write: 60–90 ms US-East↔US-West, ~80–100 ms US↔EU. Against a
   p99 budget of 250 ms *excluding* gateway time, where our whole pipeline is currently ~75 ms,
   that is not affordable. Conflict resolution on a payment — deciding after the fact which of two
   authorizations was real — is not a thing that can be done correctly.
3. **Split-brain is the specific danger.** If a region is partitioned from the health checkers but
   still serving, and we promote the secondary, two writers exist. Invariant I3 (ADR-012) is
   enforced by a partial unique index *within one database*; two databases means two indexes and
   no shared constraint. The platform's central safety property evaporates exactly when it is
   needed.
4. **The control plane has none of these properties.** Configuration writes are low-volume,
   naturally partitionable by tenant, and tolerant of the latency that cross-region consistency
   requires. Reads are far more frequent than writes and benefit enormously from being local.
5. **Targets are set:** RPO ≤ 5 s (in-region 0 via synchronous commit; cross-region Aurora Global
   ≤ 1 s typical, 5 s budgeted), RTO ≤ 15 min for a region failover (§18).
6. **Data residency is a hard constraint for some tenants** (§17.3), which means region topology
   is a compliance concern as well as an availability one.

What breaks if we choose wrong: two writers producing two authorizations for one payment
(active/active money), or a multi-hour outage with no failover path (single region), or a
control plane whose global users all pay a trans-Atlantic round trip for every read.

## Decision

**Money state is active/passive per payment-processing region. The control plane is
active/active. Aurora Global secondary promotion is deliberately manual.**

1. **Money state (BC-6, BC-7, BC-8 — payments, attempts, webhooks, ledger):** exactly one active
   writer region per payment-processing region pair. Aurora Global Database replicates to the
   passive region; the passive region runs the data-plane services warm but takes no money writes.
   Cross-region writes never occur.
2. **Control plane (BC-1, BC-2, BC-4 registry, BC-5):** active/active. Tenants are homed to a
   region for their *writes* (so the write path within a tenant is single-writer and needs no
   conflict resolution), while reads are served locally in every region from replicated state.
   Configuration is published to Kafka and consumed everywhere, which is the same mechanism
   ADR-007 and ADR-019 already require.
3. **Failover is automatic at the DNS layer, manual at the database layer.** Route 53 health
   checks (3 consecutive failures over 90 s) fail traffic over; **Aurora Global secondary
   promotion requires an incident commander's authorization** after confirming the primary is
   genuinely gone. This is the one place we accept a human in the loop, and the reason is stated
   in §24 F-17: automatic cross-region promotion of a financial primary risks split-brain, and
   double-charging costs a chargeback, a fine and trust while fifteen minutes of downtime costs
   fifteen minutes.
4. **Failback is a planned operation, never automatic.**
5. **Residency:** a tenant's declared residency region determines where its personal data and
   its payment processing live (§17.3), and the routing engine will not select a gateway whose
   region violates that policy (ADR-015 hard filter). Residency constrains which region pairs a
   tenant may use.
6. **The passive region is warm and continuously exercised:** it runs the data plane, consumes
   events, and serves reads. A cold standby that has never served traffic is a hypothesis, not a
   DR plan.
7. **Quarterly DR drills** (`scripts/dr-drill.sh`) measure RTO and RPO from the last committed
   payment. The drill outcome is the validation of this ADR.

## Options considered

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Active/passive money + active/active control (chosen)** | Preserves the single-writer property that makes invariant I3 enforceable, so double-charging remains structurally impossible even during a regional incident; no cross-region latency on the payment path; RPO ≤ 5 s and RTO ≤ 15 min are achievable with Aurora Global; the control plane — where active/active is cheap and valuable — gets local reads worldwide; passive capacity is genuinely usable for reads and for the control plane, so it is not pure waste | Up to 15 minutes of money-path unavailability during a region loss; passive-region compute is largely idle for the money path (cost); two topologies to operate and reason about; manual promotion means the RTO includes human response time, so on-call quality is part of the availability story | **Accepted** |
| **Active/active everywhere (money included)** | Zero RTO — traffic simply continues in the surviving region; no failover decision, no human in the loop; regional capacity fully utilised; lowest latency for globally distributed merchants | Requires either global consensus on every money write (60–100 ms cross-region round-trip per write, which alone exceeds a third of the entire 250 ms p99 budget and would apply to the idempotency claim, the attempt write and the state transition — three round-trips, not one) or conflict resolution on financial state, which is not solvable: there is no correct after-the-fact answer to "both regions authorized this payment". Invariant I3 cannot be expressed across two independent databases, so the platform's central safety property would have to be replaced by an application-level protocol — the exact substitution ADR-012 exists to avoid. This is the option an availability-focused engineer pushes for, and for the *control* plane it is right, which is why we adopted it there — it loses for money state on the impossibility of the conflict-resolution step | Rejected for money state; **adopted for the control plane** |
| **Single region only** | Simplest by a wide margin; no replication, no failover, no drills, no split-brain risk; lowest cost; no residency topology to manage | A region loss is a total outage of indeterminate length — AWS regional events have lasted hours. For a payment platform with 99.99 % contractual availability and tenants who ask about DR in every procurement, this is not defensible. It also forecloses residency requirements that need EU-resident processing | Rejected |
| **Active/active money with per-merchant region affinity (sharded writers)** | Each merchant has exactly one writer region, so no conflicts within a merchant; both regions active; I3 holds per merchant because all of a merchant's payments are in one place; better capacity utilisation than passive | Genuinely attractive, and closest to a real alternative. It loses on failure handling: when a merchant's home region is lost, that merchant is down until *their* shard is promoted — so we still need the promotion machinery, but now per-shard rather than per-region, with more moving parts and a more complex incident. It also makes the topology tenant-visible and migration between regions a data-movement project. Revisit if global growth makes single-region-primary latency a problem for a large merchant population | Rejected for now — the strongest rejected option |
| **Automatic cross-region promotion** | Removes human latency from RTO, potentially reducing it to minutes | A region partitioned from the health checkers but still serving would be promoted against, producing two writers and breaking A4's CP guarantee. The failure mode is silent and financial. §24 F-17 states the trade explicitly: fifteen minutes of downtime costs fifteen minutes | Rejected |

## Consequences

### Positive

- Single-writer money state preserved, so I3 and the whole invariant stack (I1–I5) remain
  database-enforced under all conditions including a regional incident.
- No cross-region latency on the payment path; the §12 budget stays intact.
- Control-plane reads are local everywhere, which is where the latency actually helps users.
- RPO ≤ 5 s and RTO ≤ 15 min are achievable with commodity managed services rather than a bespoke
  consensus layer.
- Residency requirements are expressible as region homing plus a routing hard filter.

### Negative

- **Up to 15 minutes of money-path unavailability during a region loss**, and the clock includes
  human decision time. This must be stated in tenant contracts, not buried.
- The passive region's money-path compute is largely idle — real cost for capacity used rarely.
- Manual promotion means DR quality depends on on-call training and runbook accuracy, which decay
  without drills.
- Two operating models (active/passive and active/active) means more to document and more ways to
  be wrong about which applies to a given component.
- Tenant-to-region homing becomes a data model concern and a migration problem when a tenant
  changes residency.

### Neutral / accepted costs

- Kafka replication across regions (MirrorMaker or MSK Replicator) is required for the control
  plane's active/active reads and for the passive region's warmth. Its lag is another monitored
  signal.
- The passive region serves payment *reads* (§15 classifies `GET /v1/payments` as AP with
  read-your-writes for the caller), which recovers some of the idle capacity.

## Risks and mitigations

| Risk | Likelihood | Impact | Mitigation | Detection signal |
|---|---|---|---|---|
| Split-brain from an incorrect promotion | Low | **Critical** — I3 broken, double charges possible | Promotion is manual and requires confirmation that the primary is genuinely gone; the runbook requires positive evidence (not just health-check failure) from at least two independent signals; the old primary is fenced at the network layer before promotion | Two-writer detection: any write observed on both clusters for the same partition range is an immediate Sev-1 |
| RTO exceeded because the human is slow or the runbook is wrong | Medium | High — extended outage | Quarterly DR drills with RTO measured; runbook is a tested artifact, not a document; on-call rotation includes DR-drill participation as a requirement | Drill RTO; time-to-page and time-to-decision measured during drills |
| Passive region has drifted and cannot serve on promotion | Medium | **High** — the DR plan is fiction | The passive region runs the same deployment continuously and serves read traffic, so drift is exercised daily; drills promote for real in a lower environment, and the production drill promotes and fails back | Drill outcome; passive-region error rates on read traffic |
| Replication lag exceeds RPO | Low | Medium — data loss on failover | Aurora Global lag monitored with alerting well below the 5 s budget; the drill measures RPO from the last committed payment | Aurora Global replication lag gauge |
| Residency violated by a failover to a non-compliant region | Medium | High — regulatory breach | Region pairs are constrained by tenant residency policy; a tenant whose residency permits only one region does not have a cross-border failover option, and that is a contractual conversation, not a silent failover | Residency-policy validation on the failover plan; routing hard-filter audit |
| Cost of idle passive capacity is cut to save money | Medium | High — DR becomes theoretical | Passive capacity is a documented line item tied to the availability commitment; scaling it down is a decision that reopens this ADR | Capacity configuration drift; drill failures due to insufficient capacity |
| Control-plane active/active produces conflicting writes | Medium | Medium — config divergence | Tenants are homed for writes, so there is a single writer per tenant; configuration versions are monotonic per merchant, and a version conflict returns `409 CONFIGURATION_VERSION_CONFLICT` | Version-conflict rate; divergence check across regions |

## Validation

- **Quarterly DR drill** (`scripts/dr-drill.sh` plus `tests/chaos/region_failover_test.go`):
  asserts **RTO ≤ 15 min** and **RPO ≤ 5 s**, measured from the last committed payment. This is
  the primary validation and its outcome is recorded against this ADR.
- **Split-brain assertion:** during the drill, attempt writes against the demoted primary and
  assert they are refused at the network and database layers.
- **Passive-region readiness:** the passive region continuously serves read traffic; its error
  rate and latency are held to the same SLO as the active region's read path.
- **Replication lag:** Aurora Global lag p99 well under the 5 s RPO budget; MSK replication lag
  within its own SLO.
- **Residency audit:** for a sample of tenants with declared residency, assert that no payment or
  personal-data record exists outside the permitted region set.

## Revisit criteria

Reopen if:

1. Merchant distribution becomes genuinely global and single-primary latency materially harms
   authorization rates in a distant region — the leading alternative is then per-merchant region
   affinity (the strongest rejected option), not global active/active.
2. A managed datastore offers strongly consistent multi-region writes at a latency that fits
   inside the §12 budget *and* can express a cross-region uniqueness constraint equivalent to I3.
   The uniqueness constraint is the binding requirement, not the latency.
3. Contractual availability requirements tighten beyond what a 15-minute RTO supports, forcing
   either automatic promotion (with an explicit split-brain risk acceptance and a fencing
   mechanism strong enough to justify it) or the affinity model.
4. Residency regulation requires more than two regions for a material share of tenants, which
   would make the active/passive pair model a poor fit for the topology.
