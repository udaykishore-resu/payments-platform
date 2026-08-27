# Diagram Index

The canonical diagram set for the multi-tenant payment gateway onboarding and payment
orchestration platform. Every diagram is derived from
[`docs/spec/00-design-baseline.md`](../spec/00-design-baseline.md) and uses its exact names for
the five planes, the nine bounded contexts, the nine deployables, the states in §8 and §9, the
events in §13.2, the topics in §13.3 and the pipeline stages in §12. If a diagram disagrees with
the baseline, the diagram is the defect.

Diagrams are Mermaid in fenced blocks and render anywhere Mermaid is supported. Files with more
than one diagram split by heading rather than producing an unreadable single graph.

| # | Title | Diagram type | Question it answers | Primary audience |
|---|---|---|---|---|
| 01 | [System Context](01-system-context.md) | flowchart | Who talks to the platform, what does it talk to, and where does trust change hands? | Everyone; new joiners; auditors |
| 02 | [High-Level Design / Container View](02-high-level-design.md) | flowchart ×2 | Which of the nine deployables owns this, and what breaks if it is down? | Engineers; architects |
| 03 | [Control Plane](03-control-plane.md) | flowchart ×2 | How does declared desired state become effective configuration, and how is drift closed? | Platform engineers; operators |
| 04 | [Automation Plane](04-automation-plane.md) | flowchart ×2 | How does a long-running saga survive a crash, and what happens when it must unwind? | Workflow and backend engineers |
| 05 | [Validation Plane](05-validation-plane.md) | flowchart ×2 | Which of the seven levels rejects this input, where does it run, and with what error code? | Engineers; QA; support |
| 06 | [Data Plane](06-data-plane.md) | flowchart ×2 | What happens to a payment request at each of the 17 stages, and where are the bulkheads? | Data-plane engineers; SRE |
| 07 | [Merchant Onboarding Saga](07-merchant-onboarding.md) | sequenceDiagram ×2 | What are the twelve steps, and what is undone if step 10 fails? | Onboarding engineers; product; compliance |
| 08 | [Payment Flow](08-payment-flow.md) | sequenceDiagram ×2 | How does a payment go from authorize to capture to settle, and where is idempotency enforced? | Engineers; integration partners |
| 09 | [Gateway Routing](09-gateway-routing.md) | flowchart, stateDiagram-v2 | Why did this payment go to this gateway, and what could have excluded it? | Payments engineers; commercial |
| 10 | [Gateway Failover](10-gateway-failover.md) | flowchart, sequenceDiagram | When may we retry elsewhere, and when must we absolutely not? | Payments engineers; risk; SRE |
| 11 | [Webhook Flow](11-webhook-flow.md) | sequenceDiagram, flowchart | How is an untrusted gateway callback verified, deduped and applied without corrupting state? | Engineers; security |
| 12 | [Event Architecture](12-event-architecture.md) | flowchart ×2 | How does a state change become an event, and what happens to a poison message? | Engineers; data consumers |
| 13 | [State Machines](13-state-machines.md) | stateDiagram-v2 ×3 | Is this transition legal, and what guards it? | Engineers; QA; support; auditors |
| 14 | [Database Architecture](14-database-architecture.md) | flowchart, erDiagram, flowchart | Where does this table live, how is it partitioned, and what enforces tenant isolation? | Backend engineers; DBAs |
| 15 | [Kubernetes Architecture](15-kubernetes-architecture.md) | flowchart ×2 | Where does each workload run, and what stops one plane starving another? | SRE; platform engineers |
| 16 | [AWS Architecture](16-aws-architecture.md) | flowchart ×2 | What is the cloud substrate, and what is the multi-region posture? | SRE; infrastructure; security |
| 17 | [Security Architecture](17-security-architecture.md) | flowchart ×2 | What are the trust zones, how does identity flow, and where is the PCI boundary? | Security; compliance; auditors |
| 18 | [Observability Architecture](18-observability-architecture.md) | flowchart ×2 | How does telemetry reach a backend, and how does observation change routing? | SRE; engineers |
| 19 | [Disaster Recovery](19-disaster-recovery.md) | flowchart, sequenceDiagram, flowchart | What replicates where, and what is the ordered failover procedure? | SRE; incident commanders; auditors |
| 20 | [End-to-End Sequence](20-end-to-end-sequence.md) | sequenceDiagram ×2 | What is the whole story, from signup to settled payment to the feedback loop? | Everyone; exec and customer briefings |

## Reading orders

- **New engineer**: 01 → 02 → 20 → then the plane diagram for your area (03, 04, 05, 06).
- **Payments deep dive**: 06 → 08 → 09 → 10 → 11 → 13.
- **Infrastructure and on-call**: 15 → 16 → 18 → 19, with 14 for the data layer.
- **Audit or compliance review**: 01 → 17 → 13 → 12 → 19.

## Conventions

- Solid edges are synchronous calls or direct writes; dotted edges are asynchronous, event-driven
  or trust relationships.
- Node labels avoid unquoted punctuation so every diagram parses under strict Mermaid.
- Error codes quoted on edges are the reserved codes from baseline §20.2.
- Section references such as §12 or A7 point at
  [`docs/spec/00-design-baseline.md`](../spec/00-design-baseline.md).
