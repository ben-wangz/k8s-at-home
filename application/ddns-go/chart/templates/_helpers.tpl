{{/*
Expand the name of the chart.
*/}}
{{- define "ddns-go.pvc.fullname" -}}
{{- include "common.names.fullname" . }}-pvc
{{- end -}}
