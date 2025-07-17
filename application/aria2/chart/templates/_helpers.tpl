{{/*
Expand the name of the chart.
*/}}

{{/*
Generate full name of the PVC.
*/}}
{{- define "aria2.pvc.fullname" -}}
{{- printf "%s-downloads" (include "common.names.fullname" .) -}}
{{- end -}}

{{/*
Generate full name of the Service.
*/}}
{{- define "aria2.service.fullname" -}}
{{- include "common.names.fullname" . -}}
{{- end -}}}}