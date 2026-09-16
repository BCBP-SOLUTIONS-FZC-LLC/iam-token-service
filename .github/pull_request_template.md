## Description

<!-- What does this PR change, and why? Link the LLD section(s) it implements or affects. -->

## Type of Change

- [ ] Bug fix
- [ ] New feature / milestone work
- [ ] Refactor (no behavior change)
- [ ] Documentation
- [ ] CI/CD or deployment
- [ ] Migration (schema change)

## Testing

- [ ] `make test-unit` passes
- [ ] `make test-contract` passes (§14.3, EVT-2)
- [ ] `make test-postgres` passes (RLS + repository integration, testcontainers)
- [ ] `make test-integration` passes, if this touches OpenBao/AWS wiring
- [ ] `go vet ./... && go vet -tags=integration ./...` clean
- [ ] `go-arch-lint check` clean (no new architecture-boundary violation)

## Checklist

### Code Quality
- [ ] `make fmt-check` clean
- [ ] No new `golangci-lint` findings
- [ ] Every frozen name (env var, header, metric, event name, OpenBao path shape) matches LLD §25 character-for-character

### Event Contract (if `api/asyncapi.yaml` or `internal/adapter/outbound/eventbus/schemas/` changed)
- [ ] `api/asyncapi.yaml` updated in the same commit as any schema change
- [ ] Change is additive (new optional field) — a breaking change requires a new Glue schema name, not an in-place edit (§7.3.1 compaction discipline)

### TS-INV-1 / Secret Handling
- [ ] No new import of a Keycloak Admin API client anywhere in this diff (TS-INV-1 — this service has no Keycloak dependency at all)
- [ ] No credential/secret field name reaches a log sink, trace span, or metric label (TS-INV-2)
- [ ] Any new OpenBao path follows the frozen `iam/serviceaccount/<tenant_id>/<keycloak_client_id>/v<version>` shape (§6.3, §25) — no widened ACL scope in `deploy/openbao/policy.hcl` without platform review

### RLS / Tenant Isolation
- [ ] Any new tenant-scoped query binds `app.tenant_id` via `pgcommon.WithGUCSet`/`GUCSetFromContext` (transaction-local `SET LOCAL`), never a session-scoped `SET` (RLS-6)
- [ ] Any new table touching tenant data has `FORCE ROW LEVEL SECURITY` and an RLS policy

### Idempotency / Replay Safety
- [ ] Rotation/issue paths remain safe under retry (rotation_id replay, §9.2)
- [ ] Offboarding cascade handling remains idempotent under SQS at-least-once redelivery

### Database / Migrations
- [ ] Migration is additive/backward-compatible, or a deliberate breaking change is called out and approved
- [ ] `serviceaccount_app` / `serviceaccount_reconciler` / `serviceaccount_migrator` grants are unchanged, or the change is deliberate and reviewed

### Security
- [ ] No long-lived AWS credentials introduced (OIDC only, §13.5)
- [ ] No new dependency added without checking its license/provenance

### Documentation
- [ ] `CHANGELOG.md` updated if this PR touches `internal/`, `api/`, `deploy/`, `cmd/`, or `pkg/`
- [ ] Relevant doc comments / runbooks updated

## Related Issue

<!-- Closes #... -->

## Deployment Notes

<!-- Any values.yaml changes, new env vars, migration ordering constraints, or rollout considerations. -->
