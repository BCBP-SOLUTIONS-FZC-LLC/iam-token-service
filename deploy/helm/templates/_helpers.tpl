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
ServiceAccount for one workload. With serviceAccount.perWorkload (default)
each binary runs as its own ServiceAccount — <fullname>-<component> — so its
IRSA role and OpenBao role carry only that binary's permissions (the
consumer cannot publish to SNS, the CronJobs reach no AWS API at all).
Call with (dict "component" "server" "root" $).
*/}}
{{- define "iam-token-service.workloadServiceAccountName" -}}
{{- if and .root.Values.serviceAccount.create .root.Values.serviceAccount.perWorkload }}
{{- printf "%s-%s" (include "iam-token-service.fullname" .root) .component | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- include "iam-token-service.serviceAccountName" .root }}
{{- end }}
{{- end }}

{{/*
ServiceAccount of the migrate hook Job. Always its own <fullname>-migrate
(with perWorkload true or false): the Job is a pre-install hook, so its
ServiceAccount must be a hook too (serviceaccount.yaml) — the release's
ordinary resources, the shared ServiceAccount included, do not exist yet
when it runs on a first install. With serviceAccount.create=false it uses
the externally managed ServiceAccount like every other workload.
*/}}
{{- define "iam-token-service.migrateServiceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- printf "%s-migrate" (include "iam-token-service.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- include "iam-token-service.serviceAccountName" . }}
{{- end }}
{{- end }}

{{/*
Annotations that run a pre-install/pre-upgrade prerequisite of the migrate
Job (its ServiceAccount, Secret, NetworkPolicy) before it: weight -10 sorts
them ahead of the Job's 0, and before-hook-creation replaces the previous
release's copy rather than failing on "already exists". They are kept after
the hook (not hook-succeeded) because the workloads and later upgrades use
them too.
*/}}
{{- define "iam-token-service.migrateHookPrereqAnnotations" -}}
helm.sh/hook: pre-install,pre-upgrade
helm.sh/hook-weight: "-10"
helm.sh/hook-delete-policy: before-hook-creation
{{- end }}

{{/*
Pod annotations for the rotator/scheduler CronJob pods' Istio sidecar
(cronjobs.istioInject, see values.yaml). Off: no sidecar (a classic sidecar
would keep the Job from ever completing). On: a native sidecar, which ends
with the main container.
*/}}
{{- define "iam-token-service.cronjobIstioAnnotations" -}}
{{- if .Values.cronjobs.istioInject }}
sidecar.istio.io/inject: "true"
sidecar.istio.io/nativeSidecar: "true"
{{- else }}
sidecar.istio.io/inject: "false"
{{- end }}
{{- end }}

{{/*
Pod annotations for the server/consumer Istio sidecar. Only rendered with
authorizationPolicy.enabled: the policy is enforced by the server pod's own
sidecar, so injection is forced on rather than left to a namespace label
(an uninjected pod ignores the policy entirely). The consumer serves no API,
but is injected too so its egress (and any mesh-side policy on it) behaves
like the server's. Native sidecar by default; otherwise a classic sidecar
whose drain outlasts the app's own (grace - 10s), so outbound calls keep
working through the outbox/SQS drain. See values.yaml authorizationPolicy.
Call with (dict "grace" <terminationGracePeriodSeconds> "root" $).
*/}}
{{- define "iam-token-service.meshPodAnnotations" -}}
{{- if .root.Values.authorizationPolicy.enabled }}
sidecar.istio.io/inject: "true"
{{- if .root.Values.authorizationPolicy.nativeSidecar }}
sidecar.istio.io/nativeSidecar: "true"
{{- else }}
proxy.istio.io/config: {{ printf "{\"terminationDrainDuration\":\"%ds\"}" (max 5 (sub (.grace | int) 10)) | squote }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Restart-on-change annotation for every workload pod: the chart-rendered
Secret's checksum when the chart renders it (templates/secret.yaml), so a
changed secretValues rolls the pods; else rotationEpoch, bumped by hand after
an external Secret rotates.
*/}}
{{- define "iam-token-service.secretRestartAnnotation" -}}
{{- if and (not .Values.existingSecret) (not .Values.database.existingSecretName) }}
checksum/secret: {{ include (print .Template.BasePath "/secret.yaml") . | sha256sum }}
{{- else }}
rotationEpoch: {{ .Values.rotationEpoch | default "0" | quote }}
{{- end }}
{{- end }}

{{/*
Container image: repository@digest when image.digest is set (what the
release pipeline signs and admission policies can verify), else
repository:tag.
*/}}
{{- define "iam-token-service.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (include "iam-token-service.imageTag" .) }}
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
{{- $egress := .Values.networkPolicy.egress | default dict }}
- to:
    {{- include "iam-token-service.egressPeers" (dict "cidrs" $egress.dnsCIDRs) | nindent 4 }}
  ports:
    - port: 53
      protocol: UDP
    - port: 53
      protocol: TCP
# Postgres. RDS (and any managed database) is OFF-cluster: a
# namespaceSelector never matches it, so its address range must be listed in
# networkPolicy.egress.postgresCIDRs — the render fails without it.
- to:
    {{- include "iam-token-service.egressPeers" (dict "cidrs" $egress.postgresCIDRs) | nindent 4 }}
  ports:
    {{- include "iam-token-service.postgresEgressPorts" . | nindent 4 }}
- to:
    {{- include "iam-token-service.egressPeers" (dict "cidrs" $egress.openbaoCIDRs) | nindent 4 }}
  ports:
    - port: {{ .Values.networkPolicy.openbaoPort | default 8200 }}
      protocol: TCP
# AWS (SNS/SQS/Glue, §7/§13.5) — no cluster-internal destination to scope
# to, so this allows HTTPS egress cluster-wide-and-beyond.
- to: []
  ports:
    - port: 443
      protocol: TCP
{{- if .Values.otel.exporterEndpoint }}
- to:
    {{- include "iam-token-service.egressPeers" (dict "cidrs" $egress.otelCIDRs) | nindent 4 }}
  ports:
    - port: {{ .Values.networkPolicy.otelPort | default 4317 }}
      protocol: TCP
{{- end }}
{{- if or .Values.authorizationPolicy.enabled .Values.cronjobs.istioInject }}
# istiod (xDS/CA on 15012) for the Istio sidecar injected when
# authorizationPolicy.enabled (server/consumer) or cronjobs.istioInject: without
# it the proxy never receives its config or certificate and the pod never
# becomes ready. Rendered for every workload policy — harmless for an
# uninjected pod, which opens no such connection.
- to:
    - namespaceSelector:
        {{- .Values.networkPolicy.istiodNamespaceSelector | default (dict "matchLabels" (dict "kubernetes.io/metadata.name" "istio-system")) | toYaml | nindent 8 }}
  ports:
    - port: {{ .Values.networkPolicy.istiodPort | default 15012 }}
      protocol: TCP
{{- end }}
{{- with .Values.networkPolicy.additionalEgressRules }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Postgres egress ports: postgresPort (DATABASE_URL, through PgBouncer when
pgBouncerMode) plus postgresDirectPort when it differs — the direct DSNs
(RECONCILER_DATABASE_URL for the server's exporters, the rotator and the
scheduler; MIGRATION_DATABASE_URL for the migrate Job) bypass PgBouncer.
*/}}
{{- define "iam-token-service.postgresEgressPorts" -}}
{{- $pgPort := .Values.networkPolicy.postgresPort | default 5432 | int }}
{{- $directPort := .Values.networkPolicy.postgresDirectPort | default 5432 | int }}
- port: {{ $pgPort }}
  protocol: TCP
{{- if ne $pgPort $directPort }}
- port: {{ $directPort }}
  protocol: TCP
{{- end }}
{{- end }}

{{/*
Egress peers: any in-cluster namespace, plus the given off-cluster CIDRs
(a namespaceSelector never matches an address outside the cluster).
*/}}
{{- define "iam-token-service.egressPeers" -}}
- namespaceSelector: {}
{{- range .cidrs }}
- ipBlock:
    cidr: {{ . | quote }}
{{- end }}
{{- end }}

{{/*
Prometheus scrape ingress: the metrics port only, from the monitoring
namespace(s) — never from the mesh (networkPolicy.monitoringNamespaceSelector).
*/}}
{{- define "iam-token-service.metricsIngress" -}}
- from:
    - namespaceSelector:
        {{- .Values.networkPolicy.monitoringNamespaceSelector | toYaml | nindent 8 }}
      {{- with .Values.networkPolicy.monitoringPodSelector }}
      podSelector:
        {{- toYaml . | nindent 8 }}
      {{- end }}
  ports:
    - port: metrics
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

{{/*
OpenBao login material. With openbao.tokenAudience set, the pod logs in with
a projected ServiceAccount token issued for that audience only, so the
default API-audience token is never sent to OpenBao, and a token captured on
OpenBao's side cannot be replayed against the Kubernetes API.
openbao.caCertSecret mounts a private CA for OPENBAO_ADDR (key ca.crt), which
the SDK reads via BAO_CACERT. Both expand to nothing when unset.
*/}}
{{- define "iam-token-service.openbaoVolumeMounts" -}}
{{- if .Values.openbao.tokenAudience }}
- name: openbao-token
  mountPath: /var/run/secrets/openbao
  readOnly: true
{{- end }}
{{- if .Values.openbao.caCertSecret }}
- name: openbao-ca
  mountPath: /etc/openbao-ca
  readOnly: true
{{- end }}
{{- end }}

{{- define "iam-token-service.openbaoVolumes" -}}
{{- if .Values.openbao.tokenAudience }}
- name: openbao-token
  projected:
    sources:
      - serviceAccountToken:
          path: token
          audience: {{ .Values.openbao.tokenAudience | quote }}
          expirationSeconds: 3600
{{- end }}
{{- if .Values.openbao.caCertSecret }}
- name: openbao-ca
  secret:
    secretName: {{ .Values.openbao.caCertSecret | quote }}
    items:
      - key: ca.crt
        path: ca.crt
{{- end }}
{{- end }}
