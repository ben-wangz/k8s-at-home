{{- define "registry-hub.fullname" -}}
{{- include "common.names.fullname" . -}}
{{- end -}}

{{- define "registry-hub.backendImage" -}}
{{- include "common.images.image" (dict "imageRoot" .Values.backend.image "global" .Values.global "chart" .Chart) -}}
{{- end -}}

{{- define "registry-hub.registryImage" -}}
{{- include "common.images.image" (dict "imageRoot" .Values.registry.image "global" .Values.global "chart" .Chart) -}}
{{- end -}}

{{- define "registry-hub.labels" -}}
{{- include "common.labels.standard" (dict "customLabels" .Values.commonLabels "context" .) -}}
{{- end -}}

{{- define "registry-hub.backendSelector" -}}
{{ include "common.labels.matchLabels" (dict "customLabels" .Values.commonLabels "context" .) }}
app.kubernetes.io/component: backend
{{- end -}}

{{- define "registry-hub.registrySelector" -}}
{{ include "common.labels.matchLabels" (dict "customLabels" .Values.commonLabels "context" .) }}
app.kubernetes.io/component: registry
{{- end -}}

{{- define "registry-hub.internalSecretName" -}}
{{- $fullname := include "registry-hub.fullname" . -}}
{{- if le (len $fullname) 45 -}}
{{ printf "%s-registry-internal" $fullname }}
{{- else -}}
{{ printf "%s-%s-registry-internal" ($fullname | trunc 34 | trimSuffix "-") ($fullname | sha256sum | trunc 10) | trimSuffix "-" }}
{{- end -}}
{{- end -}}

{{- define "registry-hub.initializationRecordName" -}}
{{- $fullname := include "registry-hub.fullname" . -}}
{{- if le (len $fullname) 39 -}}
{{ printf "%s-registry-initialization" $fullname }}
{{- else -}}
{{ printf "%s-%s-registry-initialization" ($fullname | trunc 28 | trimSuffix "-") ($fullname | sha256sum | trunc 10) | trimSuffix "-" }}
{{- end -}}
{{- end -}}

{{- define "registry-hub.adminAuthGeneratedSecretName" -}}
{{ printf "%s-admin-auth" (include "registry-hub.fullname" .) }}
{{- end -}}

{{- define "registry-hub.adminInitializationRecordName" -}}
{{- $fullname := include "registry-hub.fullname" . -}}
{{- if le (len $fullname) 37 -}}
{{ printf "%s-admin-auth-initialization" $fullname }}
{{- else -}}
{{ printf "%s-%s-admin-auth-initialization" ($fullname | trunc 26 | trimSuffix "-") ($fullname | sha256sum | trunc 10) | trimSuffix "-" }}
{{- end -}}
{{- end -}}

{{- define "registry-hub.initServiceAccountName" -}}
{{ printf "%s-registry-init" (include "registry-hub.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end -}}

{{- define "registry-hub.distributionAuthSecretName" -}}
{{ default (include "registry-hub.internalSecretName" .) .Values.backend.distributionAuth.existingSecret }}
{{- end -}}

{{- define "registry-hub.distributionTLSSecretName" -}}
{{ default (include "registry-hub.internalSecretName" .) .Values.registry.tls.existingSecret }}
{{- end -}}

{{- define "registry-hub.distributionHTPasswdSecretName" -}}
{{ default (include "registry-hub.internalSecretName" .) .Values.registry.auth.htpasswdSecret }}
{{- end -}}

{{- define "registry-hub.distributionHTTPSecretName" -}}
{{ default (include "registry-hub.internalSecretName" .) .Values.registry.httpSecret.existingSecret }}
{{- end -}}

{{- define "registry-hub.adminAuthSecretName" -}}
{{ default (include "registry-hub.adminAuthGeneratedSecretName" .) .Values.backend.adminAuth.existingSecret }}
{{- end -}}

{{- define "registry-hub.needsInternalInitialization" -}}
{{- if or (not .Values.backend.distributionAuth.existingSecret) (not .Values.registry.auth.htpasswdSecret) (not .Values.registry.tls.existingSecret) (not .Values.registry.httpSecret.existingSecret) -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "registry-hub.needsAdminInitialization" -}}
{{- if not .Values.backend.adminAuth.existingSecret -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "registry-hub.needsInitialization" -}}
{{- if or (eq (include "registry-hub.needsInternalInitialization" .) "true") (eq (include "registry-hub.needsAdminInitialization" .) "true") -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "registry-hub.internalInitScope" -}}
{{- $auth := or (not .Values.backend.distributionAuth.existingSecret) (not .Values.registry.auth.htpasswdSecret) -}}
{{- $tls := not .Values.registry.tls.existingSecret -}}
{{- $http := not .Values.registry.httpSecret.existingSecret -}}
{{- toJson (dict
  "protocol" "v1"
  "instance" (include "registry-hub.fullname" .)
  "secret" (include "registry-hub.internalSecretName" .)
  "record" (include "registry-hub.initializationRecordName" .)
  "namespace" .Release.Namespace
  "serviceName" (printf "%s-registry" (include "registry-hub.fullname" .))
  "auth" $auth
  "tls" $tls
  "httpSecret" $http
  "authKey" (ternary .Values.backend.distributionAuth.key "" $auth)
  "htpasswdKey" (ternary .Values.registry.auth.htpasswdKey "" $auth)
  "caKey" (ternary .Values.registry.tls.caKey "" $tls)
  "certificateKey" (ternary .Values.registry.tls.certificateKey "" $tls)
  "privateKeyKey" (ternary .Values.registry.tls.privateKeyKey "" $tls)
  "httpSecretKey" (ternary .Values.registry.httpSecret.key "" $http)) -}}
{{- end -}}

{{- define "registry-hub.adminInitScope" -}}
{{- toJson (dict
  "protocol" "admin-v1"
  "instance" (include "registry-hub.fullname" .)
  "secret" (include "registry-hub.adminAuthGeneratedSecretName" .)
  "record" (include "registry-hub.adminInitializationRecordName" .)
  "namespace" .Release.Namespace
  "key" .Values.backend.adminAuth.key
  "username" "admin"
  "permissions" (list "task-admin" "referrer-prune" "gc-admin")) -}}
{{- end -}}

{{- define "registry-hub.internalInitJobName" -}}
{{- $identity := dict
  "internalScope" (ternary (include "registry-hub.internalInitScope" . | sha256sum) "" (eq (include "registry-hub.needsInternalInitialization" .) "true"))
  "adminScope" (ternary (include "registry-hub.adminInitScope" . | sha256sum) "" (eq (include "registry-hub.needsAdminInitialization" .) "true"))
  "image" (include "registry-hub.backendImage" .)
  "pullPolicy" .Values.backend.image.pullPolicy
  "pullSecrets" .Values.imagePullSecrets
  "resources" .Values.backend.resources
  "podSecurityContext" .Values.podSecurityContext -}}
{{- $suffix := toJson $identity | sha256sum | trunc 10 -}}
{{- printf "%s-init-%s" (include "registry-hub.fullname" . | trunc 38 | trimSuffix "-") $suffix | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "registry-hub.adminWaitContainer" -}}
- name: wait-admin-auth-initialization
  image: "{{ include "registry-hub.backendImage" . }}"
  imagePullPolicy: {{ .Values.backend.image.pullPolicy }}
  command: ["/registry-hub"]
  args: ["wait-initialization"]
  env:
    - name: REGISTRY_HUB_INIT_RECORD_FILE
      value: /var/run/registry-hub-admin-initialization/record.json
    - name: REGISTRY_HUB_INIT_SCOPE
      value: {{ include "registry-hub.adminInitScope" . | sha256sum | quote }}
    - name: REGISTRY_HUB_INIT_WAIT_PROTOCOL
      value: "admin-v1"
    - name: REGISTRY_HUB_INIT_WAIT_TIMEOUT_SECONDS
      value: "600"
  volumeMounts:
    - name: admin-initialization-record
      mountPath: /var/run/registry-hub-admin-initialization
      readOnly: true
{{- end -}}

{{- define "registry-hub.adminWaitVolume" -}}
- name: admin-initialization-record
  configMap:
    name: {{ include "registry-hub.adminInitializationRecordName" . }}
    optional: true
    items:
      - key: record.json
        path: record.json
{{- end -}}

{{- define "registry-hub.backendInitWaitContainers" -}}
{{- if eq (include "registry-hub.needsInternalInitialization" .) "true" }}
{{ include "registry-hub.internalWaitContainer" . }}
{{- end }}
{{- if eq (include "registry-hub.needsAdminInitialization" .) "true" }}
{{ include "registry-hub.adminWaitContainer" . }}
{{- end }}
{{- end -}}

{{- define "registry-hub.backendInitWaitVolumes" -}}
{{- if eq (include "registry-hub.needsInternalInitialization" .) "true" }}
{{ include "registry-hub.internalWaitVolume" . }}
{{- end }}
{{- if eq (include "registry-hub.needsAdminInitialization" .) "true" }}
{{ include "registry-hub.adminWaitVolume" . }}
{{- end }}
{{- end -}}

{{- define "registry-hub.internalWaitContainer" -}}
- name: wait-internal-initialization
  image: "{{ include "registry-hub.backendImage" . }}"
  imagePullPolicy: {{ .Values.backend.image.pullPolicy }}
  command: ["/registry-hub"]
  args: ["wait-internal"]
  env:
    - name: REGISTRY_HUB_INIT_RECORD_FILE
      value: /var/run/registry-hub-initialization/record.json
    - name: REGISTRY_HUB_INIT_SCOPE
      value: {{ include "registry-hub.internalInitScope" . | sha256sum | quote }}
    - name: REGISTRY_HUB_INIT_WAIT_TIMEOUT_SECONDS
      value: "600"
  volumeMounts:
    - name: internal-initialization-record
      mountPath: /var/run/registry-hub-initialization
      readOnly: true
{{- end -}}

{{- define "registry-hub.internalWaitVolume" -}}
- name: internal-initialization-record
  configMap:
    name: {{ include "registry-hub.initializationRecordName" . }}
    optional: true
    items:
      - key: record.json
        path: record.json
{{- end -}}
