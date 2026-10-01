{{/*
Common env vars shared by all three binaries (server, consumer, rotator) —
database, OpenBao, and general app config (§12). Each workload template
appends its own additional vars (SNS/Glue for server, SQS for consumer,
prune tuning for rotator).
*/}}
{{- define "iam-token-service.commonEnv" -}}
- name: APP_NAME
  value: {{ include "iam-token-service.name" . }}
- name: APP_ENV
  value: {{ .Values.appEnv | quote }}
- name: OBSERVABILITY_DOMAIN
  value: {{ .Values.observabilityDomain | default "iam" | quote }}
- name: BUILD_VERSION
  value: {{ include "iam-token-service.imageTag" . | quote }}
- name: LOG_LEVEL
  value: {{ .Values.logLevel | quote }}
- name: PG_BOUNCER_MODE
  value: {{ .Values.database.pgBouncerMode | quote }}
- name: DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.existingSecretName | default (include "iam-token-service.secretName" .) }}
      key: DATABASE_URL
- name: MIGRATION_DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.existingSecretName | default (include "iam-token-service.secretName" .) }}
      key: MIGRATION_DATABASE_URL
- name: RECONCILER_DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.existingSecretName | default (include "iam-token-service.secretName" .) }}
      key: RECONCILER_DATABASE_URL
- name: OPENBAO_ADDR
  value: {{ .Values.openbao.addr | quote }}
- name: OPENBAO_ROLE
  value: {{ .Values.openbao.authRole | quote }}
- name: OPENBAO_KV_MOUNT
  value: {{ .Values.openbao.kvMount | quote }}
- name: AWS_REGION
  value: {{ .Values.events.awsRegion | quote }}
- name: OTEL_SERVICE_NAME
  value: {{ .Values.otel.serviceName | quote }}
{{- if .Values.otel.exporterEndpoint }}
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: {{ .Values.otel.exporterEndpoint | quote }}
{{- end }}
{{- end }}
