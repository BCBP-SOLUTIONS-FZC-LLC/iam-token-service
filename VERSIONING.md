# Versioning

`iam-token-service` is a **deployed service, not an importable library** —
versioning applies to the container image, the Helm chart, and the runtime
contract (routes, event schemas, error codes, required configuration), not
to a Go module API. This document is modeled on the equivalent file in
sibling IAM services, adapted for this service's own frozen contract
surface (§25 of `docs/lld/iam-lld-token-service.md`).

## Semantic versioning (SemVer)

Releases are tagged `vMAJOR.MINOR.PATCH` (optionally with a pre-release
suffix, see below).

| Bump | Triggered by |
|---|---|
| **MAJOR** | Removing/renaming a route or its path shape; removing or renaming an event payload field; renaming/retiring one of the 5 frozen event names; changing an error `code` or its HTTP status; adding a **required** env var with no dev default; restructuring a `values.yaml` key that a consumer's overrides depend on; widening or changing the frozen OpenBao path shape |
| **MINOR** | A new endpoint; a new optional event payload field; a new metric; a new optional env var or `values.yaml` key with a safe default; a new consumer-facing capability that doesn't change existing behavior |
| **PATCH** | Bug fixes; performance improvements; dependency bumps; internal refactors; test/CI/doc changes with no runtime-contract effect |

### What counts as the runtime contract

| In scope (SemVer applies) | Out of scope (free to change without a version bump) |
|---|---|
| The 7 route paths and methods (TS-1..TS-6 and the EXT-6 JWKS route) and their request/response JSON shapes | Internal package layout, function/type names inside `internal/` |
| The 5 frozen event names (`ServiceAccountRegistered`, `…CredentialIssued`, `…CredentialRotated`, `…CredentialRevoked`, `ServiceAccountRevoked`) and their JSON Schemas | The Glue schema *version id* for a given event name (additive-only churn, not consumer-visible as a break) |
| The `error` codes in the §17 taxonomy (`missing_identity_headers`, `principal_not_found`, `rotation_in_flight`, `optimistic_lock_conflict`, `principal_revoked`, `secret_store_unavailable`, `invalid_request`), plus the additive `db_unavailable`, `credential_replay_revoked`, `credential_replay_expired`, `jwks_keys_unavailable`, `rate_limited`, `tenant_path_mismatch`, `unsupported_media_type` and `internal_error` codes (`internal/core/domain/errors.go` documents each as additive to the frozen taxonomy, same pattern), and their HTTP status | Log line formats, trace span names beyond `credential.issue_rotate`/`credential.revoke` |
| The frozen OpenBao path shape `iam/serviceaccount/<tenant_id>/<keycloak_client_id>/v<version>` | The OpenBao SDK version pinned in `go.mod` |
| Required env vars with no dev-safe default (`OPENBAO_ADDR`, `GLUE_REGISTRY_NAME`, `SNS_TOPIC_ARN`, `SQS_QUEUE_URL`, `REALM_PROVISIONER_BASE_URL`; `OPENBAO_ROLE`/`OPENBAO_KV_MOUNT` default to `iam-token-service`/`iam`) | Tunable env vars with a safe default (`OUTBOX_*`, `ROTATION_DEFAULT_*`, `PG_MAX_CONNS`, ...) |
| The `iam_token_service_*` metric **names** (§11.2) | Metric label cardinality additions that don't remove an existing label; the Tier-1 `platform_*` and Tier-2 `iam_*` metrics proposed in `docs/observability-registry-proposals.md` (not yet part of this frozen set until a governance reviewer ratifies them) |
| `values.yaml` keys a consumer's own override file is likely to set (`image.repository`, `database.*`, `openbao.*`, `sqs.*`, `service.port`/`metricsPort`) | Internal chart template structure, resource defaults |

A retired route, event name, or error code is **never reused** for a
different meaning — once retired, that identifier is permanently off the
table.

### Guarantees

This service is currently pre-`v1.0.0` (nothing has been tagged or
deployed yet) — under SemVer §4, **anything may change at any time** while
in `0.x`. Treat every table above as a design target this document commits
the project to reaching at `v1.0.0`, not a guarantee in force today. Once
`v1.0.0` ships: MAJOR bumps are rare and always called out in
`CHANGELOG.md` with a migration note; MINOR and PATCH releases are always
safe to adopt without a consumer-side code change.

## Supported releases

No release has shipped yet — `CHANGELOG.md`'s `[Unreleased]` section is
still the entire history (`git log` shows one commit). This section will
track the currently-supported line(s) once a `v1.0.0` exists; until then,
`main` is the only supported ref.

## Consume a release

### Pull and verify the image

```bash
docker pull ghcr.io/bcbp-solutions-fzc-llc/iam-token-service:v1.0.0
cosign verify ghcr.io/bcbp-solutions-fzc-llc/iam-token-service:v1.0.0 \
  --certificate-identity-regexp '.*' --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

`release.yml` signs every published image with `cosign` (keyless, GitHub
OIDC identity) and attaches an SBOM + provenance attestation — verify
before deploying anything pulled from `ghcr.io`.

### Image tag scheme

| Tag | Produced when |
|---|---|
| `vX.Y.Z` | Every tagged release — immutable, the only tag to pin a production deployment to |
| `vX.Y` | Floating alias to the latest `Z` within that minor line |
| `vX` | Floating alias to the latest `Y.Z` within that major line |
| `latest` | Floating alias to the most recent release |

### Deploy via Helm

```bash
helm upgrade --install iam-token-service deploy/helm \
  --set image.tag=v1.0.0 \
  -f values-prod.yaml
```

`Chart.yaml`'s `appVersion` tracks the application release
(`vX.Y.Z` without the `v`); `version` is the chart's own release number and
bumps independently — a chart-only fix (e.g. a `values.yaml` default, a
NetworkPolicy rule) can ship as a new chart `version` against an unchanged
`appVersion`.

## Maintainer release process

1. Merge the change to `main`; ensure `CHANGELOG.md` has an entry under
   `[Unreleased]` (enforced by `changelog-check.yml` on every PR).
2. Move the relevant `[Unreleased]` entries under a new `## [X.Y.Z] -
   YYYY-MM-DD` heading.
3. Bump `deploy/helm/Chart.yaml`'s `version` (chart) and `appVersion`
   (application) to match.
4. Run `make ci` locally (build + vet + lint + gates + test-ci) — do not
   rely on CI alone to catch a broken release.
5. `git tag vX.Y.Z && git push origin vX.Y.Z` — this is the only trigger
   for `release.yml`.
6. `release.yml` runs: validate/test (reuses `ci.yml`'s gates) → build
   (tag/CHANGELOG-entry gate + cross-compiled binaries for all three
   entrypoints) → docker (build, push to GHCR, Trivy scan, Cosign sign,
   SBOM + provenance attach) → deploy-gate (skipped unless
   `DEPLOY_GATE_ENABLED` and a target cluster are configured — **not yet
   wired for any environment**, since this service has never been
   deployed) → publish (GitHub Release from the CHANGELOG section).
7. Notify consumers (Realm Provisioner, Org & Membership, Audit Log
   owners) of any MAJOR change per the compatibility table above.

**Known current gaps, stated plainly rather than glossed over:** the
`deploy-gate` job has no target cluster configured anywhere yet (it is a
real no-op, not a placeholder that will silently pass); no image has ever
been tagged or pushed; `Chart.yaml` currently reads `version: 0.1.0` /
`appVersion: "1.0.0"`, which will need reconciling at the actual `v1.0.0`
cut rather than left as a mismatched starting point.

### Pre-release tags (optional)

| Suffix | Meaning |
|---|---|
| `-rc.N` | Release candidate — feature-complete, undergoing final validation |
| `-beta.N` | Early access, contract may still shift before the final tag |

Both match `release.yml`'s tag trigger pattern
(`v[0-9]*.[0-9]*.[0-9]*-*`) and produce a real signed image, just not the
`latest`/`vX`/`vX.Y` floating aliases.

## Compatibility matrix

| iam-token-service | Go | `platform-pgcommon` | `platform-events` | `platform-gincommon` |
|---|---|---|---|---|
| `main` (unreleased) | 1.26.6 | see `go.mod` | see `go.mod` | see `go.mod` |

This table gains a row per tagged release once releases exist; check
`go.mod` directly for the exact pinned versions of shared platform
libraries at any given commit — this file does not duplicate `go.mod`'s
own authority over exact version strings.

## Related files

| File | What it covers |
|---|---|
| `CHANGELOG.md` | Human-readable history, Keep a Changelog format |
| `README.md` | Onboarding, API overview, environment variables |
| `ARCHITECTURE.md` | Deep-dive mechanisms and diagrams |
| `docs/lld/iam-lld-token-service.md` | The signed-off design (§25 is the frozen name inventory this document's contract table mirrors) |
| `deploy/helm/Chart.yaml` | The chart `version` / `appVersion` this process bumps |
| `.github/workflows/release.yml` | The automation this process describes |
| `.github/workflows/changelog-check.yml` | The PR-time enforcement that a `CHANGELOG.md` entry exists |
| `go.mod` | Exact pinned versions of every shared platform library |
