# Changelog

All notable changes to this service are documented here. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning aligned with SemVer.

This service has never been deployed to any environment — there is no released version yet, so everything lives under `[Unreleased]`.

## [Unreleased]

### Added

- **Enterprise Platform Observability Standard adoption** — metrics now follow the platform/domain/service-specific three-tier taxonomy:
  - Centrally-injected labels (`domain`, `service`, `environment`) via `metrics.Register(environment)` — instrumentation call sites can no longer omit or misspell them (requirement #8).
  - Three metrics proposed for the Platform/IAM Domain registries, dual-emitted alongside their legacy `iam_token_service_*` equivalent during the compatibility period (see `docs/observability-registry-proposals.md` for the full submission): `platform_dependency_request_seconds{domain,service,environment,dependency,operation}` (parallels `iam_token_service_openbao_call_duration_seconds`), `platform_duplicate_messages_total{domain,service,environment,queue}` (parallels `iam_token_service_processed_events_duplicates_total`), `iam_offboarding_cascade_total{service,environment,outcome}` (parallels `iam_token_service_offboarding_cascade_total`).
  - New CI gate `make metrics-taxonomy` (`.github/scripts/check-metrics-taxonomy.py`), wired into `make gates`, enforcing namespace-prefix classification and the counter-`_total`/histogram-`_seconds` naming rules.
  - No existing metric was renamed or removed — this is purely additive; alerts/dashboards remain on the legacy `iam_token_service_*` names until the proposed metrics are ratified.

### Documentation

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

Production-readiness hardening pass (adversarial security/correctness/operational review):

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
