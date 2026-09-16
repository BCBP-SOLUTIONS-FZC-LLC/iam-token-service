{{/*
Expand the name of the chart.
*/}}
{{- define "iam-token-service.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to
this (by the DNS naming spec).
*/}}
{{- define "iam-token-service.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "iam-token-service.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels applied to every resource.
*/}}
{{- define "iam-token-service.labels" -}}
helm.sh/chart: {{ include "iam-token-service.chart" . }}
{{ include "iam-token-service.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels — used by every Deployment, Service, PDB, and the CronJob
(so NetworkPolicy ingress/egress rules selecting on these labels cover the
CronJob's pods too).
*/}}
{{- define "iam-token-service.selectorLabels" -}}
app.kubernetes.io/name: {{ include "iam-token-service.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Component-scoped selector labels (server|consumer|rotator) — lets the
server Deployment, consumer Deployment, and rotator CronJob select their
own pods independently while sharing the base labels above.
*/}}
{{- define "iam-token-service.componentSelectorLabels" -}}
{{ include "iam-token-service.selectorLabels" . }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{/*
Create the name of the service account to use.
*/}}
{{- define "iam-token-service.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "iam-token-service.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Image tag: prefer .Values.image.tag; fall back to chart appVersion. One
image carries all three binaries (server + consumer + rotator) — the two
Deployments and the CronJob share this same tag.
*/}}
{{- define "iam-token-service.imageTag" -}}
{{- .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{/*
Egress rules shared by the server/consumer NetworkPolicies and the rotator
CronJob's own NetworkPolicy (§13, §10.5) — this service's outbound
dependencies are the same regardless of which workload is asking: DNS,
Postgres, OpenBao (Kubernetes auth + KV v2), AWS (SNS publish / Glue
schema lookups over HTTPS — no S3, unlike a sibling service's realm-export
path), and the OTel collector. No Keycloak egress (TS-INV-1: this service
has no Keycloak dependency at all).
*/}}
{{- define "iam-token-service.egressRules" -}}
- to:
    - namespaceSelector: {}
  ports:
    - port: 53
      protocol: UDP
    - port: 53
      protocol: TCP
- to:
    - namespaceSelector: {}
  ports:
    - port: {{ .Values.networkPolicy.postgresPort | default 5432 }}
      protocol: TCP
- to:
    - namespaceSelector: {}
  ports:
    - port: {{ .Values.networkPolicy.openbaoPort | default 8200 }}
      protocol: TCP
# AWS (SNS/SQS/Glue, §7/§13.5) — no cluster-internal destination to scope
# to, so this allows HTTPS egress cluster-wide-and-beyond rather than to a
# specific namespaceSelector, same as the OTel collector rule below.
- to: []
  ports:
    - port: 443
      protocol: TCP
{{- if .Values.otel.exporterEndpoint }}
- to:
    - namespaceSelector: {}
  ports:
    - port: {{ .Values.networkPolicy.otelPort | default 4317 }}
      protocol: TCP
{{- end }}
{{- with .Values.networkPolicy.additionalEgressRules }}
{{- toYaml . }}
{{- end }}
{{- end }}

{{/*
Name of the Secret holding application secrets: either a pre-existing,
externally-managed Secret (.Values.existingSecret), or the one this chart
renders itself from .Values.secretValues (see templates/secret.yaml). No
OpenBao credential is ever stored here (§10.5 — Kubernetes auth, not a
static token); this is only for anything the chart genuinely needs as a
K8s Secret (currently none at MVP — kept for forward compatibility).
*/}}
{{- define "iam-token-service.secretName" -}}
{{- default (printf "%s-secrets" (include "iam-token-service.fullname" .)) .Values.existingSecret }}
{{- end }}
