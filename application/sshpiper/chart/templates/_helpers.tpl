{{- define "sshpiper.pluginConfigMapName" -}}
{{- printf "%s-plugin" (include "common.names.fullname" .) -}}
{{- end -}}

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
