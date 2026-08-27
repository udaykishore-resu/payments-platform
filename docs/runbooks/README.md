# Runbooks

Every alert in `deployments/prometheus/prometheusrule-alerts.yaml` and
`deployments/prometheus/prometheusrule-slo-burn.yaml` carries a `runbook_url`. This directory is
what those URLs resolve to: `https://docs.example.com/runbooks/<name>.md` is published from
`docs/runbooks/<name>.md`, one file per distinct link.

**The rule: an alert without a runbook is not allowed to page.** A page is an interrupt with a
cost — sleep, focus, a person's evening — and the only thing that justifies it is that the person
woken can act. An alert that fires into a wiki search is an alert that has spent the cost and
bought nothing. Concretely: a rule with `page: "true"` and no resolvable `runbook_url` is a
build failure (`scripts/check-runbook-links.sh`), and a new alert lands in the same pull request
as its runbook or it lands with `page: "false"`.

## Severity

The alert rules label severity `P1`/`P2`/`P3`; the runbooks below express the same thing as
**page** or **ticket**, because that is the distinction the person reading at 03:00 cares about.

| Label | Reads as | Means | Response |
|---|---|---|---|
| `P1` | **page** | Money is at risk, or an SLO is burning fast enough to be exhausted within days | 24×7 page. Acknowledge in 5 min, first action in 15 min |
| `P2` | ticket (pages in business hours) | Degraded, contained, or one failure away from P1 | Same business day |
| `P3` | ticket | A budget, a hygiene bound, or a client's bug | Next planning cycle |

Two labels are load-bearing beyond severity. `plane` (`control`, `data`, `automation`,
`observability`) is what Alertmanager routes on, and it is what tells you whether the money path
is involved before you read another word. `page: "true"/"false"` is the explicit paging decision,
separate from severity, so that changing one does not silently change the other.

Alertmanager inhibits every `plane=data` alert while `AllGatewaysUnhealthy` or a region-failover
alert is firing. One root cause produces one page. If you were paged for something in the data
plane, check `no-eligible-gateway.md` and `region-failover.md` first — if either is active, your
alert is a symptom.

## On-call expectations

- **Acknowledge a page within 5 minutes.** Acknowledging is not fixing; it is telling the rest of
  the rotation that someone has it.
- **First mitigate, then diagnose.** The runbooks are written in that order on purpose. Restoring
  service and finding the cause are different jobs and the first one has a deadline.
- **Never move money by hand.** No operator, and no automation, cancels, refunds, retries or
  fails a payment to clear an alert. `docs/security.md` §9.1 states the general form: *no alert
  may move money*, and `docs/spec/00-design-baseline.md` §12.3 states the specific one: *no timer
  may fail a payment*. `timeout-unknown.md` and `reconciliation.md` say what to do instead.
- **Escalate on a clock, not on a feeling.** Each runbook names the point at which you stop and
  call someone. Escalating early is free; escalating late is the thing postmortems are about.
- **Write it down as you go.** The incident channel is the timeline. A decision that is not in it
  did not happen, as far as the postmortem is concerned.
- **Every runbook ends the same way**: a blameless postmortem, and the control plus the test that
  would have caught it. The test is what makes the fix durable.

## Index

### Money — the payment path

| ID | Runbook | Alerts | Page? |
|---|---|---|---|
| RB-001 | [payment-api-availability.md](payment-api-availability.md) | `PaymentAPIFastBurn`, `PaymentAPISlowBurn` | yes / no |
| RB-002 | [payment-api-latency.md](payment-api-latency.md) | `PaymentAPILatencyFastBurn`, `PaymentAPILatencySlowBurn` | yes / no |
| RB-003 | [error-budget-policy.md](error-budget-policy.md) | `ErrorBudgetPolicyFreeze` | no |
| RB-004 | [gateway-degradation.md](gateway-degradation.md) | `PaymentAuthorizationRateDrop`, `GatewayCircuitOpen`, `GatewayErrorRateHigh`, `GatewayLatencyHigh` | yes / no |
| RB-005 | [no-eligible-gateway.md](no-eligible-gateway.md) | `AllGatewaysUnhealthy`, `NoEligibleGatewayErrors` | yes |
| RB-006 | [timeout-unknown.md](timeout-unknown.md) | `TimeoutUnknownSpike` | yes |
| RB-007 | [reconciliation.md](reconciliation.md) | `ReconciliationExceptionsCritical`, `ReconciliationExceptionsRising` | yes / no |

### Asynchronous plumbing

| ID | Runbook | Alerts | Page? |
|---|---|---|---|
| RB-008 | [outbox.md](outbox.md) | `OutboxBacklogGrowing`, `OutboxStalled` | no / yes |
| RB-009 | [consumer-lag.md](consumer-lag.md) | `ConsumerLagHigh`, `LedgerConsumerLagCritical` | no / yes |
| RB-010 | [dlq.md](dlq.md) | `DLQNotEmpty`, `DLQGrowingFast` | no / yes |
| RB-011 | [dlq-triage.md](dlq-triage.md) | — (the classification procedure `dlq.md` calls) | — |
| RB-012 | [kafka.md](kafka.md) | `KafkaUnderReplicated` | yes |

### Client and configuration

| ID | Runbook | Alerts | Page? |
|---|---|---|---|
| RB-013 | [idempotency.md](idempotency.md) | `IdempotencyConflictSpike`, `IdempotencyInProgressStorm` | no |
| RB-014 | [config-staleness.md](config-staleness.md) | `ConfigSnapshotStale`, `ConfigSnapshotCliff` | no / yes |
| RB-015 | [control-plane.md](control-plane.md) | `ControlPlaneAvailabilityBurn` | no |

### Stores and infrastructure

| ID | Runbook | Alerts | Page? |
|---|---|---|---|
| RB-016 | [aurora-failover.md](aurora-failover.md) | `AuroraFailoverDetected` | yes |
| RB-017 | [db-pool-exhaustion.md](db-pool-exhaustion.md) | — (queue-depth signal, `failure-handling.md` §5.3) | no |
| RB-018 | [dr-replication-lag.md](dr-replication-lag.md) | `AuroraReplicaLagHigh` | yes |
| RB-019 | [region-failover.md](region-failover.md) | — (declared by the IC; see `docs/disaster-recovery.md`) | yes |
| RB-020 | [redis-loss.md](redis-loss.md) | `RedisUnavailable` | no |
| RB-023 | [orchestrator-memory.md](orchestrator-memory.md) | `PaymentOrchestratorOOMKilled` | no |

### Automation plane

| ID | Runbook | Alerts | Page? |
|---|---|---|---|
| RB-021 | [onboarding-stuck.md](onboarding-stuck.md) | `WorkflowInstancesFailed`, `WorkflowManualGateAging`, `OnboardingSLOBreach` | no |
| RB-022 | [webhook-lag.md](webhook-lag.md) | `WebhookProcessingLagHigh`, `WebhookIngressSlow` | yes / no |
| RB-034 | [tenant-tier-migration.md](tenant-tier-migration.md) | — (planned change; `docs/multi-tenancy.md` §7) | — |
| RB-035 | [terraform-bootstrap-recovery.md](terraform-bootstrap-recovery.md) | — (`terraform/README.md`) | — |

### Observability itself

| ID | Runbook | Alerts | Page? |
|---|---|---|---|
| RB-024 | [cardinality.md](cardinality.md) | `MetricCardinalityBudgetExceeded` | no |
| RB-025 | [otel.md](otel.md) | `TraceExportFailing` | no |

### Security — the five incident classes of `docs/security.md` §9.3, plus their detectors

| ID | Runbook | Class / alert | Page? |
|---|---|---|---|
| RB-026 | [audit-integrity.md](audit-integrity.md) | `AuditChainBroken` | yes |
| RB-027 | [audit-tamper.md](audit-tamper.md) | Class: audit tamper | yes |
| RB-028 | [security-events.md](security-events.md) | `TenantMismatchSpike` | yes |
| RB-029 | [security-tenant-isolation.md](security-tenant-isolation.md) | Class: suspected cross-tenant leak | yes |
| RB-030 | [security-credential-rotation.md](security-credential-rotation.md) | Class: credential compromise | yes |
| RB-031 | [security-pci-incident.md](security-pci-incident.md) | Class: PAN in scope | yes |
| RB-032 | [security-supply-chain.md](security-supply-chain.md) | Class: image / supply chain | yes |
| RB-033 | [pan-detector.md](pan-detector.md) | `PANDetectorHits` | no |

## How to add a runbook

1. **Take the next free `RB-nnn`.** IDs are never reused; a retired runbook keeps its number and
   says what replaced it, because the number appears in postmortems.
2. **Name the file after the link, not after the alert.** Several alerts share one runbook when
   they share one mechanism — four gateway alerts all resolve to `gateway-degradation.md` because
   the diagnosis is the same tree. The file name is what the `runbook_url` says.
3. **Use the standard structure**, in this order, with no section omitted:
   header block (severity, alert, trigger, plane/service, related) → What this means → Impact →
   Immediate triage (first 5 minutes) → Diagnosis → Mitigation → Rollback / escalation →
   Verification → Follow-up.
4. **Every command must exist.** Not "should", not "will". If you write `platformctl foo`, run
   `platformctl help` first. A command that does not exist is worse than no command, because it
   costs an incident's worth of confusion before anyone doubts the document.
5. **Immediate triage is commands, not prose.** Copy-pasteable, in order, with the expected
   output. Diagnosis is a decision tree whose branches point at named mitigations.
6. **Say what is unavailable.** Where the design documents describe a tool this repository does
   not yet ship, the runbook says so and gives the manual path. See `dlq.md` for the pattern.
7. **Add the row to the index above and the link to the alert rule**, then run
   `scripts/check-runbook-links.sh`. It fails the build on a dangling `runbook_url` or a
   `docs/runbooks/...` reference with no file behind it.

## Cross-references

- `docs/observability.md` — the alert catalogue, the SLOs, the four dashboards, the metric registry.
- `docs/failure-handling.md` — the failure catalog F-1…F-20, the degradation ladder, backpressure, DLQ topology.
- `docs/disaster-recovery.md` — RPO/RTO, fencing, promotion, failback, the drill schedule.
- `docs/security.md` — the trust model, the detection table (§9.1), the incident classes (§9.3).
- `docs/deployment.md` — environments, rollout, rollback, the release gates.
