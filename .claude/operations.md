# Operations

Section numbers mirror the LLD's own numbering (`docs/lld/iam-lld-token-service.md`)
so this file stays traceable to the signed-off design. This is the
canonical source for exact metric/env-var names — README summarizes and
points here; the LLD is still the tie-breaker on any conflict.

## 10. Security

### Tenant isolation (three layers)

1. The reserved `iam-system` principal (`x-user-id`
   `00000000-0000-0000-0000-0000000000a1`) + `x-tenant-id` equal to the
   path tenant on every internal route (RLS-5) — otherwise `401`/`403`
   before any DB checkout.
2. `FORCE ROW LEVEL SECURITY` + default-deny on both tenant-scoped tables,
   keyed on the transaction-local `app.tenant_id` GUC (`SET LOCAL`, never
   plain `SET` — RLS-6).
3. The composite FK `(principal_id, tenant_id)` — a cross-tenant credential
   row is structurally impossible.

### Network and mesh isolation

The API authenticates callers only by the `x-user-id` header, which any
pod could set, so the caller restriction lives in the platform:

- **Istio `AuthorizationPolicy`** (`authorizationPolicy.*`) — required
  outside local/dev/test (`templates/validate.yaml` fails the render):
  `meshNamespaces` (RP, O&M, operator gateway) on every route,
  `keycloakNamespaces` on the JWKS route only, `workflowNamespaces` on TS-6
  only, and `/healthz`, `/readyz`, `/metrics` for probes/scrapes. Keycloak
  must be in the mesh. With it enabled the server and consumer are
  force-injected (native sidecars by default, `nativeSidecar`), and
  `strictMTLS` adds a STRICT `PeerAuthentication`.
- **NetworkPolicy** (`networkPolicy.enabled`, one policy per workload plus
  the migrate Job) — the render fails without `ingressNamespaceSelector`,
  `keycloakNamespaceSelector` (Keycloak's JWKS fetch, EXT-6),
  `monitoringNamespaceSelector` (metrics ingress from the monitoring
  namespace only) and `egress.postgresCIDRs`. Egress: DNS, Postgres
  (`postgresPort`, plus `postgresDirectPort` for direct DSNs), OpenBao,
  OTel, istiod (`istiodNamespaceSelector`/`istiodPort`), optional CIDR
  lists, and the Realm Provisioner (`realmProvisionerPort`) for the
  rotator and scheduler only. `workflowNamespaceSelector` optionally admits
  the Workflow Service for TS-6.

### Input validation

`rotation_id` must be a UUID; `overlap_seconds` clamped to `[0, 900]`;
`principal_sub` must be a UUID; `keycloak_client_id` must be
`platform-automation` or `platform-automation-<tenant_id>`. Path params
typed-parsed (`400`); request bodies capped at 1 MiB; a POST body must be
`application/json` (`415`).

### Authorization matrix

| Route | Callers |
|---|---|
| TS-4 register | Realm Provisioner |
| TS-1 issue/rotate | Realm Provisioner (issue at provisioning), operators / O&M tooling (rotate). `cmd/scheduler` rotates in-process through `CredentialService.IssueOrRotate`, not over HTTP (§16 TSQ-6 Resolved, TS-D14) |
| TS-2 revoke | Operators / O&M tooling (incl. break-glass, §8.7) |
| TS-3 read | Org & Membership, operators |
| TS-5 find-by-sub | Org & Membership (AUTH-9 non-member defense-in-depth, TS-D16) |
| TS-6 read platform-automation | Workflow Service connector workers (TS-D17) |
| JWKS (no header auth) | Keycloak's outbound client-jwt fetch (EXT-6), limited by the AuthorizationPolicy and rate limiters |

`cmd/rotator` and the offboarding consumer call no HTTP route — they drive
Postgres/OpenBao directly (RLS-7). No route accepts a tenant-facing
principal, and the automation principal holds no roles (TS-INV-4).

### Secret handling

OpenBao KV v2 is the **only** home for private-key plaintext
(`iam/serviceaccount/<tenant_id>/<keycloak_client_id>/v<version>`, frozen
§25). Kubernetes-auth login only (`openbao.Config` has no token field),
using `OPENBAO_K8S_TOKEN_PATH` — under Helm a projected ServiceAccount
token with audience `openbao` (`openbao.tokenAudience`); the OpenBao role
is bound to the four workload ServiceAccounts (`deploy/openbao/`).
`BAO_CACERT` (`openbao.caCertSecret`) for a private CA. The client
single-flights logins (detached from the caller's ctx), renews at
`lease - min(30s, lease/2)`, and re-logs in once on a 403. TS-1 returns
the private key only on issue or a replay inside `ROTATION_REPLAY_WINDOW`
(TS-3 is metadata-only); each replay is logged and counted. This service
never writes Keycloak (TS-INV-1, `no-gocloak` gate); the public half is
served by its own JWKS route (EXT-6). The `no-secret-log` gate (TS-INV-2)
rejects credential/secret/private-key names or a `-----BEGIN` literal in
log, fmt, error-constructor and span-attribute sinks, and runs its own
self-test first.

## 11. Observability

Service-level detail (identity, tiers, registration order, CI checks,
SLOs) and the alert runbooks live in
[`docs/observability/`](../docs/observability/README.md); its
`metric-registry.md` is generated (`make metrics-inventory`) and
drift-checked by `make metrics-lint`.

### SLOs

Recording rules + multi-window burn-rate alerts in
`deploy/monitoring/slo-rules.yml`, rendered in Helm as
`prometheusrule-slo.yaml`:

| SLO | Target | Note |
|---|---|---|
| SLO-1 | TS-1 issue/rotate p99 ≤ 500 ms | Fast/slow burn; the SLI includes the OpenBao write |
| SLO-2 | 99.9% write-path availability (TS-1/TS-2/TS-4 non-5xx) | Fast burn |
| SLO-3 | Overlap-expiry sweep success ≥ 99% | Operational proxy |
| SLO-4 | TS-3 read p99 ≤ 100 ms | Fast/slow burn |

### Metrics — Enterprise Platform Observability Standard

Every collector carries gincommon's `domain`/`service`/`environment`
const labels — call sites never set them. Registration order in every
binary: `InitTracingWithConfig` → `ObservabilityMiddlewares` →
`metrics.InitLibraryMetrics` (`events.InitMetrics`,
`pgmetrics.InitWithIdentity`) → `metrics.Register` (adopts platform-events'
shared collectors via `registerShared`) → pools.
`test/unit/metricsstandard` asserts the order and the identity labels.

**Tier 3 (`iam_token_service_*`):**

| Metric | Type | Labels | Source |
|---|---|---|---|
| `credentials_issued_total` | counter | `op` (issue/rotate/revoke) | credential-service decorator |
| `credential_replays_total` | counter | `result` (served/expired/revoked) | TS-1 replays (TS-D23) |
| `rotation_overlap_active` | gauge | — | server exporter |
| `openbao_call_duration_seconds` | histogram | `op` (write/delete) | dual-emitted with the Tier 1 dependency histogram |
| `offboarding_cascade_total` | counter | `result` (ok/error) | consumer, every message incl. rejects |
| `rotation_sweep_total` | counter | `result` (ok/error) — one per rotator run | rotator |
| `material_reconcile_total` | counter | `result` (orphan_deleted/missing_material/ok/error) | rotator |
| `cadence_rotation_total` | counter | `result` (rotated/skipped/failed) | scheduler |
| `keys_refresh_pending` | gauge | — | rows in `keys_refresh_pending`, intents included (server exporter) |
| `keys_refresh_oldest_age_seconds` | gauge | — | age of the oldest marker (server exporter) |
| `jwks_key_errors_total` | counter | — | unreadable live keys on the JWKS route |
| `jwks_rate_limited_total` | counter | — | any JWKS 429 |
| `jwks_rate_limited_by_bucket_total` | counter | `bucket` (tenant/global/unknown) | which limiter refused |
| `processed_events_duplicates_total` | counter | `consumer` | authoritative duplicate signal |
| `unknown_event_acknowledged_total` | counter | `consumer`, `event_type` (closed set, else `other`) | `ackUnknown` |
| `consumed_schema_violations_total` | counter | `consumer`, `event_type` | `validateConsumed` |
| `consumer_dlq_rejects_total` | counter | `reason` (schema_violation/invalid_envelope_id) | DLQ router |

**Tier 1/Tier 2 (see `docs/observability-registry-proposals.md`):**

| Metric | Tier | Labels | Status |
|---|---|---|---|
| `platform_dependency_request_seconds` | 1 | `dependency,operation,outcome` | Owned by platform-events; the service adds `dependency="openbao"` (write/delete/read/list) and `dependency="realm_provisioner",operation="refresh_keys"` |
| `platform_duplicate_messages_total` | 1 | `queue,event_type` | Counted only by platform-events' inbox |
| `iam_rls_violations_total` | 2 | `violation_type` (`cross_tenant_access`/`missing_or_invalid_guc`/`other`) | Emitted by `cmd/server`'s RLS exporter from `rls_violation_log`; every replica counts the same rows, so use `max`, not `sum` |
| `iam_offboarding_cascade_total` | 2 | `service,environment,outcome` | Proposed; not emitted until ratified |

Shared-library metrics: `platform_http_*` (`platform-gincommon`),
`platform_messages_*` / `platform_outbox_*` / `platform_dlq_messages_total`
/ `platform_queue_depth` / `platform_dlq_depth` (`platform-events`; the
consumer samples queue depth every `SQS_QUEUE_DEPTH_INTERVAL`), and
`platform_db_*` (`platform-pgcommon`, `pool` label `default` for the app
pool, `reconciler` for the BYPASSRLS pool). One identity
`{domain="iam", service="iam-token-service", environment=APP_ENV}` set on
`gincommon.Config` (`OBSERVABILITY_DOMAIN`). Pre-standard names were
removed; map old queries with platform-gincommon's
`docs/observability/migration.md`.

### Tracing

`gincommon.InitTracingWithConfig` in all four binaries (service name from
`APP_NAME`; `OTEL_EXPORTER_OTLP_ENDPOINT=none` keeps trace IDs but
disables export). HTTP spans from `ObservabilityMiddlewares`; DB spans
from `gincommon.NewSpanTracer`; spans `credential.issue_rotate`,
`credential.revoke`, `consumer.offboarding`, `rotator.overlap_sweep`,
`rotator.orphan_material`, `rotator.prune`, `scheduler.cadence_scan`,
`exporter.rotation_overlap`, `exporter.rls_violations`,
`exporter.keys_refresh_pending`; outbound RP-17 calls get client spans
and `traceparent` via `httpx`. `trace_id` goes onto produced event
envelopes and into job logs.

### Structured logs

Zap JSON via `platform-gincommon` (`LOG_LEVEL`, `LOG_SAMPLING`). No
credential field is ever a log attribute (`no-secret-log` gate, above).

### Alerts

`deploy/monitoring/app-alerts.yml`, mirrored by hand in
`deploy/helm/templates/prometheusrule.yaml` (29 alerts each); one runbook
per alert in `docs/observability/runbooks.md`. Several carry
`metric_status: proposed`.

| Alert | Severity | Fires on |
|---|---|---|
| `ServerDown` / `ConsumerDown` / `ServerReplicasMissing` | critical / warning / warning | scrape `up`, replica count |
| `RotatorNotRunning` / `SchedulerNotRunning` | warning | no successful CronJob run for 15 min |
| `CronJobRunFailed` | warning | the last scheduled rotator/scheduler run did not succeed |
| `HighErrorRate` / `HighLatency` | warning | server 5xx ratio > 5% / `platform_http_*` latency |
| `OpenBaoCallLatencyHigh` / `OpenBaoErrors` | warning | OpenBao latency / error ratio |
| `OffboardingCascadeFailures` | warning | `offboarding_cascade_total{result="error"}` |
| `ConsumedSchemaViolation` | critical | `consumed_schema_violations_total` > 0 |
| `OffboardingInvalidEnvelopeRejected` | critical | `consumer_dlq_rejects_total{reason="invalid_envelope_id"}` > 0 (tenant erasure not run — redrive) |
| `RotationSweepFailures` | warning | `rotation_sweep_total{result="error"}` |
| `OrphanMaterialMissing` | critical | `material_reconcile_total{result="missing_material"}` > 0 |
| `CadenceRotationFailures` | critical | `cadence_rotation_total{result="failed"}` > 0 (often an RP-17 failure; self-heals via the marker) |
| `JWKSKeyErrors` | critical | `jwks_key_errors_total` > 0 |
| `JWKSRateLimited` | warning | `jwks_rate_limited_by_bucket_total{bucket!="unknown"}` increase |
| `RotationOverlapStuck` | warning | `rotation_overlap_active` > 0 for 1h |
| `KeysRefreshBacklog` | warning | `keys_refresh_oldest_age_seconds` > 900 for 5m |
| `OutboxStuck` / `OutboxBacklogGrowing` | warning | `platform_outbox_*` |
| `SQSReceiveErrors` | warning | `platform_dependency_request_seconds{dependency="sqs",operation="receive_message",outcome="error"}` |
| `PostgresPoolExhaustion` | warning | `platform_db_pool_empty_acquires_total` growth, per `pool` |
| `OffboardingQueueBacklogGrowing` | warning | CloudWatch oldest-message age (needs an exporter) |
| `OffboardingQueueStalled` / `OffboardingDLQBacklog` | warning | `platform_queue_depth` > 0 for 15m / `platform_dlq_depth` > 0 for 30m |
| `RLSCrossTenantAccess` / `RLSMissingGUC` | critical / warning | `iam_rls_violations_total` increase |

All names are prefixed `IAMTokenService`. Schema-registry governance
alerts live separately in `deploy/monitoring/schema-registry-alerts.yml`.

## 12. Configuration

`.env-example` covers local development; the table below is the full
set the binaries read (directly or through the shared libraries). Helm
sets every one it needs from `values.yaml`.

| Var | Read by | Purpose (default) |
|---|---|---|
| `APP_ENV`, `APP_NAME`, `BUILD_VERSION`, `OBSERVABILITY_DOMAIN` | all | Environment (`dev`) / identity (`iam-token-service`, `iam`). Outside local/dev/development/test, `OPENBAO_ADDR`, `REALM_PROVISIONER_BASE_URL` (CronJobs), `GLUE_REGISTRY_NAME`, `SNS_TOPIC_ARN` (server) and `SQS_QUEUE_URL` (consumer) are required |
| `APP_PORT`, `METRICS_PORT` | server, consumer / all | API or health port (`8080`) / metrics port (`9090`) |
| `DATABASE_URL` or `PG_HOST`/`PG_PORT`/`PG_USER`/`PG_PASSWORD`/`PG_DBNAME`/`PG_SSLMODE` | all | App pool (`serviceaccount_app`) |
| `PG_MAX_CONNS`, `PG_MIN_CONNS`, `PG_BOUNCER_MODE`, `PG_SLOW_QUERY_THRESHOLD` (+ other `PG_*` pool settings in platform-pgcommon) | all | Pool sizing; copied into the reconciler pool |
| `PG_STATEMENT_TIMEOUT`, `PG_LOCK_TIMEOUT` | all | Per-transaction `SET LOCAL` on the app and reconciler pools, never the migration DSN. Helm: statement 5s server/consumer, 60s CronJobs; lock 2s |
| `RECONCILER_DATABASE_URL` | server, rotator, scheduler | `serviceaccount_reconciler` (BYPASSRLS, SELECT-only) pool; falls back to the app DSN with a warning |
| `MIGRATION_DATABASE_URL` | all (when migrating) | Direct-port migrator DSN, bypasses PgBouncer (`pg_advisory_lock`) |
| `RUN_MIGRATIONS` (`true`), `MIGRATE_ONLY` (`false`) | all / server | Migrate at startup / migrate then exit. Helm: hook Job with `MIGRATE_ONLY=true`, `RUN_MIGRATIONS=false` on workloads |
| `SHUTDOWN_DRAIN_DELAY` (`5s`) | server, consumer | `/readyz` 503 for this long after SIGTERM before shutting down |
| `OPENBAO_ADDR`, `OPENBAO_ROLE` (`iam-token-service`), `OPENBAO_KV_MOUNT` (`iam`) | server, consumer, rotator, scheduler | Kubernetes-auth login + KV v2 mount |
| `OPENBAO_K8S_TOKEN_PATH`, `BAO_CACERT` | same | Login JWT path (default the pod SA token; Helm: projected `openbao`-audience token) / private CA (read by the OpenBao SDK) |
| `AWS_REGION` (`ap-south-1`), `AWS_ENDPOINT_URL` | server, consumer | AWS SDK region / local endpoint (Floci) |
| `GLUE_REGISTRY_NAME`, `SNS_TOPIC_ARN` | server | Produced events (empty = no-op codec / publisher; required outside dev) |
| `OUTBOX_POLL_INTERVAL` (`500ms`), `OUTBOX_BATCH_SIZE`, `OUTBOX_MAX_ATTEMPTS`, `OUTBOX_DRAIN_TIMEOUT`, `OUTBOX_PUBLISH_CONCURRENCY` (`4`), `OUTBOX_PUBLISH_TIMEOUT`, `OUTBOX_STARTUP_JITTER` (`2s`), `OUTBOX_CLAIM_LEASE_DURATION` (`10m`) | server | platform-events outbox runner (service defaults in parentheses) |
| `SQS_QUEUE_URL`, `SQS_CONCURRENCY` (`2`), `SQS_MAX_MESSAGES`, `SQS_WAIT_SECONDS`, `SQS_VISIBILITY_TIMEOUT` (`60s`), `SQS_HANDLER_TIMEOUT` (`45s`), `SQS_DRAIN_TIMEOUT` (`15s`), `SQS_QUEUE_DEPTH_INTERVAL` (`60s`, `0s` disables) | consumer | The one inbound subscription (platform-events `config.LoadSQS`) |
| `ROTATION_DEFAULT_OVERLAP_SECONDS` (`300`) | server, scheduler | TS-1 default overlap / scheduler overlap; startup fails outside `[0,900]` |
| `ROTATION_DEFAULT_CADENCE_DAYS` (`90`) | server, scheduler | Stamped into `next_rotation_at` |
| `ROTATION_REPLAY_WINDOW` (`15m`, `0` = unlimited) | server | TS-1 replay window → `409 credential_replay_expired`; startup fails on a bad value |
| `JWKS_RATE_LIMIT_RPS`/`_BURST` (`20`/`40`) | server | Global bucket, known tenants |
| `JWKS_RATE_LIMIT_PER_TENANT_RPS`/`_BURST` (`5`/`10`) | server | Per-tenant buckets (LRU 10,000) |
| `JWKS_RATE_LIMIT_UNKNOWN_TENANT_RPS`/`_BURST` (`2`/`5`) | server | Shared bucket for tenants not known |
| `JWKS_KNOWN_TENANTS_REFRESH` (`15s`) | server | Known-tenant DB reload; startup fails on a bad value |
| `ROTATION_OVERLAP_GAUGE_INTERVAL` (`30s`), `RLS_VIOLATION_EXPORTER_INTERVAL` (`1m`), `KEYS_REFRESH_EXPORTER_INTERVAL` (`30s`) | server | Exporter cadences |
| `DOCS_ENABLED` (`false`), `DOCS_AUTH_TOKEN` | server | Docs routes outside local/dev/test only when enabled; startup fails if enabled there without a token |
| `REALM_PROVISIONER_BASE_URL` | rotator, scheduler | RP-17 target |
| `ROTATOR_RUN_TIMEOUT`/`SCHEDULER_RUN_TIMEOUT` (`180s`) | rotator / scheduler | In-process run budget (below `activeDeadlineSeconds` 240) |
| `ROTATOR_BATCH_LIMIT`/`SCHEDULER_BATCH_LIMIT` (`500`) | rotator / scheduler | Rows per sweep / scan and markers per retry pass |
| `ROTATOR_METRICS_SCRAPE_GRACE`/`SCHEDULER_METRICS_SCRAPE_GRACE` (`15s`) | rotator / scheduler | Post-run `/metrics` window |
| `PROCESSED_EVENTS_TTL_DAYS` (`8`), `PRUNE_BATCH_LIMIT` (`10000`), `OUTBOX_PRUNE_OLDER_THAN` (`168h`), `RLS_VIOLATION_LOG_TTL_DAYS` (`30`, function minimum 7) | rotator | Retention prunes |
| `OTEL_EXPORTER_OTLP_ENDPOINT` (`none` disables export), `OTEL_EXPORTER_OTLP_INSECURE`, `OTEL_TRACES_SAMPLER_RATIO`, `OTEL_PROPAGATOR_BAGGAGE`, `OTEL_TRACES_IGNORE_REMOTE_PARENT_SAMPLED` | all | gincommon tracing (`OTEL_SERVICE_NAME` is not used — `APP_NAME` is) |
| `LOG_LEVEL`, `LOG_SAMPLING` | all | gincommon logging |
| `GOMEMLIMIT` | all (Helm) | Per-workload `goMemLimit` |

Not configurable: OpenBao HTTP timeout 8s, in-lock OpenBao bound 5s,
RP-17 client 3s × 2 attempts, readiness checks 2s, HTTP request timeout
30s.

## 13. Deployment and scaling

**Topology:** one distroless image, four binaries. `Deployment` × 2
(server, consumer — `replicaCount: 2` for HA, PDB `minAvailable: 1`),
`CronJob` × 2 (rotator, scheduler — `*/5 * * * *`, `concurrencyPolicy:
Forbid`, `activeDeadlineSeconds: 240`, in-process run timeout 180s so the
graceful exit always wins), and a pre-install/pre-upgrade migrate hook
`Job` (server image, `MIGRATE_ONLY=true`, weight 0). Its prerequisites —
the migrate ServiceAccount, the chart Secret and the migrate NetworkPolicy
— are hooks at weight -10 (`before-hook-creation`, kept afterwards).
`server`/`consumer` connect as `serviceaccount_app`; the server also opens
the reconciler pool (exporters, JWKS known tenants); `rotator`/`scheduler`
enumerate as `serviceaccount_reconciler` and write as `serviceaccount_app`.

**Grace and probes:** server `terminationGracePeriodSeconds` 80, consumer
60, CronJobs 45 (SIGTERM finishes the current tenant). Probes use
`timeoutSeconds: 3`; readiness checks run with a 2s deadline. Pod
anti-affinity is required at a fixed replica count and preferred when the
HPA (`autoscaling.enabled`, default false; RPS rule in
`deploy/monitoring/prometheus-adapter-rule.yaml`) is on.

**Identity:** `serviceAccount.perWorkload` (default true:
`<fullname>-server/-consumer/-rotator/-scheduler/-migrate`) with
`workloadAnnotations.<component>` for IRSA. Only the server
(`deploy/iam/policy-server.json`: SNS publish, Glue read) and consumer
(`policy-consumer.json`: queue consume, DLQ `SendMessage`) need AWS
roles; rotator, scheduler and migrate need none. The OpenBao role is bound
to the four workload ServiceAccounts with audience `openbao`.

**Istio:** with `authorizationPolicy.enabled`, server and consumer are
force-injected (native sidecars by default); CronJob pods get a sidecar
only with `cronjobs.istioInject` (default false, then a native sidecar);
the migrate Job is never injected.

**Monitoring:** ServiceMonitor for server/consumer and a PodMonitor
(`serviceMonitor.podMonitorInterval`, 5s) for the CronJob pods, both with
`honorLabels: true`; `PrometheusRule` for alerts and a second one for SLOs.

**Helm values (`deploy/helm/values.yaml`):** `image.{repository,tag,digest}`
(deploy by digest); `server`/`consumer`/`rotator`/`scheduler`
`{replicaCount|schedule, terminationGracePeriodSeconds, goMemLimit,
resources}`, `rotator`/`scheduler.{runTimeout, metricsScrapeGrace,
batchLimit, activeDeadlineSeconds}`, `rotator.{pruneBatchLimit,
outboxPruneOlderThan}`; `migrations.{enabled, backoffLimit,
activeDeadlineSeconds}`; `shutdown.drainDelay`; `cronjobs.istioInject`;
`exporters.{rotationOverlapInterval, rlsViolationInterval,
keysRefreshInterval}`; `networkPolicy.*` and `authorizationPolicy.*` (see
§10); `database.{pgBouncerMode, existingSecretName, pool.{maxConns,
minConns, slowQueryThreshold, lockTimeout, statementTimeout,
jobStatementTimeout}}`; `openbao.{addr, authRole, kvMount, tokenAudience,
caCertSecret}`; `realmProvisioner.baseUrl`;
`rotation.{defaultOverlapSeconds, defaultCadenceDays, replayWindow}`;
`jwks.{rateLimitRPS, rateLimitBurst, perTenantRPS, perTenantBurst,
unknownTenantRPS, unknownTenantBurst, knownTenantsRefresh}`;
`events.{glueRegistryName, topicArn, awsRegion}`; `outbox.*`;
`sqs.{queueUrl, concurrency, maxMessages, waitSeconds,
visibilityTimeoutSeconds, queueDepthInterval, handlerTimeout,
drainTimeout}`; `processedEvents.ttlDays`; `rlsViolationLog.ttlDays`;
`docs.{enabled, authEnabled}`; `otel.*`; `appEnv`, `observabilityDomain`,
`logLevel`, `logSampling`; `serviceMonitor.*`; `ingress.*` (HTTPRoute +
optional Envoy `securityPolicy`); `autoscaling.*`; `podDisruptionBudget.*`;
`existingSecret`/`secretValues`/`rotationEpoch` (bump to roll pods after
an external secret rotation).

**Render guards** (`templates/validate.yaml`, outside local/dev/test):
`authorizationPolicy.enabled` with `meshNamespaces` and
`keycloakNamespaces`; `events.topicArn`; `sqs.queueUrl`; `docs.enabled`
requires `docs.authEnabled`. Always: `perWorkload: false` with
`workloadAnnotations` (other than `migrate`) is rejected. The
NetworkPolicy selectors/CIDRs and `secretValues` are guarded in their own
templates.

**Release (CI):** `release.yml` must run from a tag: candidate image →
scan → smoke → sign → publish tags; `schema-registry.yml` (workflow_call)
registers schemas before the deploy gate (pre-releases skip both); deploy
is by digest with `HELM_VALUES_B64`. All actions are SHA-pinned;
Dependabot is enabled.

## 14. Testing strategy

| Suite | Location | Proves |
|---|---|---|
| Unit | `test/unit/**` + white-box `*_test.go` in `cmd/*` and `internal/**` | Business logic with hand-written fakes, no I/O; `repository_errors_test.go` drives every driver-error branch with a fake `pgx.Tx` |
| Contract | `test/contract` | No secret in any event payload |
| Postgres | `test/postgres` (`-tags=integration`) | Real Postgres via testcontainers: RLS-6/RLS-7, lock timeouts and concurrency, migrations round-trip (incl. `000004`), inbox/consumer, RLS log, JWKS tenants, `keys_refresh_pending` |
| Integration | `test/integration` (`-tags=integration`) | Real OpenBao + fake Kubernetes TokenReview server; Glue codec |
| Metrics standard | `test/unit/metricsstandard` | Registration order, identity labels; writes the scrape for `metricslint` when `METRICS_SCRAPE_OUT` is set |
| E2E | `test/e2e` (`-tags=e2e`, `make test-e2e`) | Real router + real Postgres |
| Smoke | `make test-smoke` | A built image per binary |

`make test-ci` = merged-coverage pipeline (`-coverpkg` over `internal/...`
and `pkg/...`; `cmd/` excluded because `main()` can't be unit-invoked).
CI fails below 98% (`coverage-gate.sh`).

**CI** (`.github/workflows/validate-quality.yml`): fmt/tidy/vet, `make
lint`, `make swag-check`, `make gates` (`no-gocloak`, `no-secret-log`,
`set-local-only`, `gincommon-obs` — which also rejects
`promhttp.Handler()`/`InitTracingFromEnv()` — and `metrics-taxonomy`),
`make metrics-lint`, govulncheck, `gosec`, Dockerfile digest pin.
`validate-test.yml`: `make test-ci` + coverage gate, `go-arch-lint`,
swagger staleness, AsyncAPI → JSON schema sync, e2e. `make ci` rehearses
the quality + test path locally.

## 18. Integration points

| Service | Relationship |
|---|---|
| Realm Provisioner | Calls TS-4 and TS-1 (discards the private key, EXT-6); serves RP-17 (`POST …/tenants/:id/service-account/keys/refresh`), which this service's CronJobs call after their own revokes/rotations |
| Org & Membership / operators | TS-3 (read metadata), TS-5 (find-by-sub, AUTH-9) |
| Workflow Service | TS-6 (the tenant's automation `principal_sub`) |
| O&M / Core | Publishes `TenantMembershipsPurged` — the one inbound subscription |
| Audit Log | Subscribes to `iam-serviceaccount-events` (5 events, audit-only) |
| Keycloak | Fetches this service's JWKS (EXT-6); this service never calls Keycloak (TS-INV-1) |

## 20. Operational considerations

- **Outbox health:** `platform_outbox_pending_events` is the primary
  bus-health signal; growth means the SNS relay is stalled (credential
  operations still succeed — only audit emission is delayed, EVT-4).
  Poison events land in `outbox_dead_letters`.
- **Stuck `rotating` versions:** `rotation_overlap_active` should fall
  back after each overlap; non-zero beyond a sweep interval means
  `cmd/rotator` isn't running or is failing OpenBao deletes.
- **RP-17 backlog:** `keys_refresh_pending` / oldest age > 15 min means
  RP-17 keeps failing; revoked or superseded keys may still authenticate
  at Keycloak until it clears. Before rolling back `000005`, trigger RP-17
  by hand for every listed tenant.
- **Concurrent-revoke races:** benign by design (TS-D13) — reported as
  the already-achieved outcome or `Skipped`, not a failure.
- **`rotation_in_flight` spikes:** another write held the principal lock
  past `PG_LOCK_TIMEOUT`; callers retry.

## 21. Performance

Small service by design (§21 of the LLD) — 2 replicas cover HA, not load.
The HPA is optional (`autoscaling.enabled`, default false). Scaling
triggers, if ever needed, would be tenant count and rotation cadence;
CronJob throughput is bounded per run by `*_BATCH_LIMIT` and the 180s
budget, with the rest deferred to the next 5-minute tick.
