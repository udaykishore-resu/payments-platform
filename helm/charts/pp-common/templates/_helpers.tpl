{{/*
=============================================================================
pp-common — shared helpers for every payments-platform deployable.

Every helper is called with the *consuming* chart's context, e.g.

    {{- include "pp-common.podSpec" . | nindent 6 }}

so `.Values` inside these definitions is the subchart's values.yaml.
=============================================================================
*/}}

{{/* ---------------------------------------------------------------- naming */}}
{{- define "pp-common.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "pp-common.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- include "pp-common.name" . | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "pp-common.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* ---------------------------------------------------------------- labels */}}
{{/*
`app` is carried in addition to the app.kubernetes.io set because every
NetworkPolicy, PDB and topology-spread selector in docs/deployment.md and
docs/security.md is written against `app: <deployable>`. Dropping it would
silently unbind those policies.
*/}}
{{- define "pp-common.labels" -}}
app: {{ include "pp-common.name" . }}
app.kubernetes.io/name: {{ include "pp-common.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/component: {{ .Values.component | default "service" }}
app.kubernetes.io/part-of: payments-platform
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ include "pp-common.chart" . }}
pp.plane: {{ .Values.plane }}
pp.tenant-tier: {{ .Values.tenantTier | default "pooled" }}
{{- end -}}

{{/*
Selector labels are immutable for the life of the workload: `spec.selector` is
an immutable field on Deployment. Nothing version- or digest-derived may appear
here, or every image bump becomes a delete-and-recreate.
*/}}
{{- define "pp-common.selectorLabels" -}}
app: {{ include "pp-common.name" . }}
app.kubernetes.io/name: {{ include "pp-common.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "pp-common.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "pp-common.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* ----------------------------------------------------------------- image */}}
{{/*
Digest, never a tag (deployment.md §1.10). A Kyverno ClusterPolicy denies any
image reference containing ':' without '@sha256:', so a tag here does not fail
at render time — it fails at admission, which is worse. Fail here instead.
*/}}
{{- define "pp-common.image" -}}
{{- $reg := .Values.global.imageRegistry | default .Values.image.registry -}}
{{- if not .Values.image.digest -}}
{{- fail (printf "%s: .Values.image.digest is required — images are digest-pinned, never tagged (deployment.md §1.10)" (include "pp-common.name" .)) -}}
{{- end -}}
{{- if not (hasPrefix "sha256:" .Values.image.digest) -}}
{{- fail (printf "%s: .Values.image.digest must start with 'sha256:'" (include "pp-common.name" .)) -}}
{{- end -}}
{{- printf "%s/%s@%s" $reg .Values.image.repository .Values.image.digest -}}
{{- end -}}

{{/* -------------------------------------------------------- security context */}}
{{- define "pp-common.podSecurityContext" -}}
runAsNonRoot: {{ .Values.podSecurityContext.runAsNonRoot }}
runAsUser: {{ .Values.podSecurityContext.runAsUser }}
runAsGroup: {{ .Values.podSecurityContext.runAsGroup }}
fsGroup: {{ .Values.podSecurityContext.fsGroup }}
seccompProfile:
  type: {{ .Values.podSecurityContext.seccompProfile.type }}
{{- end -}}

{{- define "pp-common.containerSecurityContext" -}}
allowPrivilegeEscalation: {{ .Values.containerSecurityContext.allowPrivilegeEscalation }}
privileged: {{ .Values.containerSecurityContext.privileged }}
readOnlyRootFilesystem: {{ .Values.containerSecurityContext.readOnlyRootFilesystem }}
runAsNonRoot: {{ .Values.podSecurityContext.runAsNonRoot }}
capabilities:
  drop:
{{- range .Values.containerSecurityContext.capabilities.drop }}
    - {{ . }}
{{- end }}
{{- end -}}

{{/* ---------------------------------------------------------------- probes */}}
{{/*
Three probes, three questions, three failure actions (deployment.md §1.7).

  startupProbe   /healthz  "has it booted?"       -> restart, suppresses the others
  readinessProbe /readyz   "should it get traffic?" -> leave Endpoints (REVERSIBLE)
  livenessProbe  /livez    "is it wedged?"          -> kill the container (NOT reversible)

The paths differ on purpose. /livez checks only that the mux serves and the
internal watchdog counter advanced; it never touches Postgres, Redis, Kafka or a
gateway. If it did, an Aurora failover (<=60s) would fail liveness on every pod
simultaneously and the kubelet would kill the entire fleet, turning a transparent
60s failover into a 10-minute outage with cold pools and a thundering herd.
Downstream state belongs only in the reversible probe.
*/}}
{{- define "pp-common.probes" -}}
startupProbe:
  httpGet:
    path: {{ .Values.probes.startup.path }}
    port: health
  periodSeconds: {{ .Values.probes.startup.periodSeconds }}
  timeoutSeconds: {{ .Values.probes.startup.timeoutSeconds }}
  failureThreshold: {{ .Values.probes.startup.failureThreshold }}
readinessProbe:
  # MAY depend on downstreams: DB writer reachability (cached 5s), the DR write
  # fence, config snapshot age, drain state. Failing it sheds traffic and is
  # undone by the next successful probe.
  httpGet:
    path: {{ .Values.probes.readiness.path }}
    port: health
  periodSeconds: {{ .Values.probes.readiness.periodSeconds }}
  timeoutSeconds: {{ .Values.probes.readiness.timeoutSeconds }}
  successThreshold: {{ .Values.probes.readiness.successThreshold }}
  failureThreshold: {{ .Values.probes.readiness.failureThreshold }}
livenessProbe:
  # MUST NOT depend on any downstream. Different path from readiness precisely
  # so the distinction is visible in the manifest and cannot be "simplified".
  httpGet:
    path: {{ .Values.probes.liveness.path }}
    port: health
  periodSeconds: {{ .Values.probes.liveness.periodSeconds }}
  timeoutSeconds: {{ .Values.probes.liveness.timeoutSeconds }}
  failureThreshold: {{ .Values.probes.liveness.failureThreshold }}
{{- end -}}

{{/* ------------------------------------------------------------- shutdown */}}
{{/*
preStop exists because SIGTERM and Endpoints removal are concurrent and
unordered. Sleeping first lets kube-proxy / the ALB target group converge before
the process starts refusing connections; without it clients see resets as 502s.
It is NOT a workaround for slow shutdown.
*/}}
{{- define "pp-common.lifecycle" -}}
preStop:
  exec:
    command: ["/bin/sleep", "{{ .Values.lifecycle.preStopSleepSeconds }}"]
{{- end -}}

{{/* -------------------------------------------------------------- resources */}}
{{/*
Memory limits are always set: memory is not compressible, and a leak without a
limit takes down a node instead of one pod.

CPU limits are omitted on the latency-sensitive services. Setting
`resources.limits.cpu: null` in values.yaml is the deliberate, documented state
and this helper honours it by emitting no cpu key at all.
*/}}
{{- define "pp-common.resources" -}}
requests:
  cpu: {{ .Values.resources.requests.cpu | quote }}
  memory: {{ .Values.resources.requests.memory | quote }}
limits:
{{- if .Values.resources.limits.cpu }}
  cpu: {{ .Values.resources.limits.cpu | quote }}
{{- else }}
  # NO CPU LIMIT — deliberate. See values.yaml for the CFS reasoning.
{{- end }}
  memory: {{ .Values.resources.limits.memory | quote }}
{{- end -}}

{{/* ------------------------------------------------------------ scheduling */}}
{{- define "pp-common.nodeAssignment" -}}
nodeSelector:
  pp.plane: {{ .Values.nodeGroup }}
tolerations:
  - key: pp.plane
    operator: Equal
    value: {{ .Values.nodeGroup }}
    effect: NoSchedule
{{- with .Values.extraTolerations }}
{{ toYaml . | indent 2 }}
{{- end }}
{{- end -}}

{{/*
Zone spread is HARD (DoNotSchedule): AZ balance is a correctness property for
AZ-loss survival, and a fleet that drifted 8/2/2 silently voids it.
Host spread is SOFT (ScheduleAnyway): refusing to schedule during a node
shortage is worse than two pods sharing a host.
matchLabelKeys: [pod-template-hash] evaluates spread per revision, so a 10%
canary is itself zone-balanced — without it a 3-pod canary is routinely skewed
into one AZ and its analysis measures one AZ's behaviour.
*/}}
{{- define "pp-common.topologySpreadConstraints" -}}
- maxSkew: {{ .Values.topologySpread.zoneMaxSkew }}
  topologyKey: topology.kubernetes.io/zone
  whenUnsatisfiable: DoNotSchedule
  labelSelector:
    matchLabels:
{{ include "pp-common.selectorLabels" . | indent 6 }}
  matchLabelKeys:
    - pod-template-hash
- maxSkew: {{ .Values.topologySpread.hostMaxSkew }}
  topologyKey: kubernetes.io/hostname
  whenUnsatisfiable: ScheduleAnyway
  labelSelector:
    matchLabels:
{{ include "pp-common.selectorLabels" . | indent 6 }}
{{- end -}}

{{- define "pp-common.affinity" -}}
podAntiAffinity:
  preferredDuringSchedulingIgnoredDuringExecution:
    - weight: 100
      podAffinityTerm:
        topologyKey: kubernetes.io/hostname
        labelSelector:
          matchLabels:
{{ include "pp-common.selectorLabels" . | indent 12 }}
{{- end -}}

{{/* -------------------------------------------------------------------- env */}}
{{/*
No secret material here, ever. Env carries *references*
(`secretref://prod/ten_.../stripe/api_key`) and the directory where the External
Secrets Operator projects credential files. Admission policy rejects any pod
whose env var name matches (?i)(secret|password|token|api_?key|credential|
private_?key) with an inline value, and pp-common.secretGuard fails the render
before it gets that far.
*/}}
{{- define "pp-common.env" -}}
- name: PP_SERVICE
  value: {{ include "pp-common.name" . }}
- name: PP_ENV
  value: {{ .Values.global.environment | quote }}
- name: PP_REGION
  value: {{ .Values.global.region | quote }}
- name: PP_REGION_ACTIVE
  value: {{ .Values.global.regionActive | quote }}
- name: PP_HTTP_ADDR
  value: ":{{ .Values.ports.http | default 0 }}"
- name: PP_ADMIN_ADDR
  value: ":{{ .Values.ports.health }}"
- name: PP_METRICS_ADDR
  value: ":{{ .Values.ports.metrics }}"
- name: PP_SECRETS_DIR
  value: {{ .Values.externalSecrets.mountPath | quote }}
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  # Node-local agent via the downward API: a pod never blocks on a network hop
  # to a remote collector, and a gateway rollout does not drop spans.
  value: "http://$(NODE_IP):4317"
- name: OTEL_SERVICE_NAME
  value: {{ include "pp-common.name" . }}
- name: OTEL_RESOURCE_ATTRIBUTES
  value: "service.namespace=payments-platform,deployment.environment={{ .Values.global.environment }}"
- name: NODE_IP
  valueFrom:
    fieldRef:
      fieldPath: status.hostIP
- name: POD_NAME
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
- name: POD_NAMESPACE
  valueFrom:
    fieldRef:
      fieldPath: metadata.namespace
- name: POD_IP
  valueFrom:
    fieldRef:
      fieldPath: status.podIP
{{/*
GOMEMLIMIT from the memory *limit* (scaled to 90% in the entrypoint) so the Go
GC turns aggressive before the kernel OOM killer is involved.
GOMAXPROCS from the *request*, not the limit: with no CPU limit the runtime
would otherwise size its scheduler and GC workers for the node's full core
count — hardware this pod has no claim on.
*/}}
- name: GOMEMLIMIT
  valueFrom:
    resourceFieldRef:
      containerName: {{ include "pp-common.name" . }}
      resource: limits.memory
      divisor: "1"
- name: GOMAXPROCS
  valueFrom:
    resourceFieldRef:
      containerName: {{ include "pp-common.name" . }}
      resource: requests.cpu
      divisor: "1"
{{- range $k, $v := .Values.env }}
- name: {{ $k }}
  value: {{ $v | quote }}
{{- end }}
{{- end -}}

{{- define "pp-common.envFrom" -}}
- configMapRef:
    name: {{ include "pp-common.fullname" . }}-config
{{- end -}}

{{/* ---------------------------------------------------------------- volumes */}}
{{- define "pp-common.volumes" -}}
- name: tmp
  emptyDir:
    medium: Memory
    sizeLimit: {{ .Values.volumes.tmpSizeLimit }}
- name: cache
  emptyDir:
    sizeLimit: {{ .Values.volumes.cacheSizeLimit }}
{{- if .Values.externalSecrets.enabled }}
- name: pp-secrets
  secret:
    # Materialised by the External Secrets Operator from Secrets Manager.
    # Projected as FILES, never as env vars: the app reads them as Secret[T] and
    # picks up a rotation without a restart.
    secretName: {{ include "pp-common.fullname" . }}-secrets
    defaultMode: 0400
    optional: false
{{- end }}
{{- if .Values.serviceAccount.needsKubeAPI }}
- name: kube-api-token
  projected:
    sources:
      - serviceAccountToken:
          path: token
          audience: {{ .Values.serviceAccount.tokenAudience }}
          expirationSeconds: 3600
{{- end }}
{{- with .Values.extraVolumes }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{- define "pp-common.volumeMounts" -}}
- name: tmp
  mountPath: /tmp
- name: cache
  mountPath: /var/cache/pp
{{- if .Values.externalSecrets.enabled }}
- name: pp-secrets
  mountPath: {{ .Values.externalSecrets.mountPath }}
  readOnly: true
{{- end }}
{{- if .Values.serviceAccount.needsKubeAPI }}
- name: kube-api-token
  mountPath: /var/run/secrets/kubernetes.io/serviceaccount
  readOnly: true
{{- end }}
{{- with .Values.extraVolumeMounts }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/* ----------------------------------------------------------- ports block */}}
{{- define "pp-common.containerPorts" -}}
{{- if .Values.ports.http }}
- name: http
  containerPort: {{ .Values.ports.http }}
  protocol: TCP
{{- end }}
{{- if .Values.ports.grpc }}
- name: grpc
  containerPort: {{ .Values.ports.grpc }}
  protocol: TCP
{{- end }}
- name: health
  containerPort: {{ .Values.ports.health }}
  protocol: TCP
- name: metrics
  containerPort: {{ .Values.ports.metrics }}
  protocol: TCP
{{- end -}}

{{/* --------------------------------------------------------------- pod spec */}}
{{/*
One definition, shared verbatim by deployment.yaml and rollout.yaml. The whole
point of the Rollout/Deployment values flag is that the pod is identical either
way; duplicating the pod spec across two files is how they drift.
*/}}
{{- define "pp-common.podSpec" -}}
{{- include "pp-common.secretGuard" . -}}
serviceAccountName: {{ include "pp-common.serviceAccountName" . }}
{{/* Mounting an API token into every pod is a lateral-movement gift. It is on
     only where the Kubernetes API is genuinely used. */}}
automountServiceAccountToken: {{ .Values.serviceAccount.automountServiceAccountToken }}
{{- if .Values.priorityClassName }}
priorityClassName: {{ .Values.priorityClassName }}
{{- end }}
terminationGracePeriodSeconds: {{ .Values.terminationGracePeriodSeconds }}
securityContext:
{{- include "pp-common.podSecurityContext" . | nindent 2 }}
{{- include "pp-common.nodeAssignment" . }}
topologySpreadConstraints:
{{- include "pp-common.topologySpreadConstraints" . | nindent 2 }}
affinity:
{{- include "pp-common.affinity" . | nindent 2 }}
containers:
  - name: {{ include "pp-common.name" . }}
    image: {{ include "pp-common.image" . }}
    imagePullPolicy: {{ .Values.image.pullPolicy }}
    {{- with .Values.args }}
    args:
{{ toYaml . | indent 6 }}
    {{- end }}
    securityContext:
{{- include "pp-common.containerSecurityContext" . | nindent 6 }}
    ports:
{{- include "pp-common.containerPorts" . | nindent 6 }}
    env:
{{- include "pp-common.env" . | nindent 6 }}
    envFrom:
{{- include "pp-common.envFrom" . | nindent 6 }}
    resources:
{{- include "pp-common.resources" . | nindent 6 }}
{{- include "pp-common.probes" . | nindent 4 }}
    lifecycle:
{{- include "pp-common.lifecycle" . | nindent 6 }}
    volumeMounts:
{{- include "pp-common.volumeMounts" . | nindent 6 }}
volumes:
{{- include "pp-common.volumes" . | nindent 2 }}
{{- end -}}

{{/* --------------------------------------------------------- pod annotations */}}
{{- define "pp-common.podAnnotations" -}}
{{/* Roll the pods when non-secret config changes. Deliberately NOT keyed on the
     ESO-managed Secret: credential rotation must NOT restart the fleet — the
     app re-reads the projected file. */}}
checksum/config: {{ toYaml .Values.config | sha256sum }}
{{- with .Values.podAnnotations }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/* -------------------------------------------------------- the secret guard */}}
{{/*
Render-time assertion: no literal credential may appear in values. Runs on every
`helm lint`, `helm template` and `helm install`, so the check cannot be skipped
by anyone who can deploy.

Patterns are credential *shapes*, not a wordlist, so a novel variable name does
not evade it. The companion repo-wide grep lives in
helm/scripts/check-no-literal-secrets.sh for files Helm never renders.
*/}}
{{- define "pp-common.secretGuard" -}}
{{- $patterns := list
      "sk_live_[0-9A-Za-z]"
      "sk_test_[0-9A-Za-z]"
      "rk_live_[0-9A-Za-z]"
      "AKIA[0-9A-Z]{16}"
      "ASIA[0-9A-Z]{16}"
      "-----BEGIN [A-Z ]*PRIVATE KEY-----"
      "xox[baprs]-[0-9A-Za-z-]{10}"
      "ghp_[0-9A-Za-z]{20}"
      "AIza[0-9A-Za-z_-]{20}"
      "eyJhbGciOi[0-9A-Za-z_-]{10}"
      "(?i)password\\s*[:=]\\s*[^ $\\{]"
-}}
{{- $svc := include "pp-common.name" . -}}
{{- $haystack := list -}}
{{- range $k, $v := (.Values.env | default dict) -}}
{{-   $haystack = append $haystack (printf "%v" $v) -}}
{{- end -}}
{{- range $k, $v := (.Values.config | default dict) -}}
{{-   $haystack = append $haystack (printf "%v" $v) -}}
{{- end -}}
{{- range $ref := (.Values.externalSecrets.remoteRefs | default list) -}}
{{-   $haystack = append $haystack (printf "%v" $ref.remoteKey) -}}
{{- end -}}
{{- range $candidate := $haystack -}}
{{-   range $p := $patterns -}}
{{-     if regexMatch $p $candidate -}}
{{-       fail (printf "%s: a literal credential (pattern %q) appears in values. Secrets live in AWS Secrets Manager and reach the pod as an ExternalSecret-projected file; values may carry only a secretref:// reference. See docs/security.md §5.2." $svc $p) -}}
{{-     end -}}
{{-   end -}}
{{- end -}}
{{- end -}}

{{/* ------------------------------------------------- external secret helper */}}
{{/*
Builds the ExternalSecret data[] block. The Secrets Manager path is composed
here rather than written out per environment, so a chart can never point staging
at a production secret path by copy-paste:

    /{env}/{pathPrefix}/{remoteKey}
    e.g. /prod/platform/payment-orchestrator/gateway-credentials-ref

Tenant-scoped material uses the full scheme from security.md §5.1,
/{env}/{tenant_id}/{merchant_id}/{gateway}/{purpose}, resolved at use time by the
application rather than mounted per pod — a pod cannot hold every tenant's
credentials.
*/}}
{{- define "pp-common.externalSecretRefs" -}}
{{- $env := .Values.global.environment -}}
{{- $prefix := .Values.externalSecrets.pathPrefix -}}
{{- range $ref := .Values.externalSecrets.remoteRefs }}
- secretKey: {{ $ref.secretKey }}
  remoteRef:
    key: {{ printf "/%s/%s/%s" $env $prefix $ref.remoteKey | quote }}
    {{- if $ref.property }}
    property: {{ $ref.property }}
    {{- end }}
    {{- if $ref.version }}
    version: {{ $ref.version }}
    {{- end }}
    conversionStrategy: Default
    decodingStrategy: None
{{- end }}
{{- end -}}
