# Architecture — quick reference

Terse cheat-sheet for "is this import allowed" / "where does X live."
Deep-dive narrative + diagrams: root `ARCHITECTURE.md`.

## Package layout

```
cmd/
  server/      HTTP API (TS-1..TS-6 + EXT-6 JWKS route) + outbox runner;
               MIGRATE_ONLY=true = the Helm migrate hook Job's mode.
               exporters.go: rotation-overlap gauge, RLS-violation counter,
               keys_refresh_pending gauges; the JWKS known-tenant refresher
               runs here too (jwks_handler.go)            [composition root]
  consumer/    offboarding SQS subscriber: main.go, inbound_schema.go
               (validateConsumed), dlq.go (DLQ router)    [composition root]
  rotator/     sweep.go, orphan_reconciler.go, prune.go, keys_refresh.go
               (RP-17 marker retry)                       [composition root + reconciler_jobs]
  scheduler/   scan.go (cadence rotation), keys_refresh.go [composition root]
internal/
  core/
    domain/    Credential, ServiceAccountPrincipal, Event payloads, ErrorCode  [no internal imports]
    port/      repository/adapter interfaces, tx/publisher context helpers,
               inbox.go (Inbox/InboxPruner), realm_provisioner_client.go  [domain only]
    service/   CredentialService, PrincipalService, JWKSService, key generator,
               tracing.go (span wrappers)                 [domain, port, requestctx]
  adapter/
    inbound/
      http/       router.go, middleware.go (RequireIdentityHeaders, GUCBridgeMiddleware,
                  RequireTenantPathMatch, RequireJSONContentType), errors.go (HandleError),
                  credential/principal handlers, jwks_handler.go (rate limiters,
                  known-tenant set), asyncapi/swagger viewers
      consumer/   OffboardingConsumer, dedup.go (processOnce/ackUnknown over port.Inbox)
    outbound/
      postgres/   repositories, db.go (TxRunner, withPool, wrapConnErrCtx),
                  inbox_repository.go (platform-events pkg/inbox),
                  reconciler_repository.go, jwks_tenants_repository.go,
                  keys_refresh_repository.go, rls_violation_repository.go,
                  migrate.go, migrations/ (000001..000005)
      openbao/    KV v2 client, Kubernetes auth (client.go)
      httpx/      shared outbound-HTTP transport (traceparent injection, client spans)
      realmprovisioner/  RP-17 client (ClearServiceAccountKeysCache) — the one outbound
                  HTTP call to another IAM service
      eventbus/   Publisher, ValidatingCodec, GlueCodec, GlueDecoder
        schemas/  embedded JSON Schemas (5 published + 1 consumed)
      metrics/    Prometheus instruments (3-tier taxonomy) + decorators for the
                  credential service, secret store and RP-17 client
pkg/
  requestctx/  request-scoped context helpers
api/           embedded asyncapi.yaml
docs/swagger/  generated OpenAPI (make swag)
```

## Shared library dependencies

Pinned in `go.mod`: platform-gincommon v1.6.0, platform-events/v2 v2.0.0,
platform-pgcommon/v2 v2.0.0.

- **`platform-pgcommon`** — pool/RLS-GUC/retry (`pgcommon.Pool`, `SET LOCAL app.tenant_id`), per-transaction `PG_STATEMENT_TIMEOUT`/`PG_LOCK_TIMEOUT` (`SET LOCAL`, app and reconciler pools), `pgmetrics` (`platform_db_*`, `pool` label `default`/`reconciler`); `Config.Tracer` takes gincommon's `NewSpanTracer`.
- **`platform-events`** — envelope, transactional outbox (`outbox.Enqueue`/`outbox.Runner`, dead letters, `PrunePublished`), SNS/SQS transport, `pkg/inbox` (consumer dedup over `processed_events`), queue-depth sampler, its own `platform_*` metrics. `eventbus`'s `Codec`/`NoopCodec` are type aliases to `events.Codec`/`events.NoopCodec`.
- **`platform-gincommon`** — HTTP middleware chain, `/healthz`, structured Zap logging, `MetricsRegisterer()`/`MetricsConstLabels()`, `InitTracingWithConfig`, `MetricsHandler`, `NewSpanTracer`/`NewTracer`. `cmd/consumer`'s health router also runs through `ObservabilityMiddlewares`.
- **`openbao/openbao/api/v2`** — the only OpenBao SDK dependency; Kubernetes auth only, no static-token code path.

## Dependency rules (`.go-arch-lint.yml`, enforced by `make arch-lint`)

| Component | May depend on |
|---|---|
| `domain` | vendor only |
| `port` | `domain` |
| `service` | `domain`, `port`, `requestctx` |
| `requestctx`, `apispec` (`api/`), `swaggerdocs` (`docs/swagger`) | vendor only |
| `observability` (`adapter/outbound/metrics`) | `domain`, `port`, `service` |
| `postgres` | `domain`, `port` |
| `openbao` | `domain`, `port` |
| `httpx` | vendor only |
| `adapters_outbound` (`eventbus`, `realmprovisioner`) | `domain`, `port`, `service`, `observability`, `httpx` |
| `adapters_inbound` (`http`, `consumer`) | `domain`, `port`, `service`, `requestctx`, `apispec`, `observability` |
| `reconciler_jobs` (`cmd/rotator`) | `domain`, `port`, `postgres`, `openbao`, `observability`, `adapters_outbound` — **never `service`** |
| `cmd` (`server`, `consumer`, `rotator`, `scheduler`) | everything above except `httpx` |

`test/`, `scripts/`, `deploy/` and `*_test.go` are excluded; `deepScan` is
off (import-level checks only).

**The one rule most worth remembering:** `cmd/rotator` cannot import
`internal/core/service` — it talks to `postgres`/`openbao` directly, with
cross-tenant enumeration on the `BYPASSRLS` reconciler pool and every
write in an RLS-scoped app-pool transaction (RLS-7). That is why
`sweep.go` and `orphan_reconciler.go` carry a small copy of the revoke
logic and of the 5s in-lock OpenBao bound (`inLockSecretTimeout`) that
also live in `credential_service.go` — intentional, not drift.
**`cmd/scheduler` is the exception** (§16 TSQ-6 Resolved, TS-D14): it
also enumerates under `BYPASSRLS`, but sits only under `cmd`, so it calls
`CredentialService.IssueOrRotate` (rotation is a `service`-layer write
path). Both CronJobs call the `realmprovisioner` client (RP-17,
`ClearServiceAccountKeysCache`) once per tenant, inline after that
tenant's revokes/rotations, and keep a durable `keys_refresh_pending`
marker so a failed or never-made call is retried by the next run
(TS-D15, TS-D22/23). Under EXT-6 there is no Keycloak-side TTL, so a
missed RP-17 leaves a revoked/superseded key authenticating.

`postgres` and `openbao` are each their own component (not folded into
`adapters_outbound`) so **TS-INV-1** (no `gocloak` import anywhere) and
**TS-INV-2** (no credential plaintext reaches Postgres, logs, errors or
span attributes) are structurally checkable in CI (`no-gocloak`,
`no-secret-log` gates), not just documented.

## Metrics naming (Enterprise Platform Observability Standard)

Enforced by `make gates` → `metrics-taxonomy`
(`.github/scripts/check-metrics-taxonomy.py`): every metric this service
defines must start with `platform_` (Tier 1) or `iam_` (Tier 2/3 — Tier 3
is `iam_token_service_*`), counters end `_total`, histograms end
`_seconds`, and a shared (`platform_`/`iam_`) metric name must never embed
this service's own name — see `internal/adapter/outbound/metrics/metrics.go`.
`make metrics-lint` additionally runs platform-gincommon's `metricslint` on
a real scrape and checks `docs/observability/metric-registry.md` for drift.
