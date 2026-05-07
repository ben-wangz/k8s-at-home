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

{{/* Resolve ingress auth provider */}}
{{- define "chromium-bridge.ingressCredentials.provider" -}}
{{- $ctx := .context -}}
{{- $ingress := .ingress -}}
{{- if and $ingress.auth.provider (ne $ingress.auth.provider "auto") -}}
{{- $ingress.auth.provider -}}
{{- else if and $ingress.ingressClassName (contains "traefik" (lower $ingress.ingressClassName)) -}}
traefik
{{- else if and $ingress.ingressClassName (contains "nginx" (lower $ingress.ingressClassName)) -}}
nginx
{{- else if and (hasKey $ingress.annotations "kubernetes.io/ingress.class") (contains "traefik" (lower (index $ingress.annotations "kubernetes.io/ingress.class"))) -}}
traefik
{{- else if and (hasKey $ingress.annotations "kubernetes.io/ingress.class") (contains "nginx" (lower (index $ingress.annotations "kubernetes.io/ingress.class"))) -}}
nginx
{{- else if .context.Capabilities.APIVersions.Has "traefik.io/v1alpha1/Middleware" -}}
traefik
{{- else -}}
nginx
{{- end -}}
{{- end -}}

{{/* Generate deterministic fallback password */}}
{{- define "chromium-bridge.ingressCredentials.password" -}}
{{- $ctx := .context -}}
{{- $component := .component -}}
{{- $ingress := .ingress -}}
{{- if $ingress.auth.password -}}
{{- $ingress.auth.password -}}
{{- else -}}
{{- $seed := printf "%s-%s-%s-credentials" (include "common.names.fullname" $ctx) $ctx.Release.Namespace $component -}}
{{- sha256sum $seed | trunc 24 -}}
{{- end -}}
{{- end -}}

{{/* Ingress credentials secret name */}}
{{- define "chromium-bridge.ingressCredentials.secretName" -}}
{{- $ctx := .context -}}
{{- $component := .component -}}
{{- $ingress := .ingress -}}
{{- if $ingress.auth.existingSecret -}}
{{- $ingress.auth.existingSecret -}}
{{- else -}}
{{- printf "%s-%s-credentials" (include "common.names.fullname" $ctx) $component -}}
{{- end -}}
{{- end -}}

{{/* Render merged ingress annotations with auth */}}
{{- define "chromium-bridge.ingress.annotations" -}}
{{- $ctx := .context -}}
{{- $component := .component -}}
{{- $ingress := .ingress -}}
{{- $annotations := dict -}}
{{- if $ingress.annotations -}}
{{- $annotations = mergeOverwrite $annotations $ingress.annotations -}}
{{- end -}}
{{- if and $ingress.enabled $ingress.auth.enabled -}}
{{- $provider := include "chromium-bridge.ingressCredentials.provider" (dict "context" $ctx "ingress" $ingress) -}}
{{- $secretName := include "chromium-bridge.ingressCredentials.secretName" (dict "context" $ctx "component" $component "ingress" $ingress) -}}
{{- if eq $provider "nginx" -}}
{{- $_ := set $annotations "nginx.ingress.kubernetes.io/auth-type" "basic" -}}
{{- $_ := set $annotations "nginx.ingress.kubernetes.io/auth-secret" $secretName -}}
{{- $_ := set $annotations "nginx.ingress.kubernetes.io/auth-realm" "Authentication Required" -}}
{{- else if eq $provider "traefik" -}}
{{- if $ingress.auth.traefik.existingMiddleware -}}
{{- $_ := set $annotations "traefik.ingress.kubernetes.io/router.middlewares" $ingress.auth.traefik.existingMiddleware -}}
{{- else -}}
{{- $_ := set $annotations "traefik.ingress.kubernetes.io/router.middlewares" (printf "%s-%s-%s-credentials@kubernetescrd" (include "common.names.namespace" $ctx) (include "common.names.fullname" $ctx) $component) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- toYaml $annotations -}}
{{- end -}}
