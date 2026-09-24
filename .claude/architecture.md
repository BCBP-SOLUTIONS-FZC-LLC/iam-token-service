# Architecture — quick reference

Terse cheat-sheet for "is this import allowed" / "where does X live."
Deep-dive narrative + diagrams: root `ARCHITECTURE.md`.

## Package layout

```
cmd/
  server/      HTTP API (TS-1..TS-6 + EXT-6 JWKS route) + outbox runner  [composition root]
  consumer/    offboarding SQS subscriber                   [composition root]
  rotator/     overlap sweep + orphan reconciler + prune     [composition root]
  scheduler/   automatic cadence-driven rotation scan        [composition root]
internal/
  core/
    domain/    Credential, ServiceAccountPrincipal, Event, ErrorCode  [no internal imports]
    port/      repository/adapter interfaces                          [domain only]
    service/   CredentialService, PrincipalService, JWKSService, tracing.go [domain, port, requestctx]
  adapter/
    inbound/
      http/       Gin handlers, GUCBridgeMiddleware, errors.go, jwks_handler.go
      consumer/   OffboardingConsumer, dedup.go (ackUnknown/skipDuplicate)
    outbound/
      postgres/   repositories, RLS GUC binding, TxRunner, migrations/
      openbao/    KV v2 client, Kubernetes auth (client.go)
      httpx/      shared outbound-HTTP transport (traceparent injection, client spans)
      realmprovisioner/  RP-17 client (ClearServiceAccountKeysCache) — the one outbound HTTP call to another IAM service
      eventbus/   Publisher, ValidatingCodec, GlueCodec
        schemas/  embedded JSON Schemas (5 published + 1 consumed)
      metrics/    Prometheus instruments (3-tier taxonomy)
pkg/
  requestctx/  request-scoped context helpers
api/           embedded asyncapi.yaml
docs/swagger/  generated OpenAPI (make swag)
```

## Shared library dependencies

- **`platform-pgcommon`** — pool/RLS-GUC/retry (`pgcommon.Pool`, `SET LOCAL app.tenant_id`), `pgmetrics` (pool-stat Prometheus collector, wired in all 4 `cmd/*`).
- **`platform-events`** — envelope, transactional outbox (`outbox.Enqueue`/`outbox.Runner`), SNS/SQS transport, its own `outbox_*`/`sqs_*`/`events_*` metrics. `eventbus`'s `Codec`/`NoopCodec` are now type aliases to `events.Codec`/`events.NoopCodec` (library-alignment, matching iam-user-profile/iam-org-membership), not this service's own types.
- **`platform-gincommon`** — HTTP middleware chain, `/healthz`/`/readyz`, structured Zap logging, `MetricsRegisterer()`/`MetricsConstLabels()`. `cmd/consumer`'s health checks now run through this stack too (Gin + `ObservabilityMiddlewares`), not a bare `http.ServeMux` — probe traffic gets the same access logs/metrics/traces every other route does.
- **`openbao/openbao/api/v2`** — the only OpenBao SDK dependency; Kubernetes auth method only, no static-token code path exists.

## Dependency rules (`.go-arch-lint.yml`, enforced by `make lint`)

| Component | May depend on |
|---|---|
| `domain` | vendor only |
| `port` | `domain` |
| `service` | `domain`, `port`, `requestctx` |
| `observability` (`adapter/outbound/metrics`) | `domain`, `port`, `service` |
| `postgres` | `domain`, `port` |
| `openbao` | `domain`, `port` |
| `httpx` | vendor only |
| `adapters_outbound` (`eventbus`, `realmprovisioner`) | `domain`, `port`, `service`, `observability`, `httpx` |
| `adapters_inbound` (`http`, `consumer`) | `domain`, `port`, `service`, `requestctx`, `apispec`, `observability` |
| `reconciler_jobs` (`cmd/rotator`) | `domain`, `port`, `postgres`, `openbao`, `observability`, `adapters_outbound` — **never `service`** |
| `cmd` (`server`, `consumer`, `rotator`, `scheduler`) | everything |

**The one rule most worth remembering:** `cmd/rotator` cannot import
`internal/core/service` — it talks to `postgres`/`openbao` directly under
a `BYPASSRLS` connection (RLS-7), a privilege no `service`-mediated code
path may hold. This is why `cmd/rotator/sweep.go`/`orphan_reconciler.go`
duplicate a small amount of revoke logic that also exists in
`credential_service.go` — that duplication is intentional, not
accidental drift. **`cmd/scheduler` is the exception that proves it**
(§16 TSQ-6 Resolved, TS-D14): it also connects under `BYPASSRLS` for its
own cross-tenant due-list scan, but it lives under `cmd`, not
`reconciler_jobs` — it calls `CredentialService.IssueOrRotate` directly,
because rotating a credential is a `service`-layer write path, not the
narrow revoke-only concern `reconciler_jobs` is walled off to. Both
`cmd/rotator`'s sweep and `cmd/scheduler`'s scan call the new
`realmprovisioner` client (RP-17, `ClearServiceAccountKeysCache`) after
every revoke/rotate (TS-D15) — under EXT-6's client-jwt/JWKS mechanism
(rev 1.3) there is no Keycloak-side TTL, so skipping that call leaves a
revoked/superseded key authenticating at Keycloak indefinitely.

`postgres` and `openbao` are each their own component (not folded into
`adapters_outbound`) so **TS-INV-1** (no `gocloak` import anywhere) and
**TS-INV-2** (no credential plaintext reaches Postgres) are structurally
checkable in CI (`no-gocloak`, `no-secret-log` gates), not just
documented.

## Metrics naming (Enterprise Platform Observability Standard)

Enforced by `make gates` → `metrics-taxonomy`
(`.github/scripts/check-metrics-taxonomy.py`): every metric this service
defines must start with `platform_` (Tier 1) or `iam_` (Tier 2/3 — Tier 3
is `iam_token_service_*`), counters end `_total`, histograms end
`_seconds`, and a shared (`platform_`/`iam_`) metric name must never embed
this service's own name — see `internal/adapter/outbound/metrics/metrics.go`.
