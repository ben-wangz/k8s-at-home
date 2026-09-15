{{/*
Cross-field validation that JSON Schema cannot express precisely: duplicate
repository names, duration arithmetic, claim/mount combinations, and
workload-identity wiring. Render-time checks are deliberately conservative;
the binary re-validates everything at runtime.
*/}}
{{- define "git-repo-backup.validateValues" -}}
  {{- $repos := .Values.repositories -}}
  {{- if $repos -}}
    {{- $names := dict -}}
    {{- range $repos -}}
      {{- if hasKey $names .name -}}
        {{- fail (printf "duplicate repository name %q in repositories" .name) -}}
      {{- end -}}
      {{- $_ := set $names .name true -}}
    {{- end -}}
  {{- end -}}

  {{- $run := .Values.backup.maxRunDuration | default "5h30m" -}}
  {{- if mustRegexMatch "^[0-9]+d$" $run -}}
    {{- fail "backup.maxRunDuration must use Go duration syntax (no 'd' unit)" -}}
  {{- end -}}
  {{- $runSeconds := (include "git-repo-backup.durationSeconds" $run) | float64 -}}
  {{- $incomplete := .Values.retention.incompleteMaxAge | default "24h" -}}
  {{- if mustRegexMatch "^[0-9]+d$" $incomplete -}}
    {{- fail "retention.incompleteMaxAge must use Go duration syntax (no 'd' unit)" -}}
  {{- end -}}
  {{- $incompleteSeconds := (include "git-repo-backup.durationSeconds" $incomplete) | float64 -}}
  {{- if le $incompleteSeconds (addf $runSeconds 600.0) -}}
    {{- fail "retention.incompleteMaxAge must exceed backup.maxRunDuration by at least 10m" -}}
  {{- end -}}
  {{- $deadline := .Values.activeDeadlineSeconds | float64 -}}
  {{- $grace := .Values.terminationGracePeriodSeconds | float64 -}}
  {{- if le $deadline (addf $runSeconds $grace) -}}
    {{- fail "activeDeadlineSeconds must exceed backup.maxRunDuration + terminationGracePeriodSeconds" -}}
  {{- end -}}

  {{- if and .Values.cache.enabled (eq .Values.storage.type "s3") (not .Values.cache.existingClaim) -}}
    {{- fail "cache.enabled with storage.type=s3 requires cache.existingClaim (a separate PVC)" -}}
  {{- end -}}
  {{- if and .Values.cache.enabled (not .Values.cache.existingClaim) (eq .Values.storage.type "local") (not (or .Values.storage.local.persistence.enabled .Values.storage.local.existingClaim)) -}}
    {{- fail "cache.enabled requires a backup volume" -}}
  {{- end -}}

  {{- if and .Values.workspace.existingClaim .Values.cache.existingClaim (eq .Values.workspace.existingClaim .Values.cache.existingClaim) -}}
    {{- fail "workspace.existingClaim and cache.existingClaim must not reference the same claim" -}}
  {{- end -}}

  {{- if and (eq .Values.storage.s3.credentialsMode "workloadIdentity") -}}
    {{- if not (or .Values.serviceAccount.create .Values.serviceAccount.name) -}}
      {{- fail "workloadIdentity requires serviceAccount.create=true (chart-managed) or a preconfigured serviceAccount.name" -}}
    {{- end -}}
  {{- end -}}

  {{- if and .Values.customCA.existingConfigMap .Values.customCA.existingSecret -}}
    {{- fail "customCA: choose exactly one of existingConfigMap or existingSecret" -}}
  {{- end -}}
{{- end -}}
