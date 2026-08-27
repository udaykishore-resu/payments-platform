{{/*
The shared pod spec for every platformctl Job and CronJob.

Called as: {{ include "platformctl.jobPodSpec" (dict "ctx" $ "args" (list ...)) }}
so the argv is the only thing that varies between the migration, the smoke test
and the sweeps. Jobs carry no probes: there is no traffic to gate, and no wedged
state a container restart would fix. activeDeadlineSeconds is the Job-shaped
equivalent of a liveness probe.
*/}}
{{- define "platformctl.jobPodSpec" -}}
{{- $ := .ctx -}}
{{- include "pp-common.secretGuard" $ }}
restartPolicy: Never
serviceAccountName: {{ include "pp-common.serviceAccountName" $ }}
automountServiceAccountToken: {{ $.Values.serviceAccount.automountServiceAccountToken }}
priorityClassName: {{ $.Values.priorityClassName }}
terminationGracePeriodSeconds: {{ $.Values.terminationGracePeriodSeconds }}
securityContext:
{{- include "pp-common.podSecurityContext" $ | nindent 2 }}
{{- include "pp-common.nodeAssignment" $ }}
containers:
  - name: platformctl
    image: {{ include "pp-common.image" $ }}
    imagePullPolicy: {{ $.Values.image.pullPolicy }}
    args:
{{- range .args }}
      - {{ . | quote }}
{{- end }}
    securityContext:
{{- include "pp-common.containerSecurityContext" $ | nindent 6 }}
    env:
{{- include "pp-common.env" $ | nindent 6 }}
    envFrom:
{{- include "pp-common.envFrom" $ | nindent 6 }}
    resources:
{{- include "pp-common.resources" $ | nindent 6 }}
    volumeMounts:
{{- include "pp-common.volumeMounts" $ | nindent 6 }}
volumes:
{{- include "pp-common.volumes" $ | nindent 2 }}
{{- end -}}
