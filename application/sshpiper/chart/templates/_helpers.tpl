{{- define "sshpiper.allowedTargetsCSV" -}}
{{- join "," .Values.config.allowedTargets -}}
{{- end -}}

{{- define "sshpiper.serverKeySecretName" -}}
{{- if .Values.serverKey.existingSecret -}}
{{- .Values.serverKey.existingSecret -}}
{{- else -}}
{{- default (printf "%s-server-key" (include "common.names.fullname" .)) .Values.serverKey.secretName -}}
{{- end -}}
{{- end -}}

{{- define "sshpiper.validateValues" -}}
{{- if and (eq .Values.config.authMode "key") (not .Values.authFiles.existingSecret) -}}
{{- fail "authFiles.existingSecret is required when config.authMode is key" -}}
{{- end -}}
{{- if and (eq .Values.storage.knownHosts.type "pvc") (not .Values.storage.knownHosts.existingClaim) -}}
{{- fail "storage.knownHosts.existingClaim is required when storage.knownHosts.type is pvc" -}}
{{- end -}}
{{- end -}}
