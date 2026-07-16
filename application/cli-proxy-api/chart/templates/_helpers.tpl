{{- define "cli-proxy-api.port" -}}
8317
{{- end -}}

{{- define "cli-proxy-api.credentialsSecretName" -}}
{{- required "credentials.existingSecret is required" (include "common.tplvalues.render" (dict "value" .Values.credentials.existingSecret "context" $)) -}}
{{- end -}}

{{- define "cli-proxy-api.configMapName" -}}
{{- printf "%s-config" (include "common.names.fullname" .) -}}
{{- end -}}

{{- define "cli-proxy-api.authPvcName" -}}
{{- printf "%s-auth" (include "common.names.fullname" .) -}}
{{- end -}}

{{- define "cli-proxy-api.authClaimName" -}}
{{- if .Values.persistence.auth.existingClaim -}}
{{- include "common.tplvalues.render" (dict "value" .Values.persistence.auth.existingClaim "context" $) -}}
{{- else -}}
{{- include "cli-proxy-api.authPvcName" . -}}
{{- end -}}
{{- end -}}

{{- define "cli-proxy-api.validateValues" -}}
{{- if ne (int .Values.replicas) 1 -}}
{{- fail "replicas must be 1 when using the file-backed OAuth store" -}}
{{- end -}}
{{- if not .Values.credentials.existingSecret -}}
{{- fail "credentials.existingSecret is required" -}}
{{- end -}}
{{- if not .Values.credentials.secretKeys.apiKeys -}}
{{- fail "credentials.secretKeys.apiKeys is required" -}}
{{- end -}}
{{- if not (kindIs "map" .Values.config.extra) -}}
{{- fail "config.extra must be a map" -}}
{{- end -}}
{{- $reservedKeys := list "host" "port" "tls" "auth-dir" "api-keys" "remote-management" -}}
{{- range $key, $_ := .Values.config.extra -}}
{{- if or (has $key $reservedKeys) (contains "api-key" (lower $key)) -}}
{{- fail (printf "config.extra must not override reserved or sensitive key %q" $key) -}}
{{- end -}}
{{- end -}}
{{- if and .Values.persistence.auth.existingClaim (not .Values.persistence.auth.enabled) -}}
{{- fail "persistence.auth.existingClaim requires persistence.auth.enabled=true" -}}
{{- end -}}
{{- if and .Values.persistence.auth.selector (not (kindIs "map" .Values.persistence.auth.selector)) -}}
{{- fail "persistence.auth.selector must be a map when provided" -}}
{{- end -}}
{{- if and .Values.persistence.auth.labels (not (kindIs "map" .Values.persistence.auth.labels)) -}}
{{- fail "persistence.auth.labels must be a map when provided" -}}
{{- end -}}
{{- if not (has .Values.config.routing.strategy (list "round-robin" "fill-first")) -}}
{{- fail "config.routing.strategy must be round-robin or fill-first" -}}
{{- end -}}
{{- end -}}
