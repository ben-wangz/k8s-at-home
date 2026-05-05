{{/* Server deployment name */}}
{{- define "chromium-bridge.server.fullname" -}}
{{- printf "%s-server" (include "common.names.fullname" .) -}}
{{- end -}}

{{/* noVNC deployment name */}}
{{- define "chromium-bridge.novnc.fullname" -}}
{{- printf "%s-novnc" (include "common.names.fullname" .) -}}
{{- end -}}

{{/* Server service name */}}
{{- define "chromium-bridge.server.serviceName" -}}
{{- include "chromium-bridge.server.fullname" . -}}
{{- end -}}

{{/* noVNC service name */}}
{{- define "chromium-bridge.novnc.serviceName" -}}
{{- include "chromium-bridge.novnc.fullname" . -}}
{{- end -}}

{{/* noVNC target host */}}
{{- define "chromium-bridge.novnc.targetHost" -}}
{{- if .Values.novnc.target.host -}}
{{- .Values.novnc.target.host -}}
{{- else -}}
{{- include "chromium-bridge.server.serviceName" . -}}
{{- end -}}
{{- end -}}
