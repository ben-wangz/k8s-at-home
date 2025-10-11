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
OpenVSCode only accepts alphanumeric characters (0-9, a-z, A-Z) and hyphens
SECURITY NOTE: Auto-generated token uses deterministic SHA256 hash for stability
in ArgoCD/GitOps environments. For production use, consider setting:
  - openvscode.connection.token.raw with a strong random password, OR
  - openvscode.connection.token.existingSecret to reference a manually created secret
*/}}
{{- define "podman-in-container.openvscodSecret.token" -}}
{{- if .Values.openvscode.connection.token.raw }}
{{- .Values.openvscode.connection.token.raw }}
{{- else }}
{{- $seed := printf "%s-%s-openvscode-token" (include "common.names.fullname" .) .Release.Namespace }}
{{- sha256sum $seed | trunc 32 }}
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
