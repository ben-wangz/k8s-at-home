{{- define "split-dns.name" -}}
{{- include "common.names.name" . -}}
{{- end -}}

{{- define "split-dns.fullname" -}}
{{- include "common.names.fullname" . -}}
{{- end -}}

{{- define "split-dns.labels" -}}
{{- include "common.labels.standard" (dict "customLabels" .Values.commonLabels "context" .) -}}
{{- end -}}

{{- define "split-dns.podLabels" -}}
{{- $customLabels := merge (deepCopy .Values.podLabels) .Values.commonLabels -}}
{{- include "common.labels.standard" (dict "customLabels" $customLabels "context" .) -}}
{{- end -}}

{{- define "split-dns.selectorLabels" -}}
{{- include "common.labels.matchLabels" (dict "customLabels" .Values.commonLabels "context" .) -}}
{{- end -}}

{{- define "split-dns.image" -}}
{{- include "common.images.image" (dict "imageRoot" .Values.image "global" .Values.global "chart" .Chart) -}}
{{- end -}}

{{- define "split-dns.proxyUsernameEnv" -}}
{{- printf "SPLIT_DNS_PROXY_%s_USERNAME" . | upper | replace "-" "_" -}}
{{- end -}}

{{- define "split-dns.proxyPasswordEnv" -}}
{{- printf "SPLIT_DNS_PROXY_%s_PASSWORD" . | upper | replace "-" "_" -}}
{{- end -}}

{{- define "split-dns.runtimeConfig" -}}
{{- $groups := dict -}}
{{- range $name, $source := .Values.dns.upstreamGroups -}}
  {{- $group := dict "type" $source.type "queryTimeout" $source.queryTimeout "maxConcurrentRequests" $source.maxConcurrentRequests -}}
  {{- if eq $source.type "forward" -}}
    {{- $_ := set $group "servers" $source.servers -}}
  {{- else -}}
    {{- $_ := set $group "strategy" $source.strategy -}}
    {{- $_ := set $group "egress" $source.egress -}}
    {{- $_ := set $group "connectTimeout" $source.connectTimeout -}}
    {{- $_ := set $group "tlsHandshakeTimeout" $source.tlsHandshakeTimeout -}}
    {{- $_ := set $group "maxAttemptsPerEndpoint" $source.maxAttemptsPerEndpoint -}}
    {{- $endpoints := list -}}
    {{- range $index, $sourceEndpoint := $source.endpoints -}}
      {{- $endpoint := dict "url" $sourceEndpoint.url -}}
      {{- if $sourceEndpoint.bootstrapIPs -}}
        {{- $_ := set $endpoint "bootstrapIPs" $sourceEndpoint.bootstrapIPs -}}
      {{- end -}}
      {{- if $sourceEndpoint.ca -}}
        {{- $_ := set $endpoint "caFile" (printf "/etc/split-dns/ca/%s-%d/bundle.pem" $name $index) -}}
      {{- end -}}
      {{- $endpoints = append $endpoints $endpoint -}}
    {{- end -}}
    {{- $_ := set $group "endpoints" $endpoints -}}
  {{- end -}}
  {{- $_ := set $groups $name $group -}}
{{- end -}}
{{- $proxies := dict -}}
{{- range $name, $proxy := .Values.dns.proxies -}}
  {{- $runtimeProxy := dict "url" $proxy.url -}}
  {{- if and $proxy.auth $proxy.auth.existingSecret -}}
    {{- $_ := set $runtimeProxy "usernameEnv" (include "split-dns.proxyUsernameEnv" $name) -}}
    {{- $_ := set $runtimeProxy "passwordEnv" (include "split-dns.proxyPasswordEnv" $name) -}}
  {{- end -}}
  {{- $_ := set $proxies $name $runtimeProxy -}}
{{- end -}}
{{- $dns := dict
  "defaultUpstreamGroup" .Values.dns.defaultUpstreamGroup
  "upstreamGroups" $groups
  "proxies" $proxies
  "rules" .Values.dns.rules
  "cache" .Values.dns.cache -}}
{{- toJson (dict
  "listenAddress" "0.0.0.0"
  "listenPort" .Values.containerPort
  "metrics" (dict "address" "0.0.0.0" "port" .Values.healthPort "enabled" .Values.metrics.enabled)
  "dns" $dns) -}}
{{- end -}}

{{- define "split-dns.validate" -}}
{{- if eq .Values.containerPort .Values.healthPort -}}
  {{- fail "containerPort and healthPort must be different" -}}
{{- end -}}
{{- if and .Values.metrics.enabled (eq .Values.service.port .Values.metrics.port) -}}
  {{- fail "metrics.port and service.port must be different when metrics are enabled" -}}
{{- end -}}
{{- if or (hasKey .Values.podLabels "app.kubernetes.io/name") (hasKey .Values.podLabels "app.kubernetes.io/instance") -}}
  {{- fail "podLabels cannot override app.kubernetes.io/name or app.kubernetes.io/instance" -}}
{{- end -}}
{{- $groups := .Values.dns.upstreamGroups -}}
{{- if not (hasKey $groups .Values.dns.defaultUpstreamGroup) -}}
  {{- fail (printf "dns.defaultUpstreamGroup %q does not exist" .Values.dns.defaultUpstreamGroup) -}}
{{- end -}}
{{- range $name, $group := $groups -}}
  {{- if and (eq $group.type "doh") (eq $group.egress.mode "proxy") -}}
    {{- if not (hasKey $.Values.dns.proxies $group.egress.proxyRef) -}}
      {{- fail (printf "upstream group %q references missing proxy %q" $name $group.egress.proxyRef) -}}
    {{- end -}}
  {{- end -}}
  {{- if eq $group.type "doh" -}}
    {{- range $index, $endpoint := $group.endpoints -}}
      {{- if and $endpoint.ca $endpoint.ca.secretName $endpoint.ca.configMapName -}}
        {{- fail (printf "upstream group %q endpoint %d ca must reference either a Secret or ConfigMap" $name $index) -}}
      {{- end -}}
      {{- if and $endpoint.ca (not (or $endpoint.ca.secretName $endpoint.ca.configMapName)) -}}
        {{- fail (printf "upstream group %q endpoint %d ca requires secretName or configMapName" $name $index) -}}
      {{- end -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- $rules := dict -}}
{{- range $index, $rule := .Values.dns.rules -}}
  {{- if not (hasKey $groups $rule.upstreamGroup) -}}
    {{- fail (printf "dns.rules[%d] references missing upstream group %q" $index $rule.upstreamGroup) -}}
  {{- end -}}
  {{- $domain := trimSuffix "." (lower $rule.domain) -}}
  {{- $key := printf "%s:%s" $rule.match $domain -}}
  {{- if hasKey $rules $key -}}
    {{- fail (printf "dns.rules contains duplicate rule %q after normalization" $key) -}}
  {{- end -}}
  {{- $_ := set $rules $key true -}}
{{- end -}}
{{- range $name, $proxy := .Values.dns.proxies -}}
  {{- if and $proxy.auth (not $proxy.auth.existingSecret) -}}
    {{- fail (printf "dns.proxies.%s.auth requires existingSecret" $name) -}}
  {{- end -}}
{{- end -}}
{{- end -}}
