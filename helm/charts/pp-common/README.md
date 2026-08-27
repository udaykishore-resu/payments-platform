# pp-common

Library chart. Every per-deployable chart declares it as a dependency and calls
its helpers, so that a change to (say) the seccomp profile or the shutdown
arithmetic is a one-file change rather than eight.

| Helper | Emits | Notes |
|---|---|---|
| `pp-common.name` / `pp-common.fullname` / `pp-common.chart` | strings | standard naming |
| `pp-common.labels` | label block | includes `app` (the doc's NetworkPolicy/PDB selectors use it) *and* the `app.kubernetes.io/*` set |
| `pp-common.selectorLabels` | label block | immutable subset used by `spec.selector`; never contains version/digest |
| `pp-common.podSecurityContext` | pod `securityContext` | `runAsNonRoot`, uid/gid/fsGroup 65532, `seccompProfile: RuntimeDefault` |
| `pp-common.containerSecurityContext` | container `securityContext` | `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]` |
| `pp-common.probes` | startup/readiness/liveness | liveness path is deliberately different from readiness — see `deployment.md` §1.7 |
| `pp-common.env` | env list | `GOMEMLIMIT`/`GOMAXPROCS` from the downward API, downward-API pod identity, and **secret *references* only** |
| `pp-common.envFrom` | envFrom list | the non-secret ConfigMap |
| `pp-common.resources` | resources block | omits `cpu` under `limits` when `resources.limits.cpu` is null |
| `pp-common.topologySpreadConstraints` | list | hard zone spread, soft host spread |
| `pp-common.affinity` | affinity | preferred pod anti-affinity per node |
| `pp-common.nodeAssignment` | nodeSelector + tolerations | the taint per node group |
| `pp-common.lifecycle` | preStop | the sleep that closes the endpoint-removal race |
| `pp-common.volumes` / `pp-common.volumeMounts` | writable paths | `/tmp` is `medium: Memory`; the ESO secret is a read-only file mount |
| `pp-common.podSpec` | the whole pod spec | shared verbatim by `deployment.yaml` and `rollout.yaml`, so the two can never drift |
| `pp-common.secretGuard` | nothing, or `fail` | lint-time assertion that no literal credential appears in values |

## The secret guard

`pp-common.secretGuard` is invoked from every workload template. It walks
`.Values.env`, `.Values.config` and `.Values.externalSecrets.remoteRefs` and
calls `fail` if any *value* matches a known credential shape
(`sk_live_`, `AKIA…`, `-----BEGIN … PRIVATE KEY`, `xoxb-`, a long base64 run).
Because it runs during rendering, `helm lint` and `helm template` both trip on
it — a literal secret cannot reach a cluster without first breaking CI.

The same check exists as a repo-wide grep in `helm/scripts/check-no-literal-secrets.sh`
for the paths Helm never renders (README fragments, kustomize bases).
