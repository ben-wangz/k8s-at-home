{{- define "sshpiper.pluginConfigMapName" -}}
{{- printf "%s-plugin" (include "common.names.fullname" .) -}}
{{- end -}}

{{- define "sshpiper.allowedTargetsCSV" -}}
{{- join "," .Values.config.allowedTargets -}}
{{- end -}}
