{{/*
Expand the name of the chart.
*/}}

{{/*
Generate full name of the PVC.
*/}}
{{- define "aria2.pvc.fullname" -}}
{{- printf "%s-downloads-pvc" (include "aria2.fullname" .) -}}
{{- end -}}