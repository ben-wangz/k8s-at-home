{{/*
Chart-specific resource names.
*/}}
{{- define "frp.configMapName" -}}
{{- printf "%s-config" (include "common.names.fullname" .) -}}
{{- end -}}

{{/*
Reject unsupported scaling and credential literals in the ConfigMap content.
*/}}
{{- define "frp.validateValues" -}}
{{- if ne (int .Values.replicas) 1 -}}
{{- fail "frp supports exactly one frps replica because client sessions are process-local" -}}
{{- end -}}
{{- if empty .Values.config.content -}}
{{- fail "config.content must contain a non-sensitive frps configuration" -}}
{{- end -}}
{{- if empty .Values.secretMounts -}}
{{- fail "secretMounts must reference at least one existing Secret" -}}
{{- end -}}
{{- if regexMatch `(?mi)^[[:space:]]*([[:alnum:]_-]+[.])*(token|password|clientsecret)[[:space:]]*=` .Values.config.content -}}
{{- fail "config.content must not contain credential literals; reference files from secretMounts instead" -}}
{{- end -}}
{{- range .Values.secretMounts -}}
{{- if empty .existingSecret -}}
{{- fail (printf "secretMounts[%s].existingSecret is required" .name) -}}
{{- end -}}
{{- end -}}
{{- end -}}
