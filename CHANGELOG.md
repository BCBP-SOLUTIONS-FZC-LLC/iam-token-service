# Changelog

All notable changes to this service are documented here. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning aligned with SemVer.

This service has never been deployed to any environment — there is no released version yet, so everything lives under `[Unreleased]`.

## [Unreleased]

### Fixed (third production-readiness pass, TS-D23)

- **JWKS regression from TS-D22.** "Known tenant" meant served recently on this replica, so after a restart or 10 idle minutes Keycloak's real fetches shared the 2 rps unknown-tenant bucket with any attacker. Known tenants now come from the database (tenants with a live credential, reloaded every `JWKS_KNOWN_TENANTS_REFRESH`, 15s).
- **JWKS responses.** A set without the active key is no longer served as 200. The route returns the new `503 jwks_keys_unavailable` (it also replaces that route's use of `secret_store_unavailable`, whose frozen status is 502). 429s are counted by bucket (`jwks_rate_limited_by_bucket_total`).
- **Refresh markers.**
  - A concurrent rotator sweep could clear the scheduler's intent marker before its rotation committed. The scheduler now re-marks after `IssueOrRotate` and after a failed RP-17.
  - Intent markers carry `intent_until` (migration 000005); committed markers are retried on the very next run.
  - The backlog is exported (`keys_refresh_pending`, `keys_refresh_oldest_age_seconds`) and alerted (`IAMTokenServiceKeysRefreshBacklog`).
- **Locks.**
  - The TS-2 and orphan-reclaim OpenBao calls under the principal lock are bounded at 5s.
  - `missing_material` is confirmed under the lock, so an in-flight revoke no longer pages.
  - A lock-timeout `rotation_in_flight` carries `details.active_rotation_id`.
  - The scheduler treats `principal_not_found` as skipped.
- **Audit and API.**
  - `rotation_id` replays are logged and counted (`credential_replays_total`).
  - `x-tenant-roles` is ignored instead of causing a generic 401.
  - Docs in staging (any non-dev env) need `DOCS_ENABLED` and the token.
  - Swagger requires both identity headers.
  - `/readyz` checks have a 2s deadline.
- **Consumer.** Straight-to-DLQ sends are counted (`consumer_dlq_rejects_total{reason}`) and logged, and `invalid_envelope_id` pages (`IAMTokenServiceOffboardingInvalidEnvelopeRejected`).
- **Helm.**
  - With the AuthorizationPolicy on, server and consumer are force-injected with Istio (native sidecars by default, so Envoy outlives the drain). A STRICT `PeerAuthentication` covers the server, and istiod egress is allowed.
  - `networkPolicy.postgresDirectPort` opens the direct reconciler DSN port.
  - The hook Secret carries release ownership, and every workload restarts on a secret checksum change.
  - Readiness timeout is 3s.
  - `workloadAnnotations.migrate` is allowed with `perWorkload: false`.
- **Alerts.**
  - Outage alerts carry `environment`.
  - `CronJobRunFailed` fires on the latest failed run.
  - New: `IAMTokenServiceOpenBaoErrors`.
  - `JWKSRateLimited` ignores the unknown bucket.
  - Runbook corrections.
- **Release/CI.**
  - Pre-release tags skip production schema registration and the deploy gate.
  - A dispatched release must run from its tag.
  - CI smoke-tests the pushed digest before signing.
  - The CI role gains `glue:DeleteSchema` and `cloudwatch:PutMetricAlarm`.
- **Tests.** Two timing-dependent tests now use barriers or attempt counts.

### Fixed (second production-readiness pass, TS-D22)

- **Key-cache refreshes could be lost silently.** The rotator and scheduler ran every row first and called RP-17 afterwards, outside their run budget, with no SIGTERM handling. A run killed mid-flush left revoked keys trusted by Keycloak with no record. Now:
  - RP-17 runs inline per tenant, and a tenant starts only with 30s of budget left.
  - A durable `keys_refresh_pending` marker (migration `000005`) is retried at the start of every run.
  - SIGTERM finishes the current tenant before exit.
- **Rotator phases had no budget.** The orphan reconciler and prunes defer instead of failing when time runs out. The reconciler starts at a rotating offset and reclaims orphan material under the principal row lock (principals with no rows included). The sweep re-reads its row inside the retried transaction.
- **Lock timeouts were 500s.** SQLSTATE `55P03` is `409 rotation_in_flight` everywhere. TS-1 generates its key outside the lock and bounds the in-lock OpenBao write at 5s.
- **JWKS.** Unknown tenants have their own small bucket (`JWKS_RATE_LIMIT_UNKNOWN_TENANT_RPS`/`_BURST`, 2/5), so a random-UUID flood can't 429 Keycloak. Per-tenant buckets are LRU. A fully unreadable key set is `503`, not `200 {"keys":[]}`. Cancelled requests no longer count as key errors.
- **API contract.** Missing identity headers return the frozen `missing_identity_headers` code. TS-1 responses are `no-store`. A `rotation_id` replay after `ROTATION_REPLAY_WINDOW` (default 15m) returns the new `409 credential_replay_expired`. Swagger lists the real headers and error responses.
- **OpenBao login** is single-flight, detached from the caller's context, with a margin of `min(30s, lease/2)`.
- **Offboarding.** A `TenantMembershipsPurged` with a missing or non-UUID envelope id goes to the DLQ (`invalid_envelope_id`) instead of being acked, which skipped the erasure. Unknown event-type labels fold to `other`.
- **Server** refuses to start in prod with docs enabled and no `DOCS_AUTH_TOKEN`.
- **Helm:**
  - Outside local/dev/test the render requires the Istio `AuthorizationPolicy`: the API's identity header can be set by any pod that reaches the port, and the NetworkPolicy admits Keycloak's namespace to it. It also requires `events.topicArn` and `sqs.queueUrl`.
  - The migrate ServiceAccount, dev Secret and a new migrate NetworkPolicy are pre-install hooks, so a first install no longer fails.
  - The scheduler's resources are no longer `null`.
  - CronJob grace is 45s; Jobs opt out of Istio sidecars (`cronjobs.istioInject`).
  - Server anti-affinity is preferred when the HPA is on.
- **Monitoring:**
  - `honorLabels: true` on the ServiceMonitor and PodMonitor; four alerts could never fire without it.
  - SLO-3 counts runs (it fired permanently).
  - Alerts keep the `environment` label.
  - `ServerReplicasMissing` follows the replica count.
  - The runbooks are corrected (`max_receive_count`, new DLQ reasons, JWKS 503, `keys_refresh_pending`).
- **CI/release:**
  - Production Glue schemas are registered from `release.yml` before the deploy gate; a `GITHUB_TOKEN` release never triggered `schema-registry.yml`.
  - `ci.yml` and `release.yml` tag images only after scan, sign and smoke.
  - Trivy/smoke build the same image as push (`SOURCE_DATE_EPOCH`).
  - First-party actions are SHA-pinned, and secrets moved out of `run:` scripts.
- **TS-INV-2 gate** also checks error constructors, span attributes, PEM/private-key names and `-----BEGIN` literals, and runs its own self-test.

### Security

- **GO-2026-6443 (gRPC server panic on a missing authority/Host header) — tracked, not yet upgradable.** govulncheck lists it as an imported package, but no code path here reaches it: the service runs no gRPC server, and gRPC is only the OTLP trace exporter's client transport (`otlptracegrpc`, via platform-gincommon). The fix exists only in an unreleased `v1.85.0-dev` build, so we stay on the released `google.golang.org/grpc v1.84.0` and bump to v1.85.0 once it is tagged; Dependabot's gomod updates will raise it. (GO-2026-5932, `golang.org/x/crypto/openpgp`, is in a required module but in a package this service never imports.)

### Fixed

- **Production-readiness hardening pass (TS-D21).** No frozen name changes; every new setting, metric, alert and error code is additive.
  - **Concurrent rotation could corrupt a credential.** Two TS-1 calls with different `rotation_id`s could pick the same next version and overwrite each other's OpenBao material, so the committed row could point at the other caller's key. Issue/rotate, revoke and offboarding now serialize on `SELECT … FOR UPDATE` of the principal row; a caller that cannot get it within `PG_LOCK_TIMEOUT` gets `409 rotation_in_flight`.
  - **Issue after revoke failed.** The next version came from the highest non-revoked row, so it reused a revoked version number and hit `uq_sac_version`. It is now `MaxVersion + 1` over every row. Older open overlaps are closed when a new rotation commits (TS-INV-3).
  - **Scheduler vs operator rotation race.** The scheduler passes `ExpectActiveVersion`; a principal rotated meanwhile is skipped, not rotated twice.
  - **Rotator/scheduler runs could overrun their deadline or hide failures.** The runs are batched (`ROTATOR_BATCH_LIMIT`/`SCHEDULER_BATCH_LIMIT`, default 500) and stop 20s before the deadline, deferring the rest. RP-17 is called once per tenant, after that tenant's commits, on a detached context. A list, reconcile, RP-17 or prune failure now fails the Job.
  - **Orphan reconciler.** It now lists the prefixes taken from stored paths (a client-id change no longer hides material), deletes material that survived a revoke, and reports `missing_material` only for live rows after a re-check.
  - **Outbox dead-lettering was impossible.** `serviceaccount_app` had no grant on `outbox_dead_letters`, so a poison event retried forever. Migration `000004_hardening` adds the grants, pins `app_tenant_id()`'s `search_path`, revokes it from `PUBLIC`, and gives `prune_rls_violation_log()` a 7-day minimum TTL.
  - **OpenBao token expiry.** A 403 re-logs in once and retries.
  - **RP-17 client.** Any 2xx is success (was 204 only); 429/502/503/504 are retried; the response body is drained with a 64 KB cap.
  - **JWKS.** Expired overlap keys are no longer served. A per-tenant rate limit is added (`JWKS_RATE_LIMIT_PER_TENANT_RPS`/`_BURST`, default 5/10; new `iam_token_service_jwks_rate_limited_total`, alert `IAMTokenServiceJWKSRateLimited`), and responses send `Cache-Control: no-cache`.
  - **Shutdown.** The server and consumer flip `/readyz` to 503 and wait `SHUTDOWN_DRAIN_DELAY` (default 5s) before closing listeners. A consumer whose receive loop exits early now exits non-zero.
  - **Error mapping.** `57014` (query canceled) is `db_unavailable` only while the request is still live; 5xx responses are logged with `trace_id`, `tenant_id` and route; the dedup metric's `event_type` label is bounded.
  - **Smoke gate.** A container that hung until the 10s timeout (exit 124/137) passed as "exited non-zero"; it now fails. The scheduler is smoke-tested and shipped in release artifacts.
  - **docker-compose.** The server/consumer healthchecks used `wget` in a distroless image and could never pass; they are removed.

### Changed

- **Deployment hardening (TS-D21).**
  - Migrations run once per release in a pre-install/pre-upgrade hook Job (`MIGRATE_ONLY=true`); the Deployments and CronJobs start with `RUN_MIGRATIONS=false` and no migrator DSN.
  - One ServiceAccount per workload (`serviceAccount.perWorkload`, default true). IRSA is split into `deploy/iam/policy-server.json` (SNS + Glue, now including the `schema/<registry>/*` ARNs the codec actually reads) and `deploy/iam/policy-consumer.json` (SQS + DLQ send). The rotator, scheduler and migrate Job get no AWS role. The unused CloudWatch Logs and DLQ `ReceiveMessage` grants are removed. The CI schema-registry role trusts only its GitHub environment.
  - OpenBao login uses a projected ServiceAccount token with the `openbao` audience (`openbao.tokenAudience`, `OPENBAO_K8S_TOKEN_PATH`); the OpenBao role binds the four workload ServiceAccounts with that audience. Optional `openbao.caCertSecret` (`BAO_CACERT`).
  - NetworkPolicy: egress by CIDR for off-cluster Postgres/OpenBao/DNS/OTel (`networkPolicy.egress.*`, Postgres required), and the metrics port open only to `networkPolicy.monitoringNamespaceSelector` (required). Optional Istio `AuthorizationPolicy`, image by digest (`image.digest`), RuntimeDefault seccomp, per-workload grace periods, `GOMEMLIMIT` and batch limits, a PodMonitor for the CronJobs, and SLO rules as a PrometheusRule. Removed a duplicate `timeoutSeconds` in the startup probes.
  - Alerts: the per-run CronJob counters are read with `max_over_time` (`increase()` over a Job's short-lived series missed failures). New `IAMTokenServiceSchedulerNotRunning` and `IAMTokenServiceCronJobRunFailed`; runbooks for each.
  - `deploy/monitoring/prometheus-adapter-rule.yaml` is now a `rules.custom` merge snippet (applying the old ConfigMap replaced every other service's adapter rules).
  - CI/release: third-party actions pinned by commit SHA and Dependabot enabled (needs a `GO_PRIVATE_TOKEN` Dependabot secret for private modules). Releases push a candidate tag, then scan, sign and smoke-test that digest before the release tags are applied. The deploy gate deploys by digest with environment values from the `HELM_VALUES_B64` secret. CI re-scans the pushed digest before signing. `make vuln-check` scans `./...` with govulncheck pinned at v1.8.0.

### Changed

- **Shared libraries and observability wiring aligned with iam-org-membership (2026-10-05).**
  - platform-gincommon v1.4.0 → v1.6.0 (additive: four more registry `iam_*` entries; `OTEL_EXPORTER_OTLP_ENDPOINT=none` / `OTEL_TRACES_EXPORTER=none` disable trace export).
  - **Fixed:** the 30s request timeout ran outside the observability middleware, so a timed-out request was recorded as 200 while the client got 503. It is now `gincommon.Config.RequestTimeout` (server and consumer health router).
  - All four binaries use `gincommon.InitTracingWithConfig` with the metrics identity (was `InitTracingFromEnv`), `gincommon.MetricsHandler()` (was `promhttp.Handler()`), `gincommon.NewSpanTracer` for `db.query` spans (the hand-rolled `postgres.NewOTelTracer` is removed) and `gincommon.NewTracer` / `SpanTraceID` for job and consumer spans. `check-gincommon-observability.sh` now rejects `promhttp.Handler()` and `InitTracingFromEnv()`.
- **Offboarding dedup runs on platform-events' `pkg/inbox`, as in iam-org-membership (TS-D19).** New `port.Inbox` / `port.InboxPruner` and `postgres.InboxRepository` (`inbox.Store` per consumer) replace `ProcessedEventsStore` / `ProcessedEventsRepository`. The `processed_events` claim is now the first statement of the cascade's transaction, so two copies of one message handled at once run the cascade once (before, both could pass the check). The tenant GUC is still bound with `SET LOCAL` and OpenBao material is still deleted before the Postgres commit; a failure rolls the claim back, so the redelivery repeats the cascade. `platform_duplicate_messages_total` is now counted by the library only. `cmd/rotator` prunes through `inbox.Store.Prune`. Frozen consumer name `tenant_offboarding` and the table are unchanged.
- **Env vars audited against the shared libraries, aligned with iam-org-membership (TS-D20).** Every variable the Helm chart sets is now read by a binary or a shared library, and the library settings iam-org-membership passes are passed here.
  - **Removed:** `OTEL_SERVICE_NAME` (the trace name comes from `APP_NAME`), the aliases `SNS_TOPIC_SERVICEACCOUNT_ARN` / `SQS_OFFBOARDING_QUEUE_URL` / `SQS_OFFBOARDING_CONCURRENCY` (code, Helm, compose, `.env-example`; use `SNS_TOPIC_ARN` / `SQS_QUEUE_URL` / `SQS_CONCURRENCY`), `AWS_REGION` on the CronJobs, and the unused values `database.logicalName`, `events.topic`, `events.source`. Helm values renamed: `sqs.offboardingQueueUrl` → `sqs.queueUrl`, `sqs.offboardingConcurrency` → `sqs.concurrency`.
  - **Fixed:** `ROTATION_DEFAULT_OVERLAP_SECONDS` was set and documented but never read (300s was hardcoded). It now sets the TS-1 default and the scheduler's overlap; a value outside `[0, 900]` fails startup (TS-CONFIG-4).
  - **Fixed:** `PG_STATEMENT_TIMEOUT` was appended to the migration and reconciler DSNs by `postgres.ApplyStatementTimeout`, which produced a malformed URL for a DSN without a query string and would have bounded migrations by the API timeout. Removed: platform-pgcommon already applies it per transaction on both pools.
  - **Added to Helm:** `PG_MAX_CONNS`, `PG_MIN_CONNS`, `PG_SLOW_QUERY_THRESHOLD`, `PG_LOCK_TIMEOUT`, `PG_STATEMENT_TIMEOUT` (5s API/consumer, 60s CronJobs), `LOG_SAMPLING`, `OTEL_EXPORTER_OTLP_INSECURE` / `OTEL_TRACES_SAMPLER_RATIO` / `OTEL_PROPAGATOR_BAGGAGE` / `OTEL_TRACES_IGNORE_REMOTE_PARENT_SAMPLED`, an always-set `OTEL_EXPORTER_OTLP_ENDPOINT` (collector by default, `none` when empty), `SQS_HANDLER_TIMEOUT` (45s, below the visibility timeout) and `SQS_DRAIN_TIMEOUT` (15s), the CronJobs' `*_RUN_TIMEOUT` / `*_METRICS_SCRAPE_GRACE` / `PRUNE_BATCH_LIMIT` / `OUTBOX_PRUNE_OLDER_THAN`, and the server's `ROTATION_OVERLAP_GAUGE_INTERVAL` / `RLS_VIOLATION_EXPORTER_INTERVAL`. The consumer applies the same SQS defaults in code when unset.
- **SLOs tied to LLD §11.** `deploy/monitoring/slo-rules.yml` no longer calls its targets placeholders: SLO-1 (TS-1 p99 ≤ 500 ms) and SLO-2 (99.9%) cite the LLD, SLO-3 is labelled an operational proxy, and the missing TS-3 read-latency SLO (p99 ≤ 100 ms) is added as SLO-4 with fast/slow burn alerts.

- **Platform libraries' metrics contract (platform-gincommon / platform-events / platform-pgcommon, unreleased versions).** The libraries now emit only `platform_*` metrics with a mandatory `{domain, service, environment}` identity; their pre-standard names were removed with no compatibility period.
  - Every binary (`server`, `consumer`, `rotator`, `scheduler`) sets `gincommon.Config.Domain` (`"iam"`, `OBSERVABILITY_DOMAIN` overrides) and `Environment` (`APP_ENV`). New `metrics.InitLibraryMetrics` calls `events.InitMetrics` and `pgmetrics.InitWithIdentity` with gincommon's identity and registerer, logs their `RegistrationWarning`s and fails startup on an invalid identity. It runs before the first `pgcommon.NewPool`, which now registers each pool's `platform_db_pool_*` gauges itself — the hand-registered pool-stats collectors are gone. The BYPASSRLS pool gets its own `pool="reconciler"` label (`SystemPoolConfig`) instead of a suffixed `service` label.
  - `platform_dependency_request_seconds` and `platform_duplicate_messages_total` now use the registry shapes (`{dependency, operation, outcome}`, `{queue, event_type}`) and record into platform-events' own collectors. The previous `{dependency, operation}` / `{queue}` shapes made platform-events' SNS/SQS/codec series fail to register. Tier 1/2 `service` is now gincommon's `"iam-token-service"`, not `"token-service"`.
  - Alerts (`deploy/monitoring/app-alerts.yml`, Helm `prometheusrule.yaml`), SLO rules, the prometheus-adapter rule, the HPA (`platform_http_requests_per_second`), the release health gate and the docs query the `platform_*` successors. Rules on successors still `proposed` in the registry carry `metric_status: proposed`. The 5xx queries now select `status_class="5xx"` (the HTTP metrics have no `status` label), and the TS-1 latency SLI selects `route` instead of `path`.
  - Helm `appEnv` is `prod` (was `production`, which platform-gincommon rejects). The docs surfaces treat `prod` and `production` alike, so they stay gated in production. New Helm value `observabilityDomain` sets `OBSERVABILITY_DOMAIN`, which is also in `docker-compose.yml`, `.env-example` and the Makefile.

### Added

- **RLS violations are observable (TS-D18).** Migration `000003_rls_violation_log`: `rls_check_tenant()` samples failed checks (1%) into the RLS-exempt `rls_violation_log`; `cmd/server` counts new rows into the Tier 2 `iam_rls_violations_total{violation_type}` (id cursor, `RLS_VIOLATION_EXPORTER_INTERVAL`); `cmd/rotator` prunes it through `prune_rls_violation_log()` (`RLS_VIOLATION_LOG_TTL_DAYS`, 30). `IAMTokenServiceRLSCrossTenantAccess` / `IAMTokenServiceRLSMissingGUC` now query it — they referenced a metric nothing emitted. Rejected cross-tenant writes are not logged (their error rolls the log row back). Postgres tests cover logging, grants, prune and the absence of false positives on every repository path.
- **Queue-depth metrics.** `cmd/consumer` enables platform-events' `platform_queue_depth` / `platform_dlq_depth` sampler (`SQS_QUEUE_DEPTH_INTERVAL`, default 60s; Helm `sqs.queueDepthInterval`). New alerts `IAMTokenServiceOffboardingQueueStalled` and `IAMTokenServiceOffboardingDLQBacklog`. The IAM policy already granted `sqs:GetQueueAttributes` on the queue and its DLQ.
- **`docs/observability/`** (README, generated `metric-registry.md`, `runbooks.md` for every alert, `migration.md`), matching iam-org-membership. `make metrics-lint` (platform-gincommon `metricslint` on a real scrape, reference check, inventory drift) runs in `make ci` and `Validate / Quality`; `make metrics-inventory` regenerates the inventory. `test/unit/metricsstandard` pins the production registration order and identity labels.

- **TS-6 `GET /api/v1/internal/tenants/:id/service-accounts/platform-automation` (TS-D17).** Reads the tenant's automation principal, including `principal_sub`, from the tenant id alone. The Workflow Service's connector workers use it to name the acting principal on step callbacks; TS-5 answers only the reverse question.
  - Code: `PrincipalRepository.FindByType`, `PrincipalService.ReadPlatformAutomation` and `PrincipalHandler.ReadPlatformAutomation`.
  - Tests: unit, postgres and e2e. The e2e covers register, re-mint and read, and asserts `principal_id` is stable while `principal_sub` follows the re-mint.
  - Helm: new optional `networkPolicy.workflowNamespaceSelector` ingress rule.
  - Docs: LLD §5.3/§5.4/§18.5a/§22.

- **`cmd/scheduler` — automatic cadence-driven rotation (§16 TSQ-6 Resolved, TS-D14).** A fourth binary, alongside `server`/`consumer`/`rotator`, closing the gap the "Corrected an overclaim" entry below first identified:
  - `service_account_credentials` gained `rotation_cadence_days`/`next_rotation_at` (migration `000002_rotation_cadence`) plus the partial index `idx_sac_next_rotation`; `CredentialService.IssueOrRotate` now stamps both on every issue/rotate (default `domain.DefaultCadenceDays` = 90, overridable via `WithCadenceDays`/`ROTATION_DEFAULT_CADENCE_DAYS`) and clears them on demote-to-rotating — only the current `active` row is ever "due". TS-3 exposes both fields read-only.
  - New `port.ReconcilerRepository.ListDueForRotation` + Postgres impl — the same BYPASSRLS `serviceaccount_reconciler` enumeration pattern `cmd/rotator` uses (RLS-7), reused here for the due-list scan only; every actual rotation still goes through the ordinary RLS-scoped `CredentialService.IssueOrRotate`.
  - New outbound adapter `internal/adapter/outbound/realmprovisioner` (+ shared `internal/adapter/outbound/httpx` transport) — cmd/scheduler's RP-17 relay, the one outbound HTTP call this service makes to another IAM service, authenticating with the same reserved `x-user-id: iam-system` header every internal caller already presents (a new caller, not a new auth mechanism).
  - `cmd/scheduler/{main,scan,helpers}.go`: enumerate due principals, call `IssueOrRotate` under a fresh `rotation_id` per row, then call RP-17 (`RefreshKeys`) so Keycloak re-fetches the JWKS — no key material is sent to RP (EXT-6/rev 1.3). A `IssueOrRotate`-succeeds-but-RP-17-call-fails outcome is a documented, page-worthy gap (the same two-halves reality TS-INV-7 already accepts for revoke) — classified `failed` (new metric `iam_token_service_cadence_rotation_total{result}`) with tenant/principal/version logged (never key material) for manual completion, not silently retried.
  - `.go-arch-lint.yml`, Makefile, Dockerfile, `docker-compose.yml`, and the Helm chart (`cronjob-scheduler.yaml`, `networkpolicy.yaml`'s extra egress rule, `values.yaml`) all updated for the fourth binary.
  - Fixed a latent bug this landed on top of: `credential_repository.go`'s `scanCredential`/`Insert`/`Update` didn't actually read/write the two new columns despite `credentialColumns` already listing them — every credential read would have panicked on a column/destination-count mismatch the moment the migration applied. Caught before it ever ran against a live column set.

- **Enterprise Platform Observability Standard adoption** — metrics now follow the platform/domain/service-specific three-tier taxonomy:
  - Centrally-injected labels (`domain`, `service`, `environment`) via `metrics.Register(environment)` — instrumentation call sites can no longer omit or misspell them (requirement #8).
  - Three metrics proposed for the Platform/IAM Domain registries, dual-emitted alongside their legacy `iam_token_service_*` equivalent during the compatibility period (see `docs/observability-registry-proposals.md` for the full submission): `platform_dependency_request_seconds{domain,service,environment,dependency,operation}` (parallels `iam_token_service_openbao_call_duration_seconds`), `platform_duplicate_messages_total{domain,service,environment,queue}` (parallels `iam_token_service_processed_events_duplicates_total`), `iam_offboarding_cascade_total{service,environment,outcome}` (parallels `iam_token_service_offboarding_cascade_total`).
  - New CI gate `make metrics-taxonomy` (`.github/scripts/check-metrics-taxonomy.py`), wired into `make gates`, enforcing namespace-prefix classification and the counter-`_total`/histogram-`_seconds` naming rules.
  - No existing metric was renamed or removed — this is purely additive; alerts/dashboards remain on the legacy `iam_token_service_*` names until the proposed metrics are ratified.

### Documentation

- **TSQ-6 resolved — automatic cadence-driven rotation via a new `cmd/scheduler` component (IB-5, rev 1.2).** `docs/iam-lld-token-service.md` §16 TSQ-6 (previously Open) is now Resolved: `rotation_cadence_days`/`next_rotation_at` are stored on the `active` row in `service_account_credentials` (§4.2) and exposed read-only through TS-3 (§5.4); a new fourth composition root, `cmd/scheduler`, will scan the due-list and call TS-1 automatically (then relay to RP-17) using a dedicated service identity — the existing `x-user-id: iam-system` header added to the NetworkPolicy allow-list as a new caller, not a new authorization mechanism (§5.2/§10.4). `cmd/rotator` is reconfirmed as retirement/cleanup-only and will not initiate rotations. Updated §3, §4.2, §5.2, §5.4, §8.2, §12, §13.1, §16, §22 (TS-D10 corrected, new TS-D14). Implemented 2026-09-18 — see the "Added" entry below for the shipped shape.
- **Corrected an overclaim about scheduled-cadence rotation (IB-5).** §8.2, TSQ-2, and TS-D10 in `docs/iam-lld-token-service.md`, plus `README.md`'s cross-service-dependencies table, previously stated MVP supports "scheduled rotation (the `cmd/rotator`/cron path)" alongside operator on-demand rotation. Verified against the actual `cmd/rotator` source: it only runs the overlap-expiry sweep, orphaned-material reconciliation, and outbox/processed-events prune — it never calls TS-1 to initiate a rotation, and no rotation-due/cadence data model (e.g. a `next_rotation_at` column) exists anywhere in this service. Corrected the LLD and README to state plainly that only operator/O&M-tooling-initiated on-demand rotation exists today (a manually-orchestrated TS-1-then-RP-17 sequence, not an automated inter-service call), and added `docs/iam-lld-token-service.md` §16 TSQ-6 (Open) tracking who builds the cadence-driving scheduler and where its state lives. No code changed — on-demand rotation already fully covers break-glass/suspected-compromise; only automatic cadence-driven rotation is missing.
- Aligned `docs/iam-lld-token-service.md`, `README.md`, `ARCHITECTURE.md`, and a new `.claude/` directory (`CLAUDE.md`, `architecture.md`, `operations.md`, `request-flows.md`, `database-schema.md`, `api-events.md`) with current project state: the production-readiness hardening pass and the Enterprise Platform Observability Standard adoption above. LLD changes are additive prose/Decision-Register updates (new §22 entry `TS-D13`) — no frozen name (§25) or resolved open question (§16) was reopened, so no LLD revision bump was needed.

Initial build of the Token Service — custodian of the platform-automation service account's rotating credential material (LLD `docs/iam-lld-token-service.md`, v1.0 Approved).

- **HTTP API (TS-1..TS-4)** under `/api/v1/internal`: issue/rotate, revoke, and read credential metadata for the per-tenant platform-automation principal.
- **OpenBao adapter** (`internal/adapter/outbound/openbao/`) — Kubernetes-auth-backed KV v2 client at the frozen path `iam/serviceaccount/<tenant_id>/<keycloak_client_id>/v<version>` (§6.3, §25). No credential plaintext or hash is ever persisted to Postgres (TS-INV-2). TS-INV-1: this service has no Keycloak Admin API dependency at all.
- **Postgres schema** — RLS-scoped tables under `FORCE ROW LEVEL SECURITY`, `outbox_events` (`platform-events`' `outbox.ApplySchema`) and `processed_events`; `serviceaccount_app` (RLS-scoped) / `serviceaccount_reconciler` (BYPASSRLS, enumeration-only) / `serviceaccount_migrator` (BYPASSRLS, DDL) roles.
- **Transactional outbox + 5 produced events** on the single-producer `iam-serviceaccount-events` registry: `ServiceAccountRegistered`, `ServiceAccountCredentialIssued`, `ServiceAccountCredentialRotated`, `ServiceAccountCredentialRevoked`, `ServiceAccountRevoked`.
- **Offboarding cascade consumer** (`cmd/consumer`) — handles `TenantMembershipsPurged`, revoking credential material for an offboarded tenant.
- **Rotator** (`cmd/rotator`, CronJob) — overlap-expiry sweep (§8.3), orphan-material reconciler (§8.6), and outbox/processed-events prune, one binary invocation per fire.
- **Observability** — `iam_token_service_*` Prometheus metrics (credentials_issued_total, rotation_overlap_active, openbao_call_duration_seconds, offboarding_cascade_total, rotation_sweep_total, material_reconcile_total), OTel tracing spans (`credential.issue_rotate`, `credential.revoke`).
- **Deploy manifests** (`deploy/`) — Helm chart (server + consumer Deployments, rotator CronJob sharing one image), OpenBao policy + Kubernetes-auth role binding.
- **CI** (`.github/workflows/ci.yml`) — go-arch-lint, the three invariant gates (no-gocloak/TS-INV-1, no-secret-log/TS-INV-2, SET-LOCAL-only/RLS-6), format/vet/build, unit/contract/Postgres-RLS test suites.

### Fixed

- **`ServiceAccountRegistered` rejected every shared-realm registration.** `keycloak_client_id` was still `const: "platform-automation"` in `api/asyncapi.yaml` and the embedded schema after rev 1.1 widened TS-4 to `platform-automation-<tenant_id>`.
  - Effect: the enqueue-time `ValidatingCodec` failed TS-4's transaction with a 500, so every RP-1 trial mint and every RP-4 revert re-mint broke.
  - Fix: the field is now `pattern: ^platform-automation(-<uuid>)?$`, a BACKWARD-compatible loosening that matches `domain.ValidPlatformAutomationClientID`. A codec table test covers it.

**Event pipeline hardening (LLD rev 1.4), 2026-09-23:**

- **`cmd/consumer` could not decode Glue-encoded events.** Its SQS consumer had no `events.WithConsumerCodec`, and iam-org-membership publishes `TenantMembershipsPurged` Glue-encoded — every such message would have failed decode and ended in the DLQ with the offboarding cascade never run. Now wired with the new decode-only `eventbus.GlueDecoder` (no Glue client or registry).
- **Consumed payloads are validated before the cascade.** `cmd/consumer/inbound_schema.go` checks each payload against the embedded `tenant_memberships_purged.json` (`ValidatingCodec.Validate` + `ErrNoSchema`); a violation is counted by the new `iam_token_service_consumed_schema_violations_total` (critical `IAMTokenServiceConsumedSchemaViolation`) and never reaches `Handle`.
- **Permanent rejects go straight to the DLQ.** `cmd/consumer/dlq.go` sends a schema violation to `tenant-lifecycle-tokensvc-q-dlq` (`DLQReason=schema_violation`, DLQ URL from the queue's `RedrivePolicy`) and acks, instead of retrying `maxReceiveCount` times; falls back to normal redrive if the DLQ can't be resolved or the send fails. New `sqs:SendMessage` grant `OffboardingDLQPermanentRejects`.
- **Glue schema versions are resolved by definition, not "latest"; the 5-minute refresher is gone.** `GlueCodec` calls `glue:GetSchemaByDefinition` once at startup per produced schema with the embedded schema in `schema-gov register`'s exact compact form (Python-parity-tested), requiring `AVAILABLE`. `StartRefresher`, `GlueCodec.WithLogger` and `buildGlueCodec`'s logger parameter are removed. New floci integration test `test/integration/glue_codec_test.go`.

**EXT-6 (client-jwt/JWKS, rev 1.3) completeness fixes (TS-D15) — production-readiness review, 2026-09-19:**

- **Revoked keys never actually stopped authenticating at Keycloak (security regression).** `cmd/rotator`'s overlap-expiry sweep (`revokeExpiredRotating`) updated Postgres/OpenBao but never called RP-17 (`ClearServiceAccountKeysCache`) — under EXT-6's client-jwt/JWKS mechanism there is no Keycloak-side TTL, so every credential the sweep "revoked" kept validating at Keycloak indefinitely, silently, with no alert. `cmd/rotator` now wires the same `port.RealmProvisionerClient` `cmd/scheduler` already had and calls `RefreshKeys` after every successful revoke commits; a resulting RP-17 failure is classified `Failed` (page-worthy), not swallowed, since the row is already `revoked` and will never be re-enumerated by the sweep again.
- **LLD self-contradiction.** `docs/iam-lld-token-service.md` §8.2 still described the pre-EXT-6 "relay a plaintext secret to RP, which applies it via client-secret rotation" flow — directly contradicting TS-D4/TS-INV-7/TSQ-1 elsewhere in the same rev-1.3 document. Corrected §8.2 and §8.3 to the RSA-keypair/JWKS/`ClearServiceAccountKeysCache` flow, including the same page-worthy two-halves gap already documented for `cmd/scheduler`'s rotate path.
- **No network path for Keycloak to reach the new JWKS endpoint.** The unauthenticated-by-design JWKS route (`GET .../service-accounts/platform-automation/jwks.json`, EXT-6) was never added to the NetworkPolicy chart's ingress allow-list — with `networkPolicy.enabled: true` in a real cluster, Keycloak's own outbound fetch would be silently dropped and the client-jwt authenticator couldn't function. New required, no-safe-default `networkPolicy.keycloakNamespaceSelector` value (same fail-closed pattern as `ingressNamespaceSelector`) plus a new ingress rule on the `-server` NetworkPolicy; `cmd/rotator`'s NetworkPolicy also gained the RP-17 egress rule `cmd/scheduler` already had, and `cronjob-rotator.yaml` now sets `REALM_PROVISIONER_BASE_URL`.
- **JWKS route had no defense against being hit at an arbitrary rate.** It's deliberately unauthenticated by header (its whole reason for existing) and reads a private key out of OpenBao per live credential on every hit. `JWKSHandler.WithRateLimit` installs a process-wide `golang.org/x/time/rate` token bucket (`JWKS_RATE_LIMIT_RPS`/`JWKS_RATE_LIMIT_BURST`, default 20/40); the response now also carries `Cache-Control: public, max-age=60` (safe — the content is public, and Keycloak's own fetch is lazy/uncached at its end regardless, so this adds no real staleness).
- **`RealmProvisionerClient.RefreshKeys` never retried a transient RP-17 failure.** RP-17 is documented idempotent (RP LLD §2.5), and `cmd/scheduler`/`cmd/rotator` call it unattended every 5 minutes — a single blip (network hiccup, or RP-17's own documented 502 `keycloak_unavailable`) paged on-call for nothing. Now retries once (2 attempts total, 300ms backoff — see the next entry for why not 3); a permanent error (422, or anything else) still fails on the first attempt, since retrying it cannot help.
- **No alert existed for `iam_token_service_cadence_rotation_total{result="failed"}`.** The metric was emitted but nothing paged on it, unlike its siblings `rotation_sweep_total`/`material_reconcile_total`. New `IAMTokenServiceCadenceRotationFailures` alert (both `deploy/monitoring/app-alerts.yml` and `templates/prometheusrule.yaml`).

**Third pass, same review, 2026-09-20 — a follow-up audit of the fixes above found two of its own bugs:**

- **`CadenceRotationTotal` was constructed but never registered with Prometheus.** Defined when `cmd/scheduler` first landed, but never passed to `gincommon.MetricsRegisterer().MustRegister(...)` — `iam_token_service_cadence_rotation_total` never actually reached `/metrics`, so the `IAMTokenServiceCadenceRotationFailures` alert added above could never have fired despite existing. Fixed; `TestRegister_IsIdempotentAndRegistersAllCollectors` now re-`MustRegister`s every collector and asserts each one panics (a silent non-panic means "never actually registered" — exactly how this hid through two prior review passes).
- **The RP-17 retry budget above still didn't close against the CronJob deadline.** The original 3-attempt/5s-timeout policy cost ~16s worst case per row; both `ROTATOR_RUN_TIMEOUT` and `SCHEDULER_RUN_TIMEOUT` defaulted to 5 minutes — *longer* than `activeDeadlineSeconds` (240s default) — so a run using its own budget could never reach its graceful-exit path (summary log, metrics scrape, pool drain) before Kubernetes SIGKILLs the Pod. `RealmProvisionerClient`'s request timeout is now 3s (down from 5s — RP is in-mesh, not cross-region), making the retry's worst case ~6.3s/row; `ROTATOR_RUN_TIMEOUT`/`SCHEDULER_RUN_TIMEOUT` now default to 180s, safely under the k8s deadline.
- **The JWKS route's silent-partial-200 reached only a log line, never anything page-worthy.** A live credential whose OpenBao material is unreadable was dropped from the response (still 200) with a `Warn` log — a real per-credential Keycloak auth outage presented as success, with no way to distinguish it from a tenant that legitimately has zero keys. `JWKSService.PublicKeys` now also returns a `skipped` count; the HTTP handler (which, unlike `core/service`, may depend on `observability`) turns it into a new counter `iam_token_service_jwks_key_errors_total`, with a matching `IAMTokenServiceJWKSKeyErrors` alert. The pre-existing `Warn` call is also now nil-logger-guarded (a latent panic against this codebase's own established convention).
- **Miscellaneous hardening + doc gaps this same audit surfaced:** `X-Content-Type-Options: nosniff` added to the JWKS response (its one fully-public route); a stray untracked 44MB `rotator` build artifact removed from the repo root and `.gitignore` broadened to catch a repeat; `docker-compose.yml`'s `rotator` service and `.env-example` were missing `REALM_PROVISIONER_BASE_URL`/`JWKS_RATE_LIMIT_*`; `.claude/CLAUDE.md` still described three binaries and never mentioned EXT-6/JWKS at all; three remaining "rev 1.2" cross-references in `README.md`/`ARCHITECTURE.md` updated to rev 1.3; `docs/swagger/` regenerated (`make swag`) to pick up the `@Failure 429` annotation added alongside `WithRateLimit` above, which CI's `make swag-check` would otherwise have failed on.

Earlier production-readiness hardening pass (adversarial security/correctness/operational review):

- **NetworkPolicy** (`deploy/helm/templates/networkpolicy.yaml`) — ingress `namespaceSelector: {}` matched every namespace in the cluster (no restriction at all), contradicting the documented mesh-only ingress guarantee. `networkPolicy.ingressNamespaceSelector` is now a required value (render fails if unset when `networkPolicy.enabled: true`).
- **Migration rollback** (`internal/adapter/outbound/postgres/migrations/000001_schema.down.sql`) — `REVOKE ... FROM admin_readonly` unconditionally referenced a role that may legitimately not exist (it is infra-provisioned ahead of the migration in prod), so `migrate down` hard-failed in any dev/CI/fresh environment. Guarded the same way `up.sql`'s grant already is; verified against a real Postgres instance in both branches.
- **TS-2 revoke / overlap-expiry sweep** (`internal/core/service/credential_service.go`, `cmd/rotator/sweep.go`) — a credential revoke racing another concurrent revoke of the exact same row (another TS-2 call, or the sweep) surfaced a misleading `409 optimistic_lock_conflict` even though the desired end state was already reached by the other actor. Both paths now re-check after an optimistic-lock conflict and return the idempotent success/skip outcome instead.
- **TS-1 stale `rotation_id` replay** (`internal/core/service/credential_service.go`) — replaying a `rotation_id` from an already-superseded, now-`revoked` rotation let the OpenBao read fail and surface as a misleading `502 secret_store_unavailable`. Added `credential_replay_revoked` (409, additive to the frozen §17 taxonomy) to classify this explicitly.
- **OpenBao client** (`internal/adapter/outbound/openbao/client.go`) — `kvClient` mutated the single shared `*openbaoapi.Client`'s token field directly; under concurrent requests one call's token could be overwritten by another's before its own KV request executed. Now clones the client per call (same connection pool, independent token field, no added cost).
- **`TS-INV-2` gate** (`.github/scripts/check-no-secret-log.py`, new) — the previous single-line `grep -E` gate could miss a secret-named field on its own line inside a multi-line log-call argument, or a capitalized Go identifier (`Secret`, not just `secret`). Replaced with a paren-matching, case-insensitive checker.
- **SAST** — added `gosec` (`make sast`, wired as a `go tool` directive alongside `golangci-lint`) as a new, source-code-level static-analysis gate in `validate-quality.yml`, distinct from `govulncheck` (dependency CVEs) and the release pipeline's Trivy scan (container/OS CVEs).
- **Observability** — wired `platform-pgcommon`'s `PoolStatsCollector` for every connection pool in `cmd/server`/`cmd/consumer`/`cmd/rotator` (previously imported but never registered) and added three new alerts: outbox backlog growth, Postgres pool exhaustion, and offboarding-queue message age (the last documented as dormant until a CloudWatch exporter is deployed — SQS queue depth/age is not available in-process).
- **`cmd/rotator` CronJob** — `activeDeadlineSeconds` (300s) exactly equalled the 5-minute schedule interval, leaving zero slack before a `concurrencyPolicy: Forbid` job could silently skip a tick; reduced to 240s.
- Documented the (already-correct) offboarding-cascade partial-failure/DLQ behavior and the two race-condition fixes above in `ARCHITECTURE.md`'s failure-domains table.
