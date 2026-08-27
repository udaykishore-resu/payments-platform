# 15 — Kubernetes Architecture

## What this shows and why it matters

How the deployables land on EKS: five namespaces, one per plane — `pp-control-plane`,
`pp-data-plane`, `pp-automation`, `pp-platform`, `pp-observability`, each carrying a `pp.plane`
label — dedicated node groups so control-plane and data-plane workloads cannot contend for the
same CPU, separate ingress paths for the money path and the admin path, a service mesh providing
mTLS and per-gateway outlier detection, and autoscaling driven by the signal that actually
predicts each workload's load. Seven of the nine binaries are long-running workloads;
`platformctl` runs as a Job with its own service account, and `gateway-simulator` is denied
admission in production by policy. The organising
principle is the same one that produced nine binaries rather than one: **blast radius**. A
`workflow-worker` retry storm during a KYC vendor outage must not be able to evict a
`payment-orchestrator` pod.

## Diagram A — Namespaces, node groups and workload placement

```mermaid
flowchart TB
  subgraph NGDATA["Node group data - compute optimized, taint pp.plane equals data"]
    subgraph NSDATA["Namespace pp-data-plane, label pp.plane data"]
      PAPI["payment-api - HPA on RPS and concurrency, PDB minAvailable 75 percent"]
      PORC["payment-orchestrator - HPA on in-flight gateway calls"]
      WHIG["webhook-ingress - HPA on RPS, aggressive scale-up, slow scale-down"]
      ORLY["outbox-relay - fixed replicas, leader-free, SKIP LOCKED"]
      ECON["event-consumer - KEDA on Kafka consumer lag"]
    end
  end

  subgraph NGCTL["Node group control - general purpose, taint pp.plane equals control"]
    subgraph NSCTL["Namespace pp-control-plane, label pp.plane control"]
      CPAPI["control-plane-api - HPA on RPS"]
    end
    subgraph NSAUT["Namespace pp-automation, label pp.plane automation"]
      WFW["workflow-worker - KEDA on due-work backlog"]
    end
  end

  subgraph NGSYS["Node group general - platform addons"]
    subgraph NSOBS["Namespace pp-observability, label pp.plane observability"]
      OTELG["OTel collector gateway - Deployment, nodeSelector pp.plane general"]
      OTELA["OTel collector agent - DaemonSet, tolerates every pp.plane taint so it can observe the tainted data nodes"]
    end
    subgraph NSSYS["Namespace pp-platform, label pp.plane platform"]
      MESH["Service mesh control plane"]
      ESO["External Secrets Operator"]
      CERTM["cert-manager"]
      MIG["platformctl-migrator Job - migrate, seed, config, certify, dr-drill, outbox, workflow, verify-audit-chain"]
    end
  end

  SPREAD["topologySpreadConstraints plus pod anti-affinity across 3 AZs"]
  TAINT["Taint pp.plane equals data is NoSchedule - control and automation carry no matching toleration"]
  NETP["NetworkPolicy - pp-control-plane and pp-data-plane deny all ingress and egress, then allow by name"]
  PRIO["PriorityClasses - pp-money-path 1000000, pp-platform-critical 900000, pp-control-plane 500000, pp-observability 400000, pp-batch 100000, pp-preview 0"]
  DENY["Kyverno in the prod overlay denies any Pod whose image is gateway-simulator"]

  PAPI -.-> SPREAD
  PORC -.-> SPREAD
  CPAPI -.-> TAINT
  WFW -.-> TAINT
  MIG --> CPAPI
  NSDATA -.-> NETP
  NSCTL -.-> NETP
  PAPI -.-> PRIO
  NSDATA -.-> DENY
```

## Diagram B — Ingress, mesh and autoscaling signals

```mermaid
flowchart LR
  R53["Route 53 latency and health based"]
  WAF["AWS WAF - managed rules, per-IP rate limit, bot control"]
  ALBP["ALB - money path host, payments and webhooks"]
  ALBC["ALB - admin host, control plane"]
  IGWP["Ingress pp-data-plane"]
  IGWC["Ingress pp-control-plane"]

  subgraph MESHZ["Service mesh - mTLS everywhere, SPIFFE identity per workload"]
    SPAPI["payment-api sidecar"]
    SPORC["payment-orchestrator sidecar"]
    SEGRESS["Egress gateway - the only route to third-party gateways"]
    OD["Outlier detection and per-destination connection pools"]
    RETRY["Mesh retries disabled on non-idempotent money routes"]
  end

  subgraph SCALE["Autoscaling signals"]
    H1["payment-api - requests per second and active connections"]
    H2["payment-orchestrator - in-flight gateway calls, not CPU"]
    H3["webhook-ingress - requests per second, spiky, 3x headroom"]
    H4["event-consumer - Kafka consumer lag via KEDA"]
    H5["workflow-worker - due-work backlog via KEDA"]
    CA["Cluster Autoscaler or Karpenter with 3x capacity headroom"]
  end

  R53 --> WAF
  WAF --> ALBP --> IGWP --> SPAPI
  WAF --> ALBC --> IGWC
  SPAPI --> SPORC --> OD --> SEGRESS
  SEGRESS -->|"static egress IPs for gateway allowlisting"| EXT["Stripe, Adyen, PayPal, KYC vendor"]
  SPORC -.-> RETRY
  H1 --> CA
  H2 --> CA
  H3 --> CA
  H4 --> CA
  H5 --> CA
```

## Legend and notes

- **Taints and tolerations, not just node selectors.** Data-plane nodes carry
  `pp.plane=data:NoSchedule` so nothing without an explicit toleration can land there — including
  a mis-labelled batch job. Control-plane and automation workloads have no matching toleration and
  therefore *cannot* contend with the money path (§5). The one deliberate exception is the OTel
  agent DaemonSet, which tolerates `pp.plane` with `operator: Exists` because a node it cannot
  land on is a node whose every pod is unobserved.
- **`gateway-simulator` never reaches production, and it is not one control that says so.** The
  root `Dockerfile` refuses to build the image without an explicit `ALLOW_TEST_SERVICE=1` build
  arg, the prod overlay carries a Kyverno policy denying any Pod whose image matches it, and the
  binary is `//go:build`-guarded out of production images (§5).
- **Every namespace denies all traffic before allowing any.** `pp-control-plane` and
  `pp-data-plane` each start from a deny-all `NetworkPolicy` in both directions and then name what
  may talk to what, so a new workload is unreachable until somebody writes down who may reach it.
- **`payment-orchestrator` scales on in-flight gateway calls, not CPU.** It spends most of its time
  waiting on a network call, so CPU is a lagging and misleading signal; a gateway slowing from
  200 ms to 3 s multiplies concurrency without moving CPU at all.
- **`outbox-relay` needs no leader election.** Multiple replicas poll with
  `FOR UPDATE SKIP LOCKED`, so they partition the work naturally. Fixed replicas rather than an
  HPA, because throughput is bounded by Postgres and Kafka, not by pod count (§13.4).
- **Mesh-level retries are disabled on non-idempotent money routes.** A mesh that transparently
  retries `POST /v1/payments` because it saw a 5xx would defeat the entire idempotency design —
  retries belong to the orchestrator, which owns the attempt model and the derived gateway key
  (§14.4).
- **A single egress gateway with static IPs.** Gateways and KYC vendors allowlist source IPs;
  routing all third-party egress through one mesh egress gateway makes that allowlist stable
  across pod churn and gives one place to enforce and observe outbound TLS.
- **Outlier detection in the mesh complements, it does not replace, the application circuit
  breaker.** The mesh ejects a bad endpoint; the application breaker owns the
  `(gateway, operation)` health FSM that routing consumes and that publishes
  `gateway.health_changed.v1` (§10).
- **PodDisruptionBudgets plus multi-AZ topology spread plus 3× capacity headroom** is what makes
  the §18 targets survivable: node loss is a brief latency blip, AZ loss is invisible if headroom
  holds (§24).
- **The silo tier is a contractual posture, not a manifest that exists today.** Baseline §16.1
  provides for a dedicated namespace and node group per siloed tenant; `deployments/k8s` ships the
  five pooled namespaces only, and the pooled tier is what the per-tenant concurrency bulkheads and
  rate limits exist to isolate.

## Related

- [Design baseline §5 deployables, §16 multi-tenancy, §18 non-functional targets, §24 failure catalog](../spec/00-design-baseline.md)
- [16 — AWS architecture](16-aws-architecture.md), [17 — Security architecture](17-security-architecture.md)
- [deployments/k8s](../../deployments/k8s), [helm](../../helm)
