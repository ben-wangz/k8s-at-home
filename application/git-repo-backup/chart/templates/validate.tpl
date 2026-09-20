{{/*
Cross-field validation that JSON Schema cannot express precisely: duplicate
repository names, duration arithmetic, claim/mount combinations, and
workload-identity wiring. Render-time checks are deliberately conservative;
the binary re-validates everything at runtime.
*/}}
{{- define "git-repo-backup.validateValues" -}}
  {{- if .Values.ssh.enabled -}}
    {{- $policy := .Values.ssh.hostKeyPolicy | default "accept-new" -}}
    {{- if and (ne $policy "pinned") (ne $policy "accept-new") (ne $policy "none") -}}
      {{- fail "ssh.hostKeyPolicy must be pinned, accept-new, or none" -}}
    {{- end -}}
    {{- $hasConfigMap := gt (len (.Values.ssh.knownHosts.existingConfigMap | default "")) 0 -}}
    {{- $hasSecret := gt (len (.Values.ssh.knownHosts.existingSecret | default "")) 0 -}}
    {{- if and $hasConfigMap $hasSecret -}}
      {{- fail "ssh.knownHosts: choose at most one of existingConfigMap or existingSecret" -}}
    {{- end -}}
    {{- if eq $policy "pinned" -}}
      {{- if not (or $hasConfigMap $hasSecret) -}}
        {{- fail "ssh.hostKeyPolicy=pinned requires ssh.knownHosts.existingConfigMap or existingSecret" -}}
      {{- end -}}
      {{- if .Values.ssh.knownHosts.existingClaim -}}
        {{- fail "ssh.hostKeyPolicy=pinned does not use ssh.knownHosts.existingClaim" -}}
      {{- end -}}
    {{- else if eq $policy "accept-new" -}}
      {{- if and (not .Values.ssh.knownHosts.existingClaim) (not .Values.ssh.knownHosts.persistence.enabled) -}}
        {{- fail "ssh.hostKeyPolicy=accept-new requires knownHosts.existingClaim or knownHosts.persistence.enabled" -}}
      {{- end -}}
    {{- else if eq $policy "none" -}}
      {{- if or $hasConfigMap $hasSecret .Values.ssh.knownHosts.existingClaim -}}
        {{- fail "ssh.hostKeyPolicy=none must not configure known-hosts input or state" -}}
      {{- end -}}
    {{- end -}}
  {{- end -}}

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

  {{- if and (eq .Values.storage.type "s3") .Values.storage.s3.endpoint (mustRegexMatch "(?i)^http://" .Values.storage.s3.endpoint) (not .Values.storage.s3.allowInsecureHttp) -}}
    {{- fail "storage.s3.endpoint uses http; set storage.s3.allowInsecureHttp=true only for an isolated S3-compatible endpoint" -}}
  {{- end -}}

  {{- if and .Values.customCA.existingConfigMap .Values.customCA.existingSecret -}}
    {{- fail "customCA: choose exactly one of existingConfigMap or existingSecret" -}}
  {{- end -}}
{{- end -}}
