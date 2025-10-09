{{/*
Expand the name of the chart.
*/}}
{{- define "podman-in-container.containerPvc.fullname" -}}
{{- include "common.names.fullname" . }}-container-pvc
{{- end -}}

{{/*
Create the name of the home PVC
*/}}
{{- define "podman-in-container.homePvc.fullname" -}}
{{- include "common.names.fullname" . }}-home-pvc
{{- end -}}

{{/*
Create the name of the SSH secret to use
*/}}
{{- define "podman-in-container.sshSecret.name" -}}
{{- if .Values.ssh.existingSecret }}
{{- .Values.ssh.existingSecret }}
{{- else }}
{{- include "common.names.fullname" . }}-ssh
{{- end }}
{{- end -}}
