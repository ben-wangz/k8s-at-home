{{/*
Expand the name of the chart.
*/}}

{{- define "tabminal.pvc.fullname" -}}
{{- include "common.names.fullname" . }}-pvc
{{- end -}}

{{- define "tabminal.secret.fullname" -}}
{{- include "common.names.fullname" . }}-secret
{{- end -}}

{{- define "tabminal.secret.name" -}}
{{- if .Values.tabminal.existingSecret -}}
{{- .Values.tabminal.existingSecret -}}
{{- else -}}
{{- include "tabminal.secret.fullname" . -}}
{{- end -}}
{{- end -}}
