{{/*
Expand the name of the chart.
*/}}
{{- define "codespace.containerPvc.fullname" -}}
{{- include "common.names.fullname" . }}-container-pvc
{{- end -}}

{{/*
Create the name of the home PVC
*/}}
{{- define "codespace.homePvc.fullname" -}}
{{- include "common.names.fullname" . }}-home-pvc
{{- end -}}

{{/*
Create the name of the SSH secret to use
*/}}
{{- define "codespace.sshSecret.name" -}}
{{- if .Values.ssh.existingSecret }}
{{- .Values.ssh.existingSecret }}
{{- else }}
{{- include "common.names.fullname" . }}-ssh
{{- end }}
{{- end -}}

{{/*
Create the name of the SSH service
*/}}
{{- define "codespace.sshService.name" -}}
{{- include "common.names.fullname" . }}-ssh
{{- end -}}

{{/*
Create the name of the service account to use
*/}}
{{- define "codespace.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "common.names.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}
