{{- define "sshpiper.allowedTargetsCSV" -}}
{{- join "," .Values.auth.allowedTargets -}}
{{- end -}}

{{- define "sshpiper.serverKeySecretName" -}}
{{- if .Values.serverKey.existingSecret -}}
{{- .Values.serverKey.existingSecret -}}
{{- else -}}
{{- default (printf "%s-server-key" (include "common.names.fullname" .)) .Values.serverKey.secretName -}}
{{- end -}}
{{- end -}}

{{- define "sshpiper.authorizedKeysSecretName" -}}
{{- if .Values.auth.authorizedKeys.existingSecret -}}
{{- .Values.auth.authorizedKeys.existingSecret -}}
{{- else -}}
{{- default (printf "%s-authorized-keys" (include "common.names.fullname" .)) .Values.auth.authorizedKeys.secretName -}}
{{- end -}}
{{- end -}}

{{- define "sshpiper.upstreamKeypairSecretName" -}}
{{- if .Values.auth.upstreamKeypair.existingSecret -}}
{{- .Values.auth.upstreamKeypair.existingSecret -}}
{{- else -}}
{{- default (printf "%s-upstream-keypair" (include "common.names.fullname" .)) .Values.auth.upstreamKeypair.secretName -}}
{{- end -}}
{{- end -}}

{{- define "sshpiper.validateValues" -}}
{{- if and (eq .Values.auth.mode "key") (and (not .Values.auth.authorizedKeys.existingSecret) (eq (len .Values.auth.authorizedKeys.values) 0)) -}}
{{- fail "auth.authorizedKeys.values or auth.authorizedKeys.existingSecret is required when auth.mode is key" -}}
{{- end -}}
{{- if and .Values.auth.authorizedKeys.existingSecret (gt (len .Values.auth.authorizedKeys.values) 0) -}}
{{- fail "auth.authorizedKeys.values and auth.authorizedKeys.existingSecret are mutually exclusive" -}}
{{- end -}}
{{- if and .Values.storage.knownHosts.enabled .Values.storage.knownHosts.selector (not (kindIs "map" .Values.storage.knownHosts.selector)) -}}
{{- fail "storage.knownHosts.selector must be a map when provided" -}}
{{- end -}}
{{- end -}}
