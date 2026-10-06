# Runbooks: iam-token-service alerts

Runbooks for every alert in [`deploy/monitoring/app-alerts.yml`](../../deploy/monitoring/app-alerts.yml), [`deploy/monitoring/slo-rules.yml`](../../deploy/monitoring/slo-rules.yml) and the Helm [`PrometheusRule`](../../deploy/helm/templates/prometheusrule.yaml). The heading is the alert name. Every alert carries `domain="iam"`, `service="iam-token-service"` and `environment`: from the series for most, and as static rule labels for the five built on `absent(up)` or kube-state-metrics (`ServerDown`, `ConsumerDown`, `RotatorNotRunning`, `SchedulerNotRunning`, `CronJobRunFailed`). The Helm rules render those from `appEnv`; the static `app-alerts.yml` hard-codes `environment: prod`, so edit it per environment. Platform-level alerts on the libraries' own metrics (HTTP error budget, message failure ratio) are documented in platform-gincommon's `docs/observability/runbooks.md`. The Glue schema-registry alerts (`deploy/monitoring/schema-registry-alerts.yml`) are platform-wide and documented with `schema-gov`.

Common starting points:

- Rollouts: `count by (version) (platform_build_info{service="iam-token-service",environment="$environment"})`
- Logs (Loki): `{service="iam-token-service"} | json`, then follow `trace_id` into Tempo. Logs carry `tenant_id` / `principal_id` / `version`, never key material (TS-INV-2).
- Readiness: `GET /readyz` on the server checks Postgres and OpenBao concurrently.
- Binaries: `server` (TS-1..TS-6, JWKS), `consumer` (offboarding cascade), `rotator` and `scheduler` (CronJobs; Prometheus `job` = `iam-token-service-<binary>`).
- Labels: the alerts that select on `service="iam-token-service"` (`IAMTokenServiceOutboxStuck`, `IAMTokenServiceOutboxBacklogGrowing`, `IAMTokenServiceSQSReceiveErrors`, `IAMTokenServicePostgresPoolExhaustion`) depend on the series keeping their own `service` label, so the ServiceMonitor and PodMonitor set `honorLabels: true`. Without it the Prometheus Operator's `service=<Kubernetes Service name>` target label wins, the series' label is renamed `exported_service`, and those alerts can never fire. If one of them looks silent, check a raw series for `exported_service`.
- Owed RP-17 refreshes: `SELECT * FROM keys_refresh_pending;` (one row per tenant whose Keycloak key cache still needs a refresh, with `requested_at`). The rotator and the scheduler write a row with every revoke/rotation, clear it when RP-17 succeeds, and retry owed rows at the start of each run: committed rows (`intent_until` NULL) at once, the scheduler's pre-rotation intent rows only after their `intent_until` passes. A row that keeps getting older means RP-17 is still failing for that tenant. The server exports the backlog as `iam_token_service_keys_refresh_pending` (rows) and `iam_token_service_keys_refresh_oldest_age_seconds` (every `KEYS_REFRESH_EXPORTER_INTERVAL`, Helm `exporters.keysRefreshInterval`, default 30s); `IAMTokenServiceKeysRefreshBacklog` alerts on the age.

## Availability

### IAMTokenServiceServerDown

**Meaning.** No server pod has been scraped as healthy for 2 minutes. TS-1..TS-6 are unavailable, and so is the JWKS route: Keycloak cannot validate any `platform-automation` client assertion while it is down (EXT-6).

**Triage.**
1. `kubectl get pods -l app.kubernetes.io/component=server,app.kubernetes.io/name=iam-token-service` and `kubectl describe pod` for crash loops and probe failures.
2. Startup panics are deliberate for bad configuration, and the panic message names it: an invalid `APP_ENV` (e.g. `production`), an invalid identity (`init library metrics`), an unregistered or non-`AVAILABLE` Glue schema definition (`GlueCodec`), a failed OpenBao Kubernetes-auth login.
3. Migrations: with `migrations.enabled` (the default) they run in the `<fullname>-migrate` pre-install/pre-upgrade hook Job, not at server startup — a failed hook fails the release before any pod rolls. Check that Job's logs for a migration lock wait (`lock_timeout=30s`) or a dirty migration. With `migrations.enabled: false` every pod migrates at startup and the same errors appear in the server logs.

**Mitigate.** Roll back the last deploy, or fix the named configuration.

### IAMTokenServiceServerReplicasMissing

**Meaning.** Fewer server replicas are being scraped than the configured floor (`server.replicaCount`, or `autoscaling.minReplicas` when the HPA is on; 2 in the static rules). One more disruption takes the API and the JWKS route down. Counted from `platform_build_info`, which carries the `environment` label (`up` does not).

**Triage.** As for `IAMTokenServiceServerDown`, per pod. Check node pressure and pending pods.

### IAMTokenServiceConsumerDown

**Meaning.** No consumer pod has been scraped as healthy for 2 minutes. `TenantMembershipsPurged` messages accumulate on `tenant-lifecycle-tokensvc-q` (SQS keeps them), so offboarded tenants' credentials are not revoked until it recovers.

**Triage.** Pod status and logs. Startup needs the queue's `RedrivePolicy` (the DLQ URL for permanent rejects), `sqs:ReceiveMessage` / `GetQueueAttributes`, Postgres and OpenBao.

**Mitigate.** Restore the consumer. No replay is needed: SQS redelivers.

### IAMTokenServiceRotatorNotRunning

**Meaning.** The rotator CronJob (overlap-expiry sweep §8.3, orphan-material reconciler §8.6, prune) has had no successful run for 15 minutes (schedule `*/5 * * * *`). Rotated-out keys past their overlap window stay valid at Keycloak until the sweep runs.

**Triage.** `kubectl get jobs -l app.kubernetes.io/component=rotator` and the last job's logs. The job exits non-zero on purpose when any revoke or RP-17 call failed, the reconciler failed (including an OpenBao list failure), `missing_material` was found, or a prune step failed — check `IAMTokenServiceRotationSweepFailures` and `IAMTokenServiceOrphanMaterialMissing` first. A run killed by `activeDeadlineSeconds` means `ROTATOR_RUN_TIMEOUT` is no longer below it. Tenants the run had no time left for are deferred, not failed, and picked up by the next run: the log line is `overlap sweep: run budget nearly spent or run signalled — deferring the remaining tenants to the next run` (fields `deferred_rows`, `deferred_tenants`), and the end-of-run `overlap sweep complete` line carries a `deferred` count. A sustained backlog means `ROTATOR_BATCH_LIMIT` or the schedule needs tuning.

### IAMTokenServiceSchedulerNotRunning

**Meaning.** The cadence-rotation CronJob (§16 TSQ-6) has had no successful run for 15 minutes. Credentials past `next_rotation_at` are not being rotated.

**Triage.** `kubectl get jobs -l app.kubernetes.io/component=scheduler` and the last job's logs. The job exits non-zero when any rotation or RP-17 call failed — check `IAMTokenServiceCadenceRotationFailures` first. Due rows beyond `SCHEDULER_BATCH_LIMIT`, or left when the run neared `SCHEDULER_RUN_TIMEOUT`, are deferred to the next run, not failed (`cadence scan: run budget nearly spent or run signalled — deferring the remaining tenants to the next run`, with `deferred_rows` / `deferred_tenants`).

**Mitigate.** Fix the failing dependency. A missed run is safe: the next scan picks up every row still past due.

### IAMTokenServiceCronJobRunFailed

**Meaning.** The latest scheduled run of the rotator or the scheduler (`$labels.cronjob`) failed: kube-state-metrics shows it was scheduled after the last success (or the CronJob has never succeeded), and no run is active now. It fires 2 minutes after the first failed run finishes, well before the `*NotRunning` alerts' 15 minutes, and names the failing CronJob. It resolves when the next run succeeds, or goes quiet while a retry run is active.

**Triage.** `kubectl get jobs -l app.kubernetes.io/component=<rotator|scheduler>`; the failed Job's pod logs give the tenant, principal and version for each failure. A pod that never started (image pull, admission, quota) shows in `kubectl describe job`.

**Mitigate.** As for `IAMTokenServiceRotatorNotRunning` / `IAMTokenServiceSchedulerNotRunning`.

## HTTP errors and latency

### IAMTokenServiceHighErrorRate

**Meaning.** More than 5% of server requests returned 5xx over 5 minutes.

**Triage.**
1. Which routes and classes: `sum by (route, error_class) (rate(platform_http_requests_total{service="iam-token-service",status_class="5xx"}[5m]))`.
2. The error body's `code`:
   - `db_unavailable` (503): Postgres or PgBouncer, classified by SQLSTATE class `08`/`53`/`57`/`58` or a closed pool (TS-D16). Check `platform_db_pool_connections`, `platform_db_pool_empty_acquires_total`, and RDS/PgBouncer health.
   - `secret_store_unavailable` (502): OpenBao, on TS-1/TS-2. Check `IAMTokenServiceOpenBaoErrors`, `iam_token_service_openbao_call_duration_seconds`, OpenBao health and the Kubernetes-auth login.
   - `jwks_keys_unavailable` (503): the JWKS route could not read the active key, or any key (see `IAMTokenServiceJWKSKeyErrors`).
   - `request timeout` (503): the 30s request deadline; counted on `platform_http_request_timeouts_total`.
   - anything else: an unhandled error, logged with `trace_id`.
3. `/readyz` on each pod.

**Mitigate.** Roll back a bad version, or restore the failing dependency.

### IAMTokenServiceHighLatency

**Meaning.** p99 across all server routes is above 1s for 10 minutes.

**Triage.** `histogram_quantile(0.99, sum by (le, route) (rate(platform_http_request_duration_seconds_bucket{service="iam-token-service"}[5m])))` to find the route. TS-1 writes to OpenBao before committing to Postgres (material-first), so check `iam_token_service_openbao_call_duration_seconds{op}` first, then `platform_db_query_duration_seconds` and `platform_db_pool_acquire_duration_seconds`. A slow JWKS route means slow OpenBao reads (one per live credential).

### IAMTokenServiceOpenBaoCallLatencyHigh

**Meaning.** OpenBao KV v2 `write` or `delete` p99 is above 5s, approaching the 8s `HTTPTimeout`. TS-1, TS-2 and the offboarding cascade all block on these calls.

**Triage.** OpenBao server health and seal status; the Kubernetes-auth login path (§10.5) — a token re-login on every call shows as uniform latency; network policy egress to OpenBao. `platform_dependency_request_seconds{dependency="openbao"}` carries the same calls by `outcome`; once they start failing, `IAMTokenServiceOpenBaoErrors` fires. TS-1's write and TS-2's delete run inside the transaction under the principal row lock, bounded at 5s, so slow OpenBao also holds Postgres connections and makes concurrent calls for that principal wait (`409 rotation_in_flight` after `PG_LOCK_TIMEOUT`).

**Mitigate.** Restore OpenBao. Callers retry TS-1 safely under the same `rotation_id`.

### IAMTokenServiceOpenBaoErrors

**Meaning.** More than 5% of OpenBao KV v2 calls failed over 10 minutes, across every binary (`platform_dependency_request_seconds_count{dependency="openbao",outcome="error"}` over all outcomes, selected by `service`; `metric_status: proposed`). TS-1/TS-2 return `502 secret_store_unavailable`, the JWKS route may return `503 jwks_keys_unavailable`, the offboarding cascade fails and is redelivered, and the rotator's sweep and reconciler fail.

**Triage.**
1. Which binary and operation: `sum by (job, operation) (rate(platform_dependency_request_seconds_count{service="iam-token-service",dependency="openbao",outcome="error"}[10m]))`.
2. All operations from every binary: OpenBao itself (sealed, down, TLS/CA change) or the network path (NetworkPolicy egress `openbaoCIDRs`/`openbaoPort`, the mesh).
3. Logins failing (`403`, `permission denied`, `service account name not authorized`): the OpenBao Kubernetes role binding (ServiceAccount names, namespace, `audience` = `openbao.tokenAudience`).
4. One operation only (e.g. `delete`): the OpenBao policy for that path.

**Mitigate.** Restore OpenBao or fix the role/policy. TS-1 retries are safe under the same `rotation_id`; the cascade redelivers on its own.

## Credential lifecycle

### IAMTokenServiceJWKSKeyErrors

**Meaning.** Page. The JWKS route could not serve a credential this service believes is live (`active` or `rotating`) because its OpenBao material was unreadable or unparseable. What the caller got depends on which key:
- the **active** key, or **every** key, unreadable: **503 `jwks_keys_unavailable`** instead of a key set missing the key. Keycloak keeps its previously cached keys, so a principal whose key it already holds keeps working, but a new key (after a rotation and RP-17) cannot be picked up and **that principal's new client assertions fail** until this is fixed.
- only an **overlap** (`rotating`) key unreadable, the active key fine: **200** with the readable keys. Assertions signed with the outgoing key may fail before its overlap window ends; the active key keeps working.

**Triage.** Is it one tenant or all? Many tenants at once means OpenBao itself (seal status, the Kubernetes-auth login, network policy egress; `IAMTokenServiceOpenBaoCallLatencyHigh`), and recovers by itself when OpenBao does. Server logs: `jwks: skipping unreadable credential` gives `tenant_id`, `principal_id`, `credential_id` and `version`. Read the OpenBao entry at that credential's path (`iam/serviceaccount/<tenant_id>/<keycloak_client_id>/v<version>`): missing (see `IAMTokenServiceOrphanMaterialMissing`), unreadable (policy), or not a valid PEM RSA key.

**Mitigate.** Rotate the credential (TS-1 with a new `rotation_id`), then call RP-17 so Keycloak drops its cached keys. Never copy key material by hand.

### IAMTokenServiceJWKSRateLimited

**Meaning.** The JWKS route answered 429 for **known** tenants in the last 10 minutes (`iam_token_service_jwks_rate_limited_by_bucket_total{bucket=~"tenant|global"}`), from the per-tenant limiter (`JWKS_RATE_LIMIT_PER_TENANT_RPS`/`_BURST`, default 5/10) or the process-wide one. Keycloak fetches a tenant's JWKS right after RP-17; if that fetch is rate-limited, the tenant's client assertions fail until Keycloak retries. 429s from the shared unknown-tenant bucket (`bucket="unknown"`: tenant ids with no live credential, e.g. a random-UUID scan) are excluded on purpose — that bucket exists to absorb such floods without touching real tenants' budgets; watch it on a dashboard, not a pager. A tenant counts as known once the server's DB refresh (`JWKS_KNOWN_TENANTS_REFRESH`, Helm `jwks.knownTenantsRefresh`, default 15s) or a served request has seen it; a brand-new tenant can sit in the unknown bucket for up to that interval.

**Triage.** Server access logs for the JWKS path: is it one tenant (a misbehaving caller, or a scan of the public route) or many (the global limit is too low for the fleet)? The route is unauthenticated by design, so outside traffic reaching it means the ingress or `AuthorizationPolicy` is wider than intended.

**Mitigate.** Block the abusive source at the ingress. If the traffic is legitimate Keycloak fetches, raise `jwks.perTenantRPS`/`perTenantBurst` in the Helm values.

### IAMTokenServiceOrphanMaterialMissing

**Meaning.** Page. The latest rotator run's §8.6 reconciler found a live (`active` or `rotating`) credential row with no OpenBao material (`result="missing_material"`). The row is re-checked before it is reported, so a credential revoked mid-run is not counted. That principal cannot authenticate.

**Triage.** Rotator logs for the tenant, principal and version. Check OpenBao audit logs for a delete at that path. A material-first write that failed after the Postgres commit is impossible by design, so a missing entry usually means an out-of-band delete or an OpenBao restore.

**Mitigate.** Rotate the credential (TS-1), then RP-17.

### IAMTokenServiceCadenceRotationFailures

**Meaning.** Page. The latest `cmd/scheduler` run reported `result="failed"` (the alert reads the per-run counter with `max_over_time` over 1h, so it clears an hour after the last failing run). Either `IssueOrRotate` failed (the next scan retries it), or it committed but the RP-17 `ClearServiceAccountKeysCache` call failed afterwards. `next_rotation_at` has already advanced in the second case, but the owed refresh is recorded in `keys_refresh_pending` and retried at the start of every rotator and scheduler run, so it **self-heals on the next run once the Realm Provisioner is healthy**. Until then Keycloak keeps its cached keys and does not see the new one.

**Triage.** Scheduler job logs give `tenant_id`, `principal_id` and `version`. Check whether the new version exists (TS-3). `SELECT * FROM keys_refresh_pending;` lists the tenants still owed a refresh. Realm Provisioner health (`platform_dependency_request_seconds{dependency="realm_provisioner"}`): RP-17 is called once per tenant per run and retried once on 429/502/503/504.

**Mitigate.** Restore the Realm Provisioner; the next runs clear `keys_refresh_pending`. Call RP-17 by hand only for a row that stays after the Realm Provisioner is healthy again (then find out why the retry is failing).

### IAMTokenServiceRotationSweepFailures

**Meaning.** A rotator run in the last hour reported sweep errors (`max_over_time` over the per-run counter). Rotated-out credentials past their overlap window may still be valid at Keycloak. RP-17 is called once per tenant after its revokes commit; if it fails, the run counts as failed. The row is already `revoked` and is never re-enumerated, but the owed refresh is recorded in `keys_refresh_pending` in the same transaction as the revoke and retried at the start of every rotator and scheduler run, so that half **self-heals on the next run once the Realm Provisioner is healthy**.

**Triage.** Rotator logs per credential. `optimistic_lock_conflict` races are classified `Skipped`, not failures, so every error here is real. `SELECT * FROM keys_refresh_pending;` lists tenants whose key-cache refresh is still owed (`IAMTokenServiceKeysRefreshBacklog` if it is not draining).

**Mitigate.** Fix the cause (OpenBao, Postgres, Realm Provisioner). Call RP-17 by hand only for a `keys_refresh_pending` row that stays after the Realm Provisioner is healthy again.

### IAMTokenServiceKeysRefreshBacklog

**Meaning.** The oldest `keys_refresh_pending` row has been owed an RP-17 key-cache refresh for more than 15 minutes, for 5 minutes (`max` of `iam_token_service_keys_refresh_oldest_age_seconds` across server replicas, which all export the same DB state). Every rotator and scheduler run retries owed rows first, so this row has survived about three runs: RP-17 is still failing. Until it succeeds, Keycloak may still accept a revoked key, or not yet know a new one, for those tenants (`iam_token_service_keys_refresh_pending` counts them).

**Triage.**
1. `SELECT tenant_id, requested_at FROM keys_refresh_pending ORDER BY requested_at;` — one tenant or many?
2. Many: the Realm Provisioner (`platform_dependency_request_seconds_count{dependency="realm_provisioner",outcome="error"}`), and whether the rotator/scheduler are running at all (`IAMTokenServiceRotatorNotRunning`, `IAMTokenServiceSchedulerNotRunning`, `IAMTokenServiceCronJobRunFailed`) — nothing retries the rows if neither runs. The CronJob pods reach RP-17 through NetworkPolicy egress `realmProvisionerPort`; under a STRICT-mTLS Realm Provisioner they also need `cronjobs.istioInject: true`.
3. One tenant: the rotator/scheduler logs for that `tenant_id` give the RP-17 error (e.g. the realm no longer exists at Keycloak).
4. A stale gauge: the exporter keeps the last value on a query error and logs it; check server logs before trusting the age.

**Mitigate.** Restore the Realm Provisioner; the next runs clear the rows. Call RP-17 by hand only for a row that stays after it is healthy, then find out why the retry fails.

### IAMTokenServiceRotationOverlapStuck

**Meaning.** `iam_token_service_rotation_overlap_active` has been above 0 for an hour, so `rotating` credentials are not draining. It should trend to 0 between rotations.

**Triage.** Is the rotator running (`IAMTokenServiceRotatorNotRunning`) and succeeding (`IAMTokenServiceRotationSweepFailures`)? Was a long overlap window configured? The gauge is refreshed from the BYPASSRLS `reconciler` pool by the server's exporter; a query failure is logged as `rotation overlap exporter: query failed` and leaves the last value.

## Events and consumers

### IAMTokenServiceConsumedSchemaViolation

**Meaning.** Page. Consumed `TenantMembershipsPurged` payloads fail the embedded schema (`tenant_memberships_purged.json`). They are sent straight to `tenant-lifecycle-tokensvc-q-dlq` (`DLQReason=schema_violation`) without running the offboarding cascade, so those tenants' service accounts are **not** being cleaned up.

**Triage.** Inspect a DLQ message and compare it with `api/asyncapi.yaml` and the embedded schema. Either iam-org-membership broke its contract, or this repo's copy of the consumed schema is stale.

**Mitigate.** Fix the wrong side, then redrive the DLQ.

### IAMTokenServiceOffboardingInvalidEnvelopeRejected

**Meaning.** Page (GDPR erasure). A `TenantMembershipsPurged` arrived with a missing or non-UUID envelope `id` (`iam_token_service_consumer_dlq_rejects_total{reason="invalid_envelope_id"}`). It can never be deduplicated, and acking it would silently skip the tenant's credential erasure, so the consumer sent it straight to `tenant-lifecycle-tokensvc-q-dlq` (`DLQReason=invalid_envelope_id`) and acked it. **That tenant's credentials have not been revoked** and will not be until the message is redriven. (`schema_violation` rejects are counted on the same metric but page through `IAMTokenServiceConsumedSchemaViolation` instead.)

**Triage.** Consumer logs at Error: `permanent reject sent to DLQ — tenant credential erasure is pending until it is redriven` (`event_id`, `event_type`, `trace_id`). Read the DLQ message: the payload's `tenant_id` names the tenant. If the DLQ send itself failed (`DLQ send failed — falling back to SQS redrive`), this counter does not move; the message reaches the DLQ via `maxReceiveCount` and shows only on `IAMTokenServiceOffboardingDLQBacklog`.

**Mitigate.** The producer (iam-org-membership) must fix its envelope ids. Give the DLQ message a valid UUID `id` and redrive it, so the cascade runs for that tenant. Never discard it.

### IAMTokenServiceOffboardingCascadeFailures

**Meaning.** The offboarding cascade (§8.4) returned errors in the last 15 minutes, and credential revocation for those tenants is delayed. A transient failure (OpenBao, Postgres) is redelivered by SQS and retried. A permanent reject also counts here but is **not** redelivered: a schema violation or an invalid envelope id is sent straight to the DLQ and acked (`IAMTokenServiceConsumedSchemaViolation`, `IAMTokenServiceOffboardingInvalidEnvelopeRejected`).

**Triage.** Consumer logs (`trace_id`). OpenBao deletes run inside the cascade transaction, before it commits (material-first), so OpenBao failures are the usual cause; then Postgres. A persistent failure ends in the DLQ after the queue's `RedrivePolicy` `maxReceiveCount`. SQS moves it there itself, so no in-service counter records it: the signal is `platform_dlq_depth` (`IAMTokenServiceOffboardingDLQBacklog`).

**Backoff.** Redelivery backs off exponentially (`SQS_RETRY_BACKOFF` 60s → 1m, 2m, 4m, 8m, capped at `SQS_MAX_RETRY_BACKOFF` 15m), so the 5 receives span roughly 15 minutes before a message reaches the DLQ. Retries are therefore sparse during an outage; that is expected, not a stuck consumer.

**Mitigate.** Restore the dependency; redrive anything that reached the DLQ.

### IAMTokenServiceSQSReceiveErrors

**Meaning.** `ReceiveMessage` on the offboarding queue is failing (`platform_dependency_request_seconds_count{dependency="sqs",operation="receive_message",outcome="error"}`; `metric_status: proposed`).

**Triage.** IAM permissions (`sqs:ReceiveMessage`, `GetQueueAttributes`, `GetQueueUrl`), queue existence, VPC endpoint or network policy egress.

### IAMTokenServiceOffboardingQueueBacklogGrowing

**Meaning.** The oldest message on `tenant-lifecycle-tokensvc-q` is over 15 minutes old. **Dormant** until a CloudWatch exporter publishes `aws_sqs_approximate_age_of_oldest_message_maximum` for this queue (SQS exposes message age only through CloudWatch). `IAMTokenServiceOffboardingQueueStalled` covers the same failure from platform-events' own depth sampler.

**Triage.** Consumer health (`IAMTokenServiceConsumerDown`), `platform_messages_in_flight` against `SQS_CONCURRENCY`, cascade failures.

### IAMTokenServiceOffboardingQueueStalled

**Meaning.** `platform_queue_depth` for the offboarding queue has stayed above 0 for 15 minutes. Offboarding traffic is sparse and each message is handled in seconds, so a backlog that never empties means the consumer is not receiving or is failing every message. Offboarded tenants' credentials are not being revoked. Sampled by platform-events every `SQS_QUEUE_DEPTH_INTERVAL` (`metric_status: proposed`).

**Triage.** `IAMTokenServiceConsumerDown`, `IAMTokenServiceSQSReceiveErrors`, `IAMTokenServiceOffboardingCascadeFailures`; `platform_messages_received_total` and `platform_messages_failed_total{reason}` for the queue; `platform_messages_in_flight` stuck at the concurrency limit means a hung handler.

### IAMTokenServiceOffboardingDLQBacklog

**Meaning.** Messages have been sitting in the offboarding queue's DLQ for 30 minutes (`platform_dlq_depth`, labelled with the source queue; `metric_status: proposed`). Those tenants' offboarding cascade has not run. The inflow alerts (`IAMTokenServiceConsumedSchemaViolation`, cascade failures ending in `max_receive_count`) say why they arrived; this one says they are still there.

**Mitigate.** Inspect the messages' `DLQReason` attribute, fix the cause, then redrive:
- `schema_violation`: the payload failed the consumed-event schema (see `IAMTokenServiceConsumedSchemaViolation`).
- `invalid_envelope_id`: a `TenantMembershipsPurged` whose envelope `id` is missing or not a UUID. It can never be deduplicated, and acking it would silently skip a GDPR erasure, so the consumer dead-letters it instead. The producer (iam-org-membership) must fix the envelope; redrive a corrected copy.
- no `DLQReason`: moved by SQS after `maxReceiveCount` deliveries (a cascade that kept failing; see `IAMTokenServiceOffboardingCascadeFailures`). Never discard without the owning team's sign-off: a discarded `TenantMembershipsPurged` leaves that tenant's credentials live.

## Outbox and database

### IAMTokenServiceOutboxStuck

**Meaning.** Produced service-account events (`ServiceAccountRegistered`, `...CredentialIssued`, `...CredentialRotated`, `...CredentialRevoked`, `ServiceAccountRevoked`) exhausted `MaxAttempts` and were moved to `outbox_dead_letters`. Consumers on `iam-serviceaccount-events` never received them.

**Triage.**
1. `SELECT id, event_type, tenant_id, attempts, last_error, failed_at FROM outbox_dead_letters ORDER BY failed_at DESC LIMIT 20;`
2. `last_error` usually names the cause: SNS permission, `SNS_TOPIC_ARN`, or a Glue schema version that is not `AVAILABLE`.

**Mitigate.** Fix the cause, then replay with the library's `outbox.Runner.ReprocessDeadLetters`.

### IAMTokenServiceOutboxBacklogGrowing

**Meaning.** More than 100 outbox events have been due for 15 minutes (`platform_outbox_pending_events`; `metric_status: proposed`). Transient SNS failures never dead-letter, so they show up here, before `IAMTokenServiceOutboxStuck`.

**Triage.** Outbox runner logs for SNS errors; `platform_outbox_oldest_pending_age`; `platform_outbox_errors_total{operation}`.

### IAMTokenServicePostgresPoolExhaustion

**Meaning.** Acquires on a pool had to wait because no connection was idle (`platform_db_pool_empty_acquires_total`). `pool="default"` is the RLS-scoped app pool; `pool="reconciler"` the BYPASSRLS enumeration pool (gauge exporter, rotator, scheduler).

**Triage.** `platform_db_pool_connections{state="acquired"}` against `platform_db_pool_max_connections`; `platform_db_pool_acquire_duration_seconds`; slow queries (`platform_db_slow_queries_total`) and long transactions. A slow OpenBao call **does** hold a connection: TS-1's write and TS-2's delete run inside the transaction under the principal row lock (TS-D21/TS-D22), bounded at 5s each, and the offboarding cascade's deletes run inside its inbox transaction. Check `IAMTokenServiceOpenBaoCallLatencyHigh` first. Then check whether HPA scale-out × `PG_MAX_CONNS` outgrew PgBouncer before raising `PG_MAX_CONNS`.

## Security

### IAMTokenServiceRLSCrossTenantAccess

**Meaning.** Page. A query running under one tenant's `app.tenant_id` touched a row belonging to another tenant, and RLS hid it (`iam_rls_violations_total{violation_type="cross_tenant_access"}`). The log is 1%-sampled, so one sample means more occurred. No data leaked — RLS held — but a code path is reading by an identifier that belongs to another tenant: either an attack (a caller presenting another tenant's ids) or a bug.

**Triage.**
1. From the reconciler or `admin_readonly` role: `SELECT occurred_at, table_name, row_tenant_id, app_tenant_id, session_role, query_text FROM rls_violation_log WHERE violation_type = 'cross_tenant_access' ORDER BY id DESC LIMIT 20;`
2. `query_text` names the repository query; `app_tenant_id` is the caller's tenant (from the path), `row_tenant_id` the owner of the row it reached. Correlate the time with server logs and traces for that tenant.
3. Rejected cross-tenant **writes** are not in the log (the error rolls the log row back) — look for `row-level security` errors in the server logs for those.

**Mitigate.** Page security. Roll back if a recent change introduced the path; block the caller if it is an attack.

### IAMTokenServiceRLSMissingGUC

**Meaning.** Queries ran without a valid `app.tenant_id` (`violation_type="missing_or_invalid_guc"`) for 10+ minutes. RLS returns zero rows rather than leaking data, so the symptom is a read that unexpectedly finds nothing (404 `principal_not_found`) or a write rejected by `WITH CHECK`.

**Triage.** `query_text` in `rls_violation_log` identifies the query. Typical causes: a repository call outside the GUC bridge (`pgcommon.GUCSetFromContext`), a new background path using the app pool instead of the BYPASSRLS `reconciler` pool for a cross-tenant enumeration (RLS-7), or a non-UUID tenant id. `check-set-local-only.sh` (RLS-6) already prevents a session-scoped `SET` at build time.

**Mitigate.** Fix the path; roll back if it is new.

## SLOs

Targets come from LLD §11 (SLO-3's 99% is an operational proxy); see the header of `deploy/monitoring/slo-rules.yml`.

### IAMTokenServiceSLO1_TS1LatencyFastBurn / IAMTokenServiceSLO1_TS1LatencySlowBurn

**Meaning.** TS-1 issue/rotate is missing its p99 ≤ 500 ms target faster than the 99% budget allows (14.4× fast, page; 6× slow, ticket).

**Triage.** As for `IAMTokenServiceHighLatency`, restricted to the TS-1 route: OpenBao write latency first, then Postgres. The LLD target excludes the OpenBao round-trip but the SLI cannot, so a burn fully explained by `iam_token_service_openbao_call_duration_seconds` is an OpenBao problem (`IAMTokenServiceOpenBaoCallLatencyHigh`), not a TS-1 regression.

### IAMTokenServiceSLO2_WriteErrorRateFastBurn

**Meaning.** More than 1.44% of mutating requests returned 5xx over 5 minutes, 14.4× the 99.9% budget.

**Triage.** As for `IAMTokenServiceHighErrorRate`, restricted to write routes. Usually `db_unavailable` or `secret_store_unavailable`; check `/readyz`.

### IAMTokenServiceSLO3_SweepSuccessRateLow

**Meaning.** The share of §8.3 rotator runs in the last hour whose sweep reported `result="ok"` has been below 99% for 20 minutes: a sustained degradation rather than a single failed run. The SLI counts runs — each run is its own short-lived pod whose counter is 1 for its result, read with `max_over_time` and summed across pods — because `rate()` over those never-changing per-pod counters is always 0. Runs whose RP-17 call failed count as failed here even though `keys_refresh_pending` repairs them later.

**Triage.** As for `IAMTokenServiceRotationSweepFailures`.

### IAMTokenServiceSLO4_TS3LatencyFastBurn / IAMTokenServiceSLO4_TS3LatencySlowBurn

**Meaning.** TS-3 (`GET …/service-accounts/:principal_id`) is missing its p99 ≤ 100 ms target faster than the 99% budget allows (14.4× fast, page; 6× slow, ticket).

**Triage.** TS-3 makes no OpenBao call, so this is Postgres: `platform_db_query_duration_seconds`, `platform_db_pool_acquire_duration_seconds{pool="default"}` (pool waits), `platform_db_slow_queries_total`. The credential list read uses `idx_sac_principal`; a plan change to a sequential scan shows here first.

## Caller-visible outcomes (no alert)

### credential_replay_expired (409)

**Meaning.** A TS-1 retry reused a `rotation_id` more than `ROTATION_REPLAY_WINDOW` (Helm `rotation.replayWindow`, default 15m; `0` disables the limit) after the credential it created was issued. The credential may still be live, but its private key is not returned again: a `rotation_id` is a retry key, not a standing read handle. `details.version` names the credential.

**Triage.** The caller is replaying a stale request (a retry loop that outlived its own timeout, or a replayed job). Distinct from `credential_replay_revoked` (the credential is gone).

**Mitigate.** The caller issues a new rotation with a fresh `rotation_id`. Raise the window only if a legitimate retry path needs longer.

### JWKS route returns 503 jwks_keys_unavailable

**Meaning.** The JWKS route could not read the tenant's active key, or any of its keys, from OpenBao and answered `503 jwks_keys_unavailable` rather than a key set missing the active key (`IAMTokenServiceJWKSKeyErrors` pages on it). Keycloak keeps its cached keys on a failed fetch. A set missing only an overlap key is still served (200).

**Triage.** As for `IAMTokenServiceJWKSKeyErrors`: OpenBao health first, then the one credential's path.

