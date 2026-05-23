{{- define "agent-task-manager.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "agent-task-manager.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "agent-task-manager.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "agent-task-manager.labels" -}}
app.kubernetes.io/name: {{ include "agent-task-manager.name" . }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "agent-task-manager.selectorLabels" -}}
app.kubernetes.io/name: {{ include "agent-task-manager.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "agent-task-manager.apiImage" -}}
{{- printf "%s/%s:%s" .Values.backend.api.image.registry .Values.backend.api.image.repository .Values.backend.api.image.tag -}}
{{- end -}}

{{- define "agent-task-manager.frontendImage" -}}
{{- printf "%s/%s:%s" .Values.frontend.image.registry .Values.frontend.image.repository .Values.frontend.image.tag -}}
{{- end -}}
