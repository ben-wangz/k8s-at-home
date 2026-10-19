{{- define "git-repo-backup.fullname" -}}
{{- include "common.names.fullname" . -}}
{{- end -}}

{{/* Preserve retained PVC names for releases whose name contains the chart name. */}}
{{- define "git-repo-backup.persistentNamePrefix" -}}
{{- if .Values.fullnameOverride -}}
{{- include "common.names.fullname" . -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "common.names.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
CronJob name: at most 52 characters so job/controller-derived names stay
within the 63-character Kubernetes limit; truncation appends a stable hash
to avoid collisions between equal-length prefixes.
*/}}
{{- define "git-repo-backup.cronjob.name" -}}
{{- $name := include "git-repo-backup.fullname" . -}}
{{- if gt (len $name) 52 -}}
{{- printf "%s-%s" (substr 0 41 $name) (substr 0 10 (sha256sum $name)) -}}
{{- else -}}
{{- $name -}}
{{- end -}}
{{- end -}}

{{- define "git-repo-backup.labels" -}}
{{- include "common.labels.standard" (dict "customLabels" .Values.commonLabels "context" .) -}}
{{- end -}}

{{- define "git-repo-backup.selectorLabels" -}}
{{- $labels := mergeOverwrite (deepCopy .Values.commonLabels) .Values.podLabels -}}
{{- include "common.labels.matchLabels" (dict "customLabels" $labels "context" .) -}}
{{- end -}}

{{- define "git-repo-backup.podLabels" -}}
{{- $labels := mergeOverwrite (deepCopy .Values.commonLabels) .Values.podLabels -}}
{{- include "common.labels.standard" (dict "customLabels" $labels "context" .) -}}
{{- end -}}

{{- define "git-repo-backup.configMapName" -}}
{{- printf "%s-config" (include "git-repo-backup.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "git-repo-backup.repositoriesConfigMapName" -}}
{{- printf "%s-repositories" (include "git-repo-backup.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "git-repo-backup.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "git-repo-backup.fullname" .) .Values.serviceAccount.name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "git-repo-backup.image" -}}
{{- include "common.images.image" (dict "imageRoot" .Values.image "global" .Values.global "chart" .Chart) -}}
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

{{- define "git-repo-backup.knownHostsFile" -}}
{{- $policy := default "accept-new" .Values.ssh.hostKeyPolicy -}}
{{- if eq $policy "none" -}}
/dev/null
{{- else if eq $policy "accept-new" -}}
/etc/git-repo-backup/ssh-state/state/known_hosts
{{- else -}}
/etc/git-repo-backup/ssh/known_hosts
{{- end -}}
{{- end -}}

{{- define "git-repo-backup.inputKnownHostsFile" -}}
{{- if or .Values.ssh.knownHosts.existingConfigMap .Values.ssh.knownHosts.existingSecret -}}
/input/known-hosts/known_hosts
{{- else -}}
""
{{- end -}}
{{- end -}}

{{- define "git-repo-backup.prepareKnownHostsStateFile" -}}
{{- if and .Values.ssh.enabled (eq (default "accept-new" .Values.ssh.hostKeyPolicy) "accept-new") -}}
/known-hosts-state/state/known_hosts
{{- else -}}
""
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
