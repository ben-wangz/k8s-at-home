{{/*
Expand the name of the chart.
*/}}

{{/*
Generate the name of the generated config template.
*/}}
{{- define "clash.configTemplate.fullname" -}}
{{- printf "%s-config-template" (include "common.names.fullname" .) -}}
{{- end -}}

{{/*
Resolve the required subscription Secret.
*/}}
{{- define "clash.subscription.secretName" -}}
{{- required "subscription.existingSecret is required" .Values.subscription.existingSecret -}}
{{- end -}}
