{{/*
Common env vars shared by all four binaries (server, consumer, rotator,
scheduler) — identity, logging, tracing, database pool and OpenBao (§12).
Every variable here is read by this service or by a shared library it calls
(platform-gincommon: APP_NAME, APP_ENV, OBSERVABILITY_DOMAIN, BUILD_VERSION,
the LOG_ and OTEL_ vars; platform-pgcommon: DATABASE_URL and the PG_ vars), matching
iam-org-membership. PG_STATEMENT_TIMEOUT is set per workload: the API path
and the CronJobs need different bounds. Each workload template appends its
own vars (SNS/Glue/outbox for server, SQS for consumer, prune/run tuning for
the CronJobs).
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
- name: LOG_SAMPLING
  value: {{ .Values.logSampling | default false | quote }}
- name: PG_BOUNCER_MODE
  value: {{ .Values.database.pgBouncerMode | quote }}
- name: PG_MAX_CONNS
  value: {{ .Values.database.pool.maxConns | quote }}
- name: PG_MIN_CONNS
  value: {{ .Values.database.pool.minConns | quote }}
- name: PG_SLOW_QUERY_THRESHOLD
  value: {{ .Values.database.pool.slowQueryThreshold | quote }}
- name: PG_LOCK_TIMEOUT
  value: {{ .Values.database.pool.lockTimeout | quote }}
- name: DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.existingSecretName | default (include "iam-token-service.secretName" .) }}
      key: DATABASE_URL
{{- if .Values.migrations.enabled }}
# Migrations run once per release in the migrate Job (job-migrate.yaml), so
# no long-running pod holds the migrator (table-owner, BYPASSRLS) DSN.
- name: RUN_MIGRATIONS
  value: "false"
{{- else }}
{{ include "iam-token-service.migrationDBEnv" . }}
{{- end }}
- name: OPENBAO_ADDR
  value: {{ .Values.openbao.addr | quote }}
- name: OPENBAO_ROLE
  value: {{ .Values.openbao.authRole | quote }}
- name: OPENBAO_KV_MOUNT
  value: {{ .Values.openbao.kvMount | quote }}
{{- if .Values.openbao.tokenAudience }}
- name: OPENBAO_K8S_TOKEN_PATH
  value: /var/run/secrets/openbao/token
{{- end }}
{{- if .Values.openbao.caCertSecret }}
- name: BAO_CACERT
  value: /etc/openbao-ca/ca.crt
{{- end }}
{{- /* No OTEL_SERVICE_NAME: InitTracingWithConfig takes the service name
from APP_NAME in code, so the variable is not read. */}}
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: {{ .Values.otel.exporterEndpoint | default "none" | quote }}
- name: OTEL_EXPORTER_OTLP_INSECURE
  value: {{ .Values.otel.insecure | quote }}
- name: OTEL_TRACES_SAMPLER_RATIO
  value: {{ .Values.otel.samplerRatio | quote }}
- name: OTEL_PROPAGATOR_BAGGAGE
  value: {{ .Values.otel.propagatorBaggage | quote }}
- name: OTEL_TRACES_IGNORE_REMOTE_PARENT_SAMPLED
  value: {{ .Values.otel.ignoreRemoteParentSampled | quote }}
{{- end }}

{{/*
RECONCILER_DATABASE_URL — the BYPASSRLS, SELECT-only enumeration role
(RLS-7). Only the server (DB-state exporters), rotator and scheduler use it;
the consumer never does.
*/}}
{{- define "iam-token-service.reconcilerDBEnv" -}}
- name: RECONCILER_DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.existingSecretName | default (include "iam-token-service.secretName" .) }}
      key: RECONCILER_DATABASE_URL
{{- end }}

{{/*
MIGRATION_DATABASE_URL — the migrator role (table owner, BYPASSRLS, direct
to Postgres past PgBouncer, TS-CONFIG-2). Injected only into the migrate
Job — or into every workload when migrations.enabled=false, which then
migrate at startup themselves.
*/}}
{{- define "iam-token-service.migrationDBEnv" -}}
- name: MIGRATION_DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.existingSecretName | default (include "iam-token-service.secretName" .) }}
      key: MIGRATION_DATABASE_URL
{{- end }}
