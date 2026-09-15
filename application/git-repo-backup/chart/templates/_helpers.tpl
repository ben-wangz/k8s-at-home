{{/* Expand the chart name. */}}
{{- define "git-repo-backup.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fullname without the length suffix logic. */}}
{{- define "git-repo-backup.rawfullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "git-repo-backup.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
CronJob name: at most 52 characters so job/controller-derived names stay
within the 63-character Kubernetes limit; truncation appends a stable hash
to avoid collisions between equal-length prefixes.
*/}}
{{- define "git-repo-backup.cronjob.name" -}}
{{- $name := include "git-repo-backup.rawfullname" . -}}
{{- if gt (len $name) 52 -}}
{{- printf "%s-%s" (substr 0 41 $name) (substr 0 10 (sha256sum $name)) -}}
{{- else -}}
{{- $name -}}
{{- end -}}
{{- end -}}

{{/* Standard labels; identity labels cannot be overridden. */}}
{{- define "git-repo-backup.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "git-repo-backup.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
{{- end -}}

{{- define "git-repo-backup.selectorLabels" -}}
app.kubernetes.io/name: {{ include "git-repo-backup.name" . | quote }}
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end -}}

{{- define "git-repo-backup.configMapName" -}}
{{- printf "%s-config" (include "git-repo-backup.rawfullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "git-repo-backup.repositoriesConfigMapName" -}}
{{- printf "%s-repositories" (include "git-repo-backup.rawfullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "git-repo-backup.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "git-repo-backup.rawfullname" .) .Values.serviceAccount.name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "git-repo-backup.image" -}}
{{- $registry := .Values.image.registry -}}
{{- $repository := .Values.image.repository -}}
{{- $image := $repository -}}
{{- if $registry -}}
{{- $image = printf "%s/%s" $registry $repository -}}
{{- end -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" $image .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $image .Values.image.tag -}}
{{- end -}}
{{- end -}}

{{/*
cacheRoot: a separate cache claim mounts at /cache; without one, the local
backend reuses the backup volume through <mountPath>/cache (the only
sanctioned nesting, a sibling of backups/ and .staging/).
*/}}
{{- define "git-repo-backup.cacheRoot" -}}
{{- if .Values.cache.existingClaim -}}
/cache
{{- else -}}
{{- printf "%s/cache" .Values.storage.local.mountPath -}}
{{- end -}}
{{- end -}}

{{- define "git-repo-backup.repositoryListSource" -}}
{{- if .Values.repositoriesConfigMap.existingConfigMap -}}
{{- .Values.repositoriesConfigMap.existingConfigMap -}}
{{- else -}}
{{- include "git-repo-backup.repositoriesConfigMapName" . -}}
{{- end -}}
{{- end -}}

{{- define "git-repo-backup.repositoryListKey" -}}
{{- default "repositories.yaml" .Values.repositoriesConfigMap.key -}}
{{- end -}}

{{/*
durationSeconds converts a Go duration string (e.g. 5h30m) to seconds.
Render-time duration checks are advisory: the binary parses strictly.
*/}}
{{- define "git-repo-backup.durationSeconds" -}}
{{- $total := 0.0 -}}
{{- $rest := . -}}
{{- $units := list (list "ns" 0.000000001) (list "us" 0.000001) (list "µs" 0.000001) (list "ms" 0.001) (list "s" 1.0) (list "m" 60.0) (list "h" 3600.0) -}}
{{- range $u := $units -}}
{{- $pat := printf "([0-9]*\\.?[0-9]+)%s" (index $u 0) -}}
{{- range $m := regexFindAll $pat $rest -1 -}}
{{- $total = add $total (mul (index $u 1) ((regexFind "[0-9]*\\.?[0-9]+" $m) | float64)) -}}
{{- end -}}
{{- $rest = regexReplaceAll $pat $rest "" -}}
{{- end -}}
{{- if ne (len $rest) 0 -}}
{{- fail (printf "invalid Go duration value %q (use syntax such as 24h, 30m, 10s)" .) -}}
{{- end -}}
{{- $total -}}
{{- end -}}
