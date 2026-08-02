# ADR-005: Amazon EKS (Kubernetes) as the deployment platform

## Status
Accepted

## Context
Need a deployment platform for a stateless, horizontally-scalable Go service that supports
zero-downtime rolling deploys, AZ-aware scheduling, autoscaling, and standardized
health-check-driven self-healing — while staying portable enough to avoid deep single-vendor
lock-in for a system expected to run for years.

## Decision
Amazon EKS.

- Kubernetes' Deployment + ReadinessProbe/LivenessProbe + PodDisruptionBudget +
  HorizontalPodAutoscaler primitives directly implement the "zero downtime deployment",
  "self-healing", and "graceful startup/shutdown" requirements from the org-wide quality bar,
  using battle-tested, well-understood mechanisms rather than bespoke tooling.
- Portable: the same manifests (with environment overlay via Kustomize) run on any conformant
  Kubernetes cluster — reduces AWS lock-in risk relative to proprietary orchestration, and gives a
  credible multi-cloud/DR-to-another-provider option if ever needed.
- Mature ecosystem for the observability and security stack this system needs: Prometheus
  Operator, OpenTelemetry Collector as a DaemonSet, cert-manager for TLS automation, external-dns,
  network policies for microsegmentation.
- EKS specifically (vs self-managed K8s) offloads control-plane HA and upgrades to AWS, which is
  the right tradeoff for a small platform team that should spend its time on the payments domain,
  not Kubernetes control-plane operations.

## WHEN to use this choice
Right default for stateless or semi-stateless services needing fine-grained scaling, scheduling,
and self-healing control, run by a team with (or building) Kubernetes operational competency.
Reassess for AWS Lambda for spiky, short-lived, low-connection-count workloads (e.g. the outbox
relay could plausibly move to Lambda+EventBridge Scheduler later); reassess for ECS Fargate if
the team wants to shed Kubernetes operational overhead entirely in exchange for less portability
and a smaller ecosystem.

## Alternatives Considered
- **ECS Fargate**: simpler mental model, zero node management, but weaker portability, smaller
  ecosystem for the observability/security tooling this platform standardizes on, and less
  flexible scheduling (no native pod anti-affinity across AZs in the same expressive way).
  Reasonable choice for a smaller team; rejected here given the multi-service platform ambition.
- **AWS Lambda**: excellent for spiky/event-driven workloads and true pay-per-use, but a poor fit
  for a service that wants long-lived DB connection pools, an in-process background
  worker (outbox relay), and predictable P99 latency without cold-start tail risk. Considered for
  the outbox relay in isolation; kept in-process with the API for v1 to minimize moving parts,
  revisit if relay and API scaling needs diverge significantly.
- **Self-managed Kubernetes (kops/kubeadm on EC2)**: full control over control-plane version and
  networking, but the operational burden (etcd backups, control-plane upgrades, CVE patching) is
  a poor use of a platform team's time versus a managed offering. Rejected.

## Tradeoffs
- Kubernetes has a real learning curve and operational surface area (RBAC, network policies,
  admission control, upgrade cadence) — mitigated by investing in the Kubernetes Administrator
  role's runbooks and using managed add-ons (EKS managed node groups, Karpenter for node
  autoscaling) to reduce hand-rolled operations.
- Multi-AZ EKS node groups cost more at idle than a single-AZ deployment — accepted, this is the
  direct cost of the availability NFR (99.95%).

## Risks
- Cluster upgrade windows are a real operational event (control plane + node group + add-on
  compatibility matrix) — mitigated by staging-environment upgrade rehearsal and a documented
  rollback procedure in the runbook.
- Misconfigured PodDisruptionBudget or resource requests/limits can silently reduce actual
  availability below the SLO during node drains — mitigated by the production checklist gate and
  periodic chaos testing (pod/node kill drills) that would catch this in staging before it bites
  in prod.
