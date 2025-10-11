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

{{/*
Create the name of the OpenVSCode secret to use
*/}}
{{- define "podman-in-container.openvscodSecret.name" -}}
{{- if .Values.openvscode.connection.token.existingSecret }}
{{- .Values.openvscode.connection.token.existingSecret }}
{{- else }}
{{- include "common.names.fullname" . }}-openvscode
{{- end }}
{{- end -}}

{{/*
Generate OpenVSCode connection token
*/}}
{{- define "podman-in-container.openvscodSecret.token" -}}
{{- if .Values.openvscode.connection.token.raw }}
{{- .Values.openvscode.connection.token.raw }}
{{- else }}
{{- derivePassword 1 "long" (include "common.names.fullname" .) "openvscode-token" .Release.Namespace }}
{{- end }}
{{- end -}}

{{/*
Create the name of the SSH service
*/}}
{{- define "podman-in-container.sshService.name" -}}
{{- include "common.names.fullname" . }}-ssh
{{- end -}}

{{/*
Create the name of the OpenVSCode service
*/}}
{{- define "podman-in-container.openvscodService.name" -}}
{{- include "common.names.fullname" . }}-openvscode
{{- end -}}
