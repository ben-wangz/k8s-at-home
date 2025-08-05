{{/*
Expand the name of the chart.
*/}}

{{/*
Generate full name of the Secret.
*/}}
{{- define "clash.secret.fullname" -}}
{{- printf "%s-config" (include "common.names.fullname" .) -}}
{{- end -}}