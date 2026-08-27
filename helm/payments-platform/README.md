# `payments-platform` umbrella chart

One subchart per deployable of `docs/spec/00-design-baseline.md` §5, plus
`platformctl`, over the `pp-common` library chart.

```
helm/
  payments-platform/        this chart: Chart.yaml, values.yaml, values-{dev,staging,prod}.yaml
  charts/pp-common/         library chart — labels, security context, probes, topology, podSpec
  charts/control-plane-api/
  charts/payment-api/
  charts/payment-orchestrator/
  charts/workflow-worker/
  charts/webhook-ingress/
  charts/outbox-relay/
  charts/event-consumer/
  charts/platformctl/       Job/CronJob chart: migrations and scheduled sweeps
  scripts/check-no-literal-secrets.sh
```

## Rendering

```bash
helm dependency build helm/payments-platform            # resolves the file:// deps
helm lint  helm/payments-platform -f helm/payments-platform/values-prod.yaml
helm template pp helm/payments-platform \
  -f helm/payments-platform/values-prod.yaml \
  --namespace pp-data-plane | kubeconform -strict -summary \
     -schema-location default \
     -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceAPIVersion}}/{{.ResourceKind}}.json'
```

The CRD schema location is required: `Rollout`, `AnalysisTemplate`,
`ScaledObject`, `ExternalSecret`, `ServiceMonitor` and `PrometheusRule` are all
custom resources, and `kubeconform` silently passes anything it has no schema
for. A run without `-schema-location` for CRDs validates roughly half of what
this chart emits and reports success, which is worse than not running it.

## Why the umbrella is not what production installs

Production does not `helm install payments-platform`. The ApplicationSet in
`deployments/argocd/` generates one Argo CD Application per *(environment,
service)*, each pointing at a single subchart. The reasons:

| | |
|---|---|
| **Blast radius of a bad chart** | A render error in `event-consumer` fails one Application, not the sync of `payment-api` |
| **Sync waves are per-Application** | Wave ordering across nine deployables in one release is a single dependency graph Argo re-derives on every sync; nine Applications each declaring their own wave is legible |
| **Rollback granularity** | Reverting one service is a revert of one Application's target revision |
| **Different namespaces** | The deployables span four namespaces; one release with `--namespace` set to one of them needs every subchart to hard-code the others |

The umbrella earns its place for local rendering, for the CI manifest-validation
stage, and for preview environments, where "the whole stack, one command, one
namespace, one TTL" is exactly what is wanted.

## Environments

| | dev | staging | prod |
|---|---|---|---|
| Replicas | 1 per service | 2–3 | the §0 table (12–120 for `payment-api`) |
| Canary | off — no traffic to measure against | **same steps as prod** | 1→10→25→50→100, ~40 min |
| Autoscaling | off | on, narrow range | on, full range |
| PDBs | off — a PDB over 1 replica blocks every drain, and dev is Spot-first | absolute counts | percentages (`payment-api` 75%) |
| CPU limits | set, generous | as prod | omitted on the latency-sensitive services |
| Secrets | fake values in the `pp-dev` Secrets Manager | sandbox credentials | live credentials, ≤ 90 d rotation |

## The things in here that look wrong and are not

**No CPU limit on `payment-api`, `payment-orchestrator`, `webhook-ingress`.**
Deliberate; the full argument is a comment block in each of those charts'
`values.yaml`. Short version: a CPU limit is CFS bandwidth control, and an
exhausted quota stops *every thread in the cgroup* for the remainder of a 100 ms
period. A Go service with `GOMAXPROCS=8` can burn its quota in ~12 ms and then
sit throttled for 88 ms — 35 % of a 250 ms p99 budget, spent doing nothing.
Requests (`cpu.shares`) already guarantee a floor under contention. Memory
always gets a limit, because memory is not compressible.

**`livenessProbe` and `readinessProbe` point at different paths.** `/livez`
answers "is this process wedged such that only a restart helps" and touches
nothing downstream. `/readyz` answers "should this pod get traffic right now" and
may check the DB writer, the DR write fence and config staleness. If `/livez`
checked the database, an Aurora failover would fail liveness on every pod at
once and the kubelet would kill the entire fleet — a transparent 60-second
failover turned into a ten-minute outage against cold pools. Readiness is
reversible; liveness is not.

**`preStop` sleeps before the process is even told to stop.** `SIGTERM` and
Endpoints removal are concurrent and unordered. The sleep lets kube-proxy and the
ALB target group converge *while the container is still serving normally*; the
alternative is connection resets that clients see as 502s on every deploy. It is
not a workaround for slow shutdown.

**Scale-up and scale-down are wildly asymmetric.** `payment-api` scales up with a
0-second stabilisation window and doubles every 30 s; it scales down only after
10 minutes of stability, 10 % at a time. Over-provisioning costs money;
under-provisioning costs payments.

**Nothing scales to zero, including in the KEDA charts.**
`minReplicaCount: 0` would mean the outbox stops draining when idle and the first
event after quiet hours waits for a cold start — and `pp_outbox_backlog` would be
both the trigger for the scale-up and the thing made worse by the delay.

**`maxUnavailable: 0` on every canary.** Capacity is never reduced to deploy, so
the stable ReplicaSet is still fully scaled when an abort happens: rollback is a
weight change taking ~10 s, not a re-deploy.

## Secrets

**No secret value appears anywhere in this chart tree, and two independent
mechanisms fail if one ever does.**

1. **Render-time.** `pp-common.secretGuard` is included from every workload
   template. It scans `.Values.env`, `.Values.config` and the ExternalSecret
   remote refs for credential *shapes* — `sk_live_…`, `AKIA…`,
   `-----BEGIN … PRIVATE KEY`, `xox[baprs]-…`, `ghp_…`, `AIza…`, a JWT header,
   `password: …` — and calls `fail`. Because it runs during rendering, both
   `helm lint` and `helm template` trip on it, so it cannot be skipped by anyone
   who can deploy.
2. **CI, manifest shapes.** `helm/scripts/check-no-literal-secrets.sh` greps
   `helm/` and `deployments/` for the Kubernetes and Helm shapes that mean
   "material is being delivered inline" regardless of whether the value looks
   like a credential: a literal `kind: Secret`, a `stringData:` block, an env var
   whose name matches the credential pattern carrying a `value:` rather than a
   reference, and a values key of the same shape assigned a literal. Wire it as a
   required check:

   ```yaml
   - name: no inline secret material in manifests
     run: ./helm/scripts/check-no-literal-secrets.sh helm deployments
   ```

   It deliberately does **not** re-implement credential-shape detection —
   `scripts/check-secrets.sh` already scans the whole repository for provider
   token prefixes, private-key blocks, JWTs, inline connection-string passwords
   and Luhn-valid digit runs, and it has a reviewed allowlist for legitimate test
   vectors. Two pattern lists would drift apart, and the second one would be the
   one without the allowlist.

What the pods actually get: an `ExternalSecret` per deployable resolves
`/{env}/{pathPrefix}/{key}` from AWS Secrets Manager using that deployable's own
IRSA role, and the resulting Secret is projected as **files** at
`/var/run/secrets/pp` with mode `0400`. The pod annotation checksum covers the
ConfigMap only and deliberately *not* the Secret: a 90-day credential rotation
with a 24-hour dual-run must not restart the fleet — the application re-reads the
file.

Env vars carry references (`secretref://prod/ten_…/stripe/api_key`), never
material.

## PriorityClasses

Defined in `deployments/k8s/base/priorityclasses.yaml` (cluster-scoped, sync wave
`-2`), assigned per deployable here. The order is the degradation ladder read
backwards — what must survive longest ranks highest:

| Class | Value | Assigned to |
|---|---|---|
| `pp-money-path` | 1000000 | `payment-api`, `payment-orchestrator`, `webhook-ingress`, `outbox-relay` |
| `pp-platform-critical` | 900000 | CoreDNS, kube-proxy, CNI, `otel-agent`, Karpenter |
| `pp-control-plane` | 500000 | `control-plane-api`, `workflow-worker`, `event-consumer` |
| `pp-observability` | 400000 | Prometheus, `otel-gateway`, fluent-bit |
| `pp-batch` | 100000 | `platformctl` CronJobs, certification runs, sweeps |
| `pp-preview` | 0 | preview environments (dev cluster only) |

`outbox-relay` sits at money-path priority although it is asynchronous, because
rung 8 of the ladder ("money-out only") still requires the outbox to drain —
refunds and webhook receipts are events, and an undrained outbox is money that
moved without anyone downstream knowing. `event-consumer` does not: projections
and the ledger can lag and catch up.

## Ports

| Port | Purpose | Who may reach it |
|---|---|---|
| `8443` | public HTTP, mTLS-terminated | `ingress-nginx` only |
| `9443` | internal gRPC (`payment-api` → `payment-orchestrator`) | `payment-api` only |
| `8081` | `/healthz`, `/readyz`, `/livez` | the kubelet only — never exposed at the edge |
| `9090` | `/metrics` | `pp-observability` only, ingress with no reciprocal egress |

All are above 1024, which is why `capabilities: drop: ["ALL"]` needs no
`NET_BIND_SERVICE` back.

`docs/deployment.md` §3.4 shows an ALB `servicePort: 8080` in the Rollout
fragment while `docs/security.md` §2.3 enumerates the allowed ingress flow as
`ingress-nginx → payment-api :8443`. The charts use **8443** and drive the ALB
`servicePort` from the same `.Values.ports.http`, so the NetworkPolicy and the
traffic-routing config cannot disagree; a mismatch there is an outage that only
appears at the first canary step.
