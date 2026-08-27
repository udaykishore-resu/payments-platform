# Argo CD

```
appproject.yaml        one AppProject per environment
applicationset.yaml    pp-services (env × service), pp-platform-base, pp-observability
kustomization.yaml
```

## Sync waves

Ordering within a sync, lowest first. A wave completes — every resource in it
Healthy — before the next begins.

| Wave | Contents | Why here |
|---|---|---|
| `-2` | Namespaces, ResourceQuotas, LimitRanges, PriorityClasses, default-deny NetworkPolicies, ServiceAccounts, RBAC | Must exist before anything references them. A pod naming a PriorityClass that does not exist is rejected at admission; a pod admitted before the namespace carries its `pod-security.kubernetes.io/enforce` label bypasses `restricted` entirely |
| `-1` | ExternalSecrets, ConfigMaps, CRDs | Workloads mount these |
| `0` | **PreSync hook: `platformctl migrate up`** | See below |
| `1` | Services, ServiceMonitors, PodMonitors, AnalysisTemplates | Endpoints ready for the workloads that follow; the AnalysisTemplate must exist before the Rollout that references it |
| `2` | `payment-api`, `payment-orchestrator`, `webhook-ingress`, `control-plane-api` | The request-serving tier |
| `3` | `outbox-relay`, `event-consumer`, `workflow-worker` | The async tier, after its producers are healthy |
| `4` | HPAs, ScaledObjects, PDBs, CronJobs | Applied after the workloads exist, so an autoscaler never targets a workload that is not yet there |
| `5` | **PostSync hook: smoke tests** | Verifies the deployment; a failure fails the sync |

At the ApplicationSet level the same order is enforced a second time by
`strategy.rollingSync`, which will not start the wave-2 Applications until the
wave-0 migration Application is Healthy. Two mechanisms, because sync-wave
annotations order resources *within* an Application and say nothing about the
order *between* Applications — and migrations are a different Application from
the workloads that depend on them.

### Migrations run PreSync, not PostSync, and never as an init container

PreSync means: once per sync, in one Job, before any new pod starts. An init
container would run the migration once per pod — N concurrent migrations racing
for the advisory lock, with N−1 blocking pod startup while they wait for it.

`backoffLimit: 0`, because a partially-applied migration retried automatically is
how a bad deploy becomes a data incident. A failed migration blocks the sync,
which means the new pods never start and the previous version keeps serving:
**a failed migration is a non-event for traffic**, and it pages a human, which is
the correct outcome.

`--expect-version` comes from the release manifest. If the database is at an
unexpected version the job fails before applying anything, catching a re-ordered
or skipped migration.

## `prune` and `selfHeal`, per environment

| | prod | staging | dev |
|---|---|---|---|
| `pp-services` prune | **true** | true | **false** |
| `pp-services` selfHeal | **true** | true | **false** |
| `pp-platform-base` prune | **false** | false | false |
| `pp-platform-base` selfHeal | true | true | false |
| `pp-observability` prune | true | true | true |

**`selfHeal: true` in production is a decision with teeth.** A `kubectl edit` made
during an incident is reverted within minutes. That is intentional: undocumented
drift is how the DR premise — "re-apply Git and you have the cluster back" —
quietly becomes false, and it becomes false silently, discovered during the
restore drill or the real failover. Emergency changes go through a break-glass PR
with a one-approver rule, which takes about ninety seconds and leaves a record
that outlives the incident channel.

**`selfHeal: false` in dev** for the opposite reason. Developers have admin in dev
and debug by editing live objects. A controller that reverts an edit every three
minutes makes the cluster unusable for the one thing dev exists for. The cost is
drift, which is acceptable precisely because dev's state is disposable — nothing
in dev is a source of truth for anything.

**`prune: false` on `pp-platform-base` everywhere, including production.** That
Application owns Namespaces, PriorityClasses, ResourceQuotas and the default-deny
NetworkPolicies, and each of those prunes catastrophically:

- pruning a Namespace cascades to everything inside it;
- pruning a PriorityClass leaves every pod that names it unschedulable at the
  next reschedule — which surfaces as a mysterious inability to recover from an
  unrelated node failure, hours later;
- pruning a default-deny NetworkPolicy silently *opens* the data plane, and
  nothing fails, which is the worst failure mode available.

None of those is recoverable by "sync again". Removing one of these resources is
a deliberate two-step: merge the removal, then delete it by hand with a named
ticket. The `PruneLast=true` sync option is a second belt: whatever pruning does
happen happens after everything else has converged.

**`prune: true` on observability**, because a deleted alert rule must actually
stop firing and a deleted dashboard must actually disappear. An orphaned alert
that nobody owns is worse than no alert: it pages, and the runbook link 404s.

## `ignoreDifferences` on `/spec/replicas`

The HPA and KEDA own the live replica count. Without this stanza every scale
event marks the Application `OutOfSync`, `selfHeal` writes the Git value back,
the autoscaler scales again, and the cluster oscillates — the classic
GitOps-versus-HPA flap, which presents as "the deploy never goes Healthy" and is
usually misdiagnosed as an autoscaler bug.

## Two repositories

The chart lives in the application repo; the values live in the GitOps repo, and
the Application references both via a `$values` multi-source ref.

A production deploy is therefore a commit to the GitOps repo that changes one
image digest — reviewable in ten seconds. The application repo's history stays
code rather than deployment noise, and CI's write access to production is
confined to *"may open a PR that changes an image digest in
`environments/prod/image-versions-*.yaml`"*. CI holds no cluster credentials at
all.

## Sync windows

Production denies automated sync outside business hours and from Friday noon.
Not because deploying is dangerous, but because *diagnosing* a bad deploy at
02:00 on a Saturday with one person awake is. `manualSync: true` leaves the
override available; using it is audited.
