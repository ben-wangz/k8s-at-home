{{/*
Expand the name of the chart.
*/}}

{{- define "tabminal.pvc.fullname" -}}
{{- include "common.names.fullname" . }}-pvc
{{- end -}}

{{- define "tabminal.secret.name" -}}
{{- required "tabminal.existingSecret is required" (include "common.tplvalues.render" (dict "value" .Values.tabminal.existingSecret "context" $)) -}}
{{- end -}}

{{- define "tabminal.ssh.keygen.fullname" -}}
{{- printf "%s-ssh-keygen" (include "common.names.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "tabminal.ssh.generatedSecretName" -}}
{{- if .Values.ssh.keyGeneration.outputSecretName -}}
{{- include "common.tplvalues.render" (dict "value" .Values.ssh.keyGeneration.outputSecretName "context" $) -}}
{{- else -}}
{{- printf "%s-ssh" (include "common.names.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "tabminal.ssh.secretName" -}}
{{- if .Values.ssh.keyGeneration.enabled -}}
{{- include "tabminal.ssh.generatedSecretName" . -}}
{{- else -}}
{{- required "ssh.existingSecret is required when SSH key generation is disabled" (include "common.tplvalues.render" (dict "value" .Values.ssh.existingSecret "context" $)) -}}
{{- end -}}
{{- end -}}

{{- define "tabminal.validateValues" -}}
{{- if not (kindIs "slice" .Values.tabminal.extraEnvVars) -}}
{{- fail "tabminal.extraEnvVars must be a list of Kubernetes EnvVar objects" -}}
{{- end -}}
{{- if and .Values.tabminal.existingSecretOpenrouterKey .Values.tabminal.existingSecretOpenaiKey -}}
{{- fail "tabminal.existingSecretOpenrouterKey and tabminal.existingSecretOpenaiKey are mutually exclusive" -}}
{{- end -}}
{{- if and .Values.ssh.keyGeneration.enabled (not .Values.ssh.enabled) -}}
{{- fail "ssh.enabled must be true when ssh.keyGeneration.enabled is true" -}}
{{- end -}}
{{- if and .Values.ssh.keyGeneration.enabled .Values.ssh.existingSecret -}}
{{- fail "ssh.existingSecret and ssh.keyGeneration.enabled are mutually exclusive" -}}
{{- end -}}
{{- if .Values.ssh.enabled -}}
  {{- if .Values.ssh.keyGeneration.enabled -}}
    {{- if not .Values.ssh.keyGeneration.passphraseSecret.name -}}
    {{- fail "ssh.keyGeneration.passphraseSecret.name is required when key generation is enabled" -}}
    {{- end -}}
    {{- if not .Values.ssh.keyGeneration.passphraseSecret.key -}}
    {{- fail "ssh.keyGeneration.passphraseSecret.key is required when key generation is enabled" -}}
    {{- end -}}
    {{- if lt (int .Values.ssh.keyGeneration.serviceAccountTokenExpirationSeconds) 600 -}}
    {{- fail "ssh.keyGeneration.serviceAccountTokenExpirationSeconds must be at least 600" -}}
    {{- end -}}
  {{- else -}}
    {{- if not .Values.ssh.existingSecret -}}
    {{- fail "ssh.existingSecret is required when ssh.enabled is true and key generation is disabled" -}}
    {{- end -}}
    {{- if not .Values.ssh.privateKeyKey -}}
    {{- fail "ssh.privateKeyKey is required when ssh.enabled is true" -}}
    {{- end -}}
    {{- if not .Values.ssh.privateKeyFilename -}}
    {{- fail "ssh.privateKeyFilename is required when ssh.enabled is true" -}}
    {{- end -}}
    {{- if or (contains "/" .Values.ssh.privateKeyFilename) (contains ".." .Values.ssh.privateKeyFilename) -}}
    {{- fail "ssh.privateKeyFilename must be a filename without path components" -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- end -}}
