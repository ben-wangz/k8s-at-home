{{/*
Common helper names.
*/}}
{{- define "sub2api.pvc.fullname" -}}
{{- printf "%s-data" (include "common.names.fullname" .) -}}
{{- end -}}

{{- define "sub2api.auth.secretName" -}}
{{- if .Values.sub2api.auth.existingSecret -}}
{{- tpl .Values.sub2api.auth.existingSecret . -}}
{{- else -}}
{{- printf "%s-auth" (include "common.names.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "sub2api.auth.adminPasswordKey" -}}
{{- if .Values.sub2api.auth.existingSecret -}}
{{- tpl (default "admin-password" .Values.sub2api.auth.existingSecretKeys.adminPasswordKey) . -}}
{{- else -}}
admin-password
{{- end -}}
{{- end -}}

{{- define "sub2api.auth.jwtSecretKey" -}}
{{- if .Values.sub2api.auth.existingSecret -}}
{{- tpl (default "jwt-secret" .Values.sub2api.auth.existingSecretKeys.jwtSecretKey) . -}}
{{- else -}}
jwt-secret
{{- end -}}
{{- end -}}

{{- define "sub2api.auth.totpEncryptionKey" -}}
{{- if .Values.sub2api.auth.existingSecret -}}
{{- tpl (default "totp-encryption-key" .Values.sub2api.auth.existingSecretKeys.totpEncryptionKey) . -}}
{{- else -}}
totp-encryption-key
{{- end -}}
{{- end -}}

{{- define "sub2api.redis.dependency.fullname" -}}
{{- include "common.names.dependency.fullname" (dict "chartName" "redis" "chartValues" .Values.redis "context" $) -}}
{{- end -}}

{{- define "sub2api.redis.host" -}}
{{- if .Values.redis.enabled -}}
{{- printf "%s-master" (include "sub2api.redis.dependency.fullname" .) -}}
{{- else -}}
{{- required "externalRedis.host is required when redis.enabled=false" (tpl (default "" .Values.externalRedis.host) .) -}}
{{- end -}}
{{- end -}}

{{- define "sub2api.redis.port" -}}
{{- if .Values.redis.enabled -}}
{{- dig "master" "service" "ports" "redis" 6379 .Values.redis -}}
{{- else -}}
{{- default 6379 .Values.externalRedis.port -}}
{{- end -}}
{{- end -}}

{{- define "sub2api.redis.db" -}}
{{- if .Values.redis.enabled -}}
{{- default 0 .Values.sub2api.redis.db -}}
{{- else -}}
{{- default 0 .Values.externalRedis.db -}}
{{- end -}}
{{- end -}}

{{- define "sub2api.redis.enableTLS" -}}
{{- if .Values.redis.enabled -}}
{{- default false .Values.sub2api.redis.enableTLS -}}
{{- else -}}
{{- default false .Values.externalRedis.enableTLS -}}
{{- end -}}
{{- end -}}

{{- define "sub2api.redis.password.secretName" -}}
{{- if .Values.redis.enabled -}}
{{- if .Values.redis.auth.enabled -}}
{{- if .Values.redis.auth.existingSecret -}}
{{- tpl .Values.redis.auth.existingSecret . -}}
{{- else -}}
{{- include "sub2api.redis.dependency.fullname" . -}}
{{- end -}}
{{- end -}}
{{- else -}}
{{- if .Values.externalRedis.existingSecret -}}
{{- tpl .Values.externalRedis.existingSecret . -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "sub2api.redis.password.secretKey" -}}
{{- if .Values.redis.enabled -}}
{{- if .Values.redis.auth.enabled -}}
{{- if and .Values.redis.auth.existingSecret .Values.redis.auth.existingSecretPasswordKey -}}
{{- tpl .Values.redis.auth.existingSecretPasswordKey . -}}
{{- else -}}
redis-password
{{- end -}}
{{- end -}}
{{- else -}}
{{- if .Values.externalRedis.existingSecret -}}
{{- tpl (default "redis-password" .Values.externalRedis.existingSecretPasswordKey) . -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "sub2api.postgresql.dependency.fullname" -}}
{{- include "common.names.dependency.fullname" (dict "chartName" "postgresql" "chartValues" .Values.postgresql "context" $) -}}
{{- end -}}

{{- define "sub2api.postgresql.primary.fullname" -}}
{{- $fullname := include "sub2api.postgresql.dependency.fullname" . -}}
{{- if eq (default "standalone" .Values.postgresql.architecture) "replication" -}}
{{- printf "%s-%s" $fullname (default "primary" .Values.postgresql.primary.name) -}}
{{- else -}}
{{- $fullname -}}
{{- end -}}
{{- end -}}

{{- define "sub2api.postgresql.host" -}}
{{- if .Values.postgresql.enabled -}}
{{- include "sub2api.postgresql.primary.fullname" . -}}
{{- else -}}
{{- required "externalPostgresql.host is required when postgresql.enabled=false" (tpl (default "" .Values.externalPostgresql.host) .) -}}
{{- end -}}
{{- end -}}

{{- define "sub2api.postgresql.port" -}}
{{- if .Values.postgresql.enabled -}}
{{- dig "primary" "service" "ports" "postgresql" 5432 .Values.postgresql -}}
{{- else -}}
{{- default 5432 .Values.externalPostgresql.port -}}
{{- end -}}
{{- end -}}

{{- define "sub2api.postgresql.username" -}}
{{- if .Values.postgresql.enabled -}}
{{- default "sub2api" .Values.postgresql.auth.username -}}
{{- else -}}
{{- default "sub2api" .Values.externalPostgresql.username -}}
{{- end -}}
{{- end -}}

{{- define "sub2api.postgresql.database" -}}
{{- if .Values.postgresql.enabled -}}
{{- default "sub2api" .Values.postgresql.auth.database -}}
{{- else -}}
{{- default "sub2api" .Values.externalPostgresql.database -}}
{{- end -}}
{{- end -}}

{{- define "sub2api.postgresql.sslmode" -}}
{{- if .Values.postgresql.enabled -}}
{{- default "disable" .Values.sub2api.database.sslmode -}}
{{- else -}}
{{- default "disable" .Values.externalPostgresql.sslmode -}}
{{- end -}}
{{- end -}}

{{- define "sub2api.postgresql.password.secretName" -}}
{{- if .Values.postgresql.enabled -}}
{{- if .Values.postgresql.auth.existingSecret -}}
{{- tpl .Values.postgresql.auth.existingSecret . -}}
{{- else -}}
{{- include "sub2api.postgresql.dependency.fullname" . -}}
{{- end -}}
{{- else -}}
{{- if .Values.externalPostgresql.existingSecret -}}
{{- tpl .Values.externalPostgresql.existingSecret . -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "sub2api.postgresql.password.secretKey" -}}
{{- if .Values.postgresql.enabled -}}
{{- if and .Values.postgresql.auth.existingSecret .Values.postgresql.auth.secretKeys.userPasswordKey -}}
{{- tpl .Values.postgresql.auth.secretKeys.userPasswordKey . -}}
{{- else -}}
password
{{- end -}}
{{- else -}}
{{- if .Values.externalPostgresql.existingSecret -}}
{{- tpl (default "postgres-password" .Values.externalPostgresql.existingSecretPasswordKey) . -}}
{{- end -}}
{{- end -}}
{{- end -}}
