{{- define "fleet-rollout.name" -}}
{{- .Release.Name | trunc 40 | trimSuffix "-" -}}
{{- end -}}

{{- define "fleet-rollout.labels" -}}
app.kubernetes.io/name: fleet-rollout
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "fleet-rollout.selector" -}}
app.kubernetes.io/name: fleet-rollout
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "fleet-rollout.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag }}
{{- end -}}

{{- define "fleet-rollout.adminTokenSecret" -}}
{{- if .Values.adminToken.existingSecret -}}
{{ .Values.adminToken.existingSecret }}
{{- else -}}
{{ include "fleet-rollout.name" . }}-admin
{{- end -}}
{{- end -}}

{{- define "fleet-rollout.databaseSecret" -}}
{{- if .Values.database.existingSecret -}}
{{ .Values.database.existingSecret }}
{{- else -}}
{{ include "fleet-rollout.name" . }}-database
{{- end -}}
{{- end -}}

{{- define "fleet-rollout.securityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
runAsNonRoot: true
capabilities:
  drop: ["ALL"]
{{- end -}}
