# Token Service — Low-Level Design

## Tender Management SaaS Platform — IAM Subsystem

| Field | Value |
|---|---|
| Document type | Low-Level Design (LLD) |
| Service | `iam-token-service` (Token Service) |
| Go module | `github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service` |
| Status | **Approved — design sign-off v1.0 (2026-09-15); amended v1.1 (2026-09-17, §25 name-freeze amendment), v1.2 (2026-09-17, TSQ-6 resolution), v1.3 (2026-09-18, EXT-6 — TSQ-1's dual-secret-grace assumption corrected to the verified client-jwt/JWKS mechanism), and v1.4 (2026-09-23, event-pipeline hardening — see revision history). Maintained as a living document through TS-D23 (2026-10-07); still rev 1.4.** Names frozen (§25); all open questions resolved (§16). Brought forward from Phase 2 per HLD v1.45 §5.8/§11.7/§16/§17.1. Signed off by the document owner (IAM platform engineering); cross-service companion guards are tracked as follow-ups (§16 sign-off record), to be confirmed as those siblings and environments come up. |
| Base HLD | `IAM HLD v1.45` (Service Account & Token Service in MVP) |
| Sibling LLDs | `iam-lld-realm-provisioner-service.md` (mints the Keycloak service-account client; caller of TS-1/TS-4, §2.5/RP-17/RP-18), `iam-lld-org-membership-service.md` (non-member guarantee, AUTH-9), `iam-lld-authz-enrichment.md`, `iam-lld-tender-acl-service.md`, `iam-lld-delegation-service.md`, `iam-lld-event-consumer-service.md` |
| Owner database | RDS PostgreSQL `serviceaccount` (Multi-AZ, PgBouncer transaction pooling) |
| External system of record | **OpenBao** (credential *material*); **Keycloak** (the service-account client identity and `client_assertion` validation against this service's JWKS, written only by the Realm Provisioner, RP-INV-1) |
| Deployment stage | In development — nothing deployed to any environment (§13/§19) |
| Audience | IAM platform engineering (owner), Realm Provisioner team (credential handshake), Security / SRE, Audit Log |

### Revision history

| Rev | Date | Change |
|---|---|---|
| 0.1 | 2026-09-11 | **Initial draft.** Created as the companion service to the per-tenant automation principal brought forward into MVP (HLD §5.8/§11.7). Scope, boundaries, data model, credential lifecycle (issue/rotate/revoke), the Realm Provisioner handshake (RP-INV-1 preserved — this service never writes Keycloak), OpenBao secret handling, event contract, RLS/security invariants, and frozen-name inventory. PAT self-service and tenant-owned bots are documented as post-launch extensions of the same tables, not MVP surface. |
| 0.2 | 2026-09-11 | **Structural alignment to the sibling LLD template (HLD v1.45).** Renumbered to the canonical 25-section scheme shared with `iam-lld-event-consumer-service.md`; added Table of Contents and the previously-absent sections — §6 Credential Lifecycle and Secret Custody, §9 Concurrency/Consistency/Failure Handling, §12 Configuration, §14 Testing Strategy, §17 Error Taxonomy, §18 Integration Details, §19 Migration Strategy, §20 Operational Considerations, §21 Performance Considerations, §22 Decision Register, §23 Glossary, §24 Operational Runbooks. Disaster-recovery content folded into §13/§15; capacity content folded into §21. Also corrected §4.2/§4.4/§4.6 to state that `outbox_events` is library-owned (`platform-events`, `outbox.ApplySchema`) — this service only grants its app role on the table and customizes the payload column (MIG-1/MIG-2), matching the Realm Provisioner sibling. No design change — content preserved and expanded to sibling depth. |
| 0.3 | 2026-09-11 | **TSQ-3 resolved — plaintext-once handoff.** TS-1 returns the generated secret exactly once to the Realm Provisioner for the Keycloak update while retaining the authoritative copy in OpenBao (chosen over an OpenBao-handle-only response). Keeps RP the sole Keycloak writer (RP-INV-1) without granting RP OpenBao read access, and keeps a single issuance flow. §16 TSQ-3 marked Resolved; TS-D3 hardened from "leaning" to a firm decision. No structural change — the design already described this model (§5.4/§6.1/§10.5); this revision removes the remaining open-question hedging. |
| 0.4 | 2026-09-11 | **TSQ-5 resolved — proceed with the current design.** The base-HLD version mismatch (this LLD cites v1.45; the repository snapshot is v1.41) is acknowledged as an editorial inconsistency; the automation-principal design is materially identical across v1.41–v1.46, so no design change is required. The HLD maintainer will align the version reference in a follow-up. §16 TSQ-5 marked Resolved; the `HLD v1.45` references are left as-is pending that follow-up. |
| 0.5 | 2026-09-11 | **TSQ-2 resolved — rotation cadence & on-demand surface.** Default rotation cadence set to **90 days**; MVP supports both scheduled and operator-initiated on-demand rotation (O&M tooling / admin APIs, for break-glass and suspected-compromise), with tenant self-service deferred to post-launch. Rotation-overlap mechanism unchanged. §16 TSQ-2 marked Resolved; new decision **TS-D10**; §12 config comment firmed. No structural change — the design already carried the 90-day default and the operator/cron TS-1 paths (§8.2/§8.3/§12). |
| 0.6 | 2026-09-11 | **TSQ-1 resolved — Keycloak dual-secret grace accepted as a design assumption, pending ops validation.** The rotation/overlap design (§6.2/§8.2, TS-D4) assumes Keycloak client-secret rotation with a dual-secret grace period on the `platform-automation` template. Realm Provisioner + Keycloak Ops must validate that the deployed configuration supports the rotated-secret grace TTL and that it satisfies the overlap requirement; if dual-secret validation is not supported, the rotation and overlap assumptions must be revisited before implementation. §16 TSQ-1 marked Resolved (accepted assumption + validation gate); §6.2 and TS-D4 annotated accordingly. |
| 0.7 | 2026-09-15 | **Deepened §3, §4, §5, §7 to the `iam-lld-user-profile.md` sibling template.** §3 gains the full annotated repo tree, the `require(...)` shared-lib block, the CI dependency-rule gates, and a `platform-*` integration touch-point map with per-library symbol tables (3.3.1 gincommon / 3.3.2 pgcommon / 3.3.3 platform-events). §4 gains the DB/PgBouncer/pool preamble, an ER-overview + mermaid ER diagram, per-table DDL with indexes and Notes, the full RLS policy SQL (`app_tenant_id()`/`rls_check_tenant()`, `FORCE`/`REVOKE`/policy, least-privilege + `admin_readonly` roles, the RLS invariant/test matrix), expanded migrations, and the actual `touch_row` trigger SQL. §5 gains bulleted conventions, per-caller authorization, the endpoint-catalogue table, and full request/response JSON specs per endpoint (TS-1..TS-4) plus a status-code table. §7 gains the JSON-serialization rationale, the AsyncAPI-contract + Glue-registry-layout/evolution/wiring subsection (7.3/7.3.1), the SNS fan-out table, and the published-events + invariants tables. **No design change** — same tables, endpoints, events, and invariants; depth and structure only. |
| **1.1** | **2026-09-17** | **§25 name-freeze amendment — `keycloak_client_id` is no longer a single frozen literal (IB-2 / RP-1 coordination).** The Realm Provisioner's RP-1 (trial provisioning) mints the automation principal into the **shared** trial realm, which cannot hold two Keycloak clients named `platform-automation` (Keycloak `clientId` is unique per realm, not per tenant). TS-4's validation is relaxed from `keycloak_client_id == "platform-automation"` to `domain.ValidPlatformAutomationClientID`: either the literal base name (a dedicated-realm tenant, RP-2 direct or RP-3 post-conversion) or `"platform-automation-" + tenant_id` (a shared-realm/trial tenant, RP-1). §4.2/§4.4/§5.1/§5.4/§10.3/§23/§25 updated accordingly. **Also**, TS-4's `Register` conflict behavior changes from `ON CONFLICT ... DO NOTHING` to a conditional `DO UPDATE`: RP-3 conversion re-registers the same `(tenant_id, principal_type)` against a newly-minted dedicated-realm client, which necessarily carries a different `principal_sub` (Keycloak does not let a caller pin a service-account user's `sub` across realms the way `MigrateUser` pins a human user's). A differing `principal_sub`/`keycloak_client_id` on repeat now updates the existing row in place (same `principal_id`) and re-emits `ServiceAccountRegistered`; an identical repeat remains a true no-op. §5.4 updated. No table/column/event-name change — the `keycloak_client_id` column already stored a per-row value (§4.4's existing note), only the MVP validation and conflict-handling narrowed by this design were relaxed. Coordinated with `iam-realm-provisioner`'s RP-17/RP-18 implementation (its own LLD §2.5). |
| **1.0** | **2026-09-15** | **Design sign-off.** All open questions resolved (TSQ-1..5, §16); name inventory frozen (§25, TSQ-4). Status → **Approved (v1.0)**. Added the §16.1 sign-off record: pre-deployment design sign-off by the document owner; Security/SRE review and the Audit Log reconciliation deferred to implementation; three cross-service follow-ups (TSQ-1 realm-template verification, O&M non-member guard, Audit entry-type contract) tracked past sign-off. No design change from rev 0.10 — this revision records the sign-off and freezes names. |
| 0.10 | 2026-09-15 | **Closed the cross-tenant access gap for the scheduled jobs (review finding).** The overlap-expiry sweep (§8.3) and orphan-material reconciler (§8.6) are cross-tenant batch jobs, but the stated RLS posture (`serviceaccount_app` has no `BYPASSRLS`; the service "never uses `admin_readonly`") gave them no way to enumerate candidate rows — a fail-closed `SELECT` returns nothing. Added a dedicated read-only `serviceaccount_reconciler` role (`NOLOGIN BYPASSRLS`, `SELECT`-only) that the crons use **only to enumerate** the candidate set; each revoke write is then performed as `serviceaccount_app` in an RLS-scoped `RunInTx` bound to the row's tenant (new invariant **RLS-7** — writes never bypass RLS). Updated §4.3, §4.4 (role list), §8.3, §8.6, §13.1, RLS-1, and added §14.2 cross-tenant + orphan tests. No contract/table/event change. |
| 0.9 | 2026-09-15 | **TSQ-1 reframed to dev-stage reality.** Nothing is deployed (all IAM services and Keycloak are in development), so TSQ-1 cannot be a "confirm the deployed Keycloak" check. Reworded §16 TSQ-1, TS-D4, and the §6.2 inline note: the rotated-secret grace TTL is a **design-time coordination item with the Realm Provisioner** (who own the `platform-automation` realm template), verified in RP dev/integration tests (the §14.4 e2e case), not a deployment validation or a blocked external dependency. No design change. |
| 0.8 | 2026-09-15 | **Pre-sign-off review fixes.** (1) **Correctness:** added `updated_at` to `service_account_credentials` — the `touch_row` trigger set a column that did not exist; §4.2/§4.5/§9.1 reconciled. (2) **Design gap closed:** defined the orphan-material reconciler (`cmd/rotator` `ReconcileOrphanedMaterial`, new §8.6, TS-D11) that the §9.3/§13.3 failure paths relied on but never specified; added its metric (§11.2) and alert (§11.5/§20.3). (3) **Security-semantics gap closed:** made explicit that a TS-2 revoke does **not** invalidate the secret at Keycloak — break-glass is a two-halves operation requiring the paired RP action (new TS-INV-7, new §8.7 flow, TS-D12; §5.4 TS-2, §16 TSQ-2, §24 updated). (4) **Cross-refs:** repointed the CI-gate cites (§3.3→§3.2) and the outbox cite (§7.3→§7.4) left stale by the rev-0.7 renumbering. No change to the frozen name inventory (§25). |
| **1.4** | **2026-09-23** | **Event-pipeline hardening, aligned with `iam-org-membership` rev 2.12 / `iam-realm-provisioner` rev 1.18 — no table, route, or frozen event-name change.** (1) **Consumer Glue decode (real gap):** `cmd/consumer` built its SQS consumer with no `events.WithConsumerCodec`, and O&M publishes `TenantMembershipsPurged` Glue-encoded — every such message would have failed decode and ended in the DLQ with the offboarding cascade never run. It now carries `eventbus.GlueDecoder` (decode-only, no registry). (2) **Consumed-payload validation (§7.1):** `cmd/consumer/inbound_schema.go`'s `validateConsumed` checks each decoded payload against the embedded `tenant_memberships_purged.json` before `Handle` (`ValidatingCodec.Validate` + `ErrNoSchema`; produced-event `Encode` keeps its fail-open behaviour for unknown types). O&M's producer schema was verified compatible (it requires `tenant_id` too, plus an optional `actor_id`). New Tier-3 `iam_token_service_consumed_schema_violations_total{consumer,event_type}` and critical `IAMTokenServiceConsumedSchemaViolation`. (3) **Straight-to-DLQ for permanent rejects:** `cmd/consumer/dlq.go` sends a schema violation to `tenant-lifecycle-tokensvc-q-dlq` (`DLQReason=schema_violation`, URL from the queue's own `RedrivePolicy`) and acks, instead of burning `maxReceiveCount` retries; unresolvable DLQ or a failed send fall back to normal redrive. New `sqs:SendMessage` grant `OffboardingDLQPermanentRejects` (`deploy/iam/policy.json`); the consumer now builds one `*sqs.Client` shared with `events.NewSQSConsumerWithClient`. (4) **Glue version resolution by definition (§7.3.1):** `GlueCodec` resolves each produced schema's version UUID once at startup via `glue:GetSchemaByDefinition` with the embedded schema in exactly `schema-gov register`'s compact form (`registeredDefinition`, Python-parity-tested), requiring `AVAILABLE`; the `GetSchemaVersion(LatestVersion)` prefetch, the 5-minute `StartRefresher`, `WithLogger` and `buildGlueCodec`'s logger parameter are deleted. New floci integration test `test/integration/glue_codec_test.go`. |
| **1.3** | **2026-09-18** | **EXT-6 — TSQ-1's "Keycloak dual-secret grace" assumption was false; corrected to the verified client-jwt/JWKS mechanism.** Investigation (backlog item EXT-6) proved empirically against a real Keycloak 26.0 instance that TSQ-1/TS-D4's premise — a rotated-secret grace TTL on the `platform-automation` client's plain client-secret authenticator — **does not exist**: Keycloak's client-secret authenticator stores exactly one secret per client; applying a new one via `UpdateClient` immediately invalidates the old one, with no realm-template setting for a second still-valid secret. The design is corrected, not patched: `platform-automation` now authenticates via Keycloak's **`client-jwt`** (private_key_jwt) authenticator with `use.jwks.url=true`, pointing at a JWKS this service serves (new §2.5-equivalent capability, `JWKSService` + `GET .../service-accounts/platform-automation/jwks.json`, registered outside the protected middleware group since Keycloak's outbound fetch carries no caller identity). `DefaultKeyGenerator` (renamed from `DefaultSecretGenerator`) now generates an RSA-2048 keypair instead of a random string; TS-1's response field (`secret`, name kept for wire stability) is a PEM-encoded private key, never a shared secret. Genuine multi-key overlap **is** real this way — verified two keys serve and authenticate simultaneously — with two operational corrections to the original optimistic sketch: Keycloak does not self-refresh a `jwks.url` client's keys (a new key is unrecognized, and a removed key keeps authenticating, until the Realm Provisioner explicitly calls `ClearServiceAccountKeysCache`, wrapping `POST /admin/realms/{realm}/clear-keys-cache`); this is now RP-17's actual shape (`RefreshServiceAccountKeys`, was `ApplyServiceAccountSecret`) — see the RP LLD's own EXT-6 revision. TSQ-1, TS-D4, and the §16.1 follow-up item are updated below; TS-INV-7's two-halves rule is **validated, not replaced** — the caller-responsibility half is now concretely "call Keycloak's clear-keys-cache," not "apply a new secret." No frozen name (§25) changed; `secret`/`SecretGenerator` field/type names are kept verbatim for wire/interface stability despite the semantic change. |
| **1.2** | **2026-09-17** | **TSQ-6 resolved — automatic cadence-driven rotation via a new `cmd/scheduler` component (IB-5).** Closes the gap TSQ-6 (Added 2026-09-17) identified: nothing tracked rotation-due state or drove TS-1 automatically on the 90-day cadence (TS-D10/TSQ-2). Resolution: (1) `rotation_cadence_days`/`next_rotation_at` are stored on the `active` row in `service_account_credentials` (new columns + `idx_sac_next_rotation` partial index, §4.2) and exposed read-only through TS-3 (§5.4); (2) a new composition root, **`cmd/scheduler`** (a fourth binary in the shared image, alongside `server`/`consumer`/`rotator`), scans the due-list and calls TS-1 automatically, authenticating as a **dedicated service identity** — a new in-mesh caller added to the existing `x-user-id: iam-system` / NetworkPolicy allow-list alongside the Realm Provisioner and operator/O&M callers (§5.2/§10.4), not a new authorization mechanism; (3) operator-triggered on-demand rotation (§8.2) is unchanged and remains the break-glass/suspected-compromise path; (4) `cmd/rotator` is explicitly reconfirmed as retirement/cleanup-only (§8.3/§8.6/outbox-prune) — it does not and will not initiate rotations. §16 TSQ-6 marked **Resolved**; TS-D10 updated; new decision **TS-D14**; §4.2, §5.4, §8.2, §12, §13.1 updated accordingly. **Implemented 2026-09-18** — `cmd/scheduler`, the `serviceaccount_reconciler`-backed due-list scan, the `CredentialService` cadence stamping, and the `internal/adapter/outbound/realmprovisioner` RP-17 client all landed; see TS-D14 and the TSQ-6 resolution below for the shipped shape. |
| — | 2026-09-18 → 2026-10-07 | **Living-document maintenance, no new rev.** Implementation-phase changes that touch no frozen name (§25) and no resolved question (§16) are recorded as §22 decisions TS-D13 to TS-D23, and every other section is kept consistent with them. The rows above stay as history. |

---

## Table of Contents

1. [Document Overview](#1-document-overview)
2. [Service Responsibilities and Boundaries](#2-service-responsibilities-and-boundaries)
3. [Architecture and Package Layout](#3-architecture-and-package-layout)
4. [Data Model](#4-data-model)
5. [API Contract](#5-api-contract)
6. [Credential Lifecycle and Secret Custody](#6-credential-lifecycle-and-secret-custody)
7. [Event Architecture](#7-event-architecture)
8. [Key Request Flows](#8-key-request-flows)
9. [Concurrency, Consistency, and Failure Handling](#9-concurrency-consistency-and-failure-handling)
10. [Security](#10-security)
11. [Observability](#11-observability)
12. [Configuration](#12-configuration)
13. [Deployment and Scaling](#13-deployment-and-scaling)
14. [Testing Strategy](#14-testing-strategy)
15. [GDPR, Data Lifecycle, and Compliance](#15-gdpr-data-lifecycle-and-compliance)
16. [Open Questions and Sign-off Register](#16-open-questions-and-sign-off-register)
17. [Appendix — Error Taxonomy](#17-appendix--error-taxonomy)
18. [Integration Details](#18-integration-details)
19. [Migration Strategy](#19-migration-strategy)
20. [Operational Considerations](#20-operational-considerations)
21. [Performance Considerations](#21-performance-considerations)
22. [Decision Register](#22-decision-register)
23. [Appendix — Glossary](#23-appendix--glossary)
24. [Appendix — Operational Runbooks](#24-appendix--operational-runbooks)
25. [Appendix — Name Inventory (proposed freeze)](#25-appendix--name-inventory-proposed-freeze)

---

## 1. Document Overview

The Token Service owns the **credential lifecycle** for non-human principals on the platform. At MVP its only principal is the **per-tenant platform automation principal** (HLD §5.8) — the identity that platform automation (LLM-drafted sections, connector-completed workflow steps) authenticates as, so that automated actions are attributable per tenant and flow through the standard authorization path rather than an anonymous in-cluster shared secret.

The Token Service does **not** create the principal's identity. The Realm Provisioner, as the sole Keycloak writer (RP-INV-1), mints the `platform-automation` `service_account`-typed Keycloak client per tenant (§2.5 of the RP LLD). The Token Service generates and rotates that client's **key material** (an RSA-2048 keypair per version since EXT-6, rev 1.3), holds the private key in OpenBao, serves the public halves as a JWKS that Keycloak's `client-jwt` authenticator fetches, records credential metadata in its own `serviceaccount` database, and emits credential-lifecycle events for audit. The private-key plaintext never touches this service's Postgres and never leaves the mesh.

### 1.1 Relationship to the HLD

| HLD section | This service |
|---|---|
| §5.8 Service Account & Token Service | Full service definition (credential half; RP owns the Keycloak-client half) |
| §11.7 Service Accounts and the Automation Principal | Credential issuance/rotation, OpenBao handling, internal-only posture |
| §11.2 Secrets Management | OpenBao KV as the secret store; Kubernetes-auth-bound policies |
| §11.3 Token Strategy | Service-account / integration credentials (MVP); opaque PATs (post-launch) |
| §9 Event Architecture | Publishes on `iam.serviceaccount.events`; consumes `TenantMembershipsPurged` |
| §11.5 Audit and Anomaly Triggers | Credential-lifecycle events feed the Audit Log; no secret in any record |
| §13 Disaster Recovery and Compliance | Metadata in Multi-AZ RDS; material in OpenBao HA; GDPR erasure on offboarding (§15) |
| §14 Capacity and Sizing | Trivially small — a few principals per tenant, writes only at provision/rotation (§21) |
| §15 Deployment and CI/CD | Shared-library and CI-gate conventions (§15.4); OpenBao via Kubernetes auth (§15.5.3) |
| §16 Build Roadmap | Pulled forward from Phase 2 into Phase 1.5 |
| §17.1 Resolved Decisions | ADR `0009-automation-principal.md` — brought forward, non-member, internal-only |

The base HLD is `IAM HLD v1.45`. Where a cross-reference below cites an HLD section, the section numbering follows the HLD's own scheme (§5 Service Inventory, §6 Role and Permission Model, §7 Data Architecture, §9 Event Architecture, §11 Security, §15 Deployment and CI/CD, §16 Build Roadmap, §17 Open Decisions).

---

## 2. Service Responsibilities and Boundaries

### 2.1 In scope

- **Credential lifecycle for the automation principal** — issue the initial credential, rotate it (with a bounded validity overlap), and revoke it. One live credential version per principal, plus at most one prior version inside the rotation-overlap window.
- **Service-account principal registry (metadata only)** — one row per tenant automation principal: its Keycloak `sub`, Keycloak `client_id`, type, status, and credential version pointers. **No secret is ever stored here** — only an OpenBao path reference.
- **Secret material custody** — generate an RSA-2048 keypair per credential version, write the PEM-encoded private key to **OpenBao** (KV v2) under a deterministic per-tenant, per-version path, return it **exactly once** to the trusted in-mesh TS-1 caller (who discards it), and serve the public half of every live version as a JWKS (§5.4) for Keycloak to fetch.
- **Credential-lifecycle events** — emit `ServiceAccountRegistered` / `…CredentialIssued` / `…CredentialRotated` / `…CredentialRevoked` / `ServiceAccountRevoked` on `iam.serviceaccount.events` via the transactional outbox, for the Audit Log.
- **Tenant-offboarding cleanup** — on `TenantMembershipsPurged`, delete every OpenBao entry under the tenant's prefix and hard-delete its registry rows (mirrors the Tender-ACL cascade, §10.1 of that LLD).
- **Keycloak key-cache refresh, triggered** — `cmd/rotator` and `cmd/scheduler` call the Realm Provisioner's RP-17 after every automatic revoke/rotate and keep a durable `keys_refresh_pending` marker until it succeeds (§8.8). This service still never writes Keycloak itself (TS-INV-1).
- **Own `serviceaccount` database** — `service_account_principals`, `service_account_credentials` (tenant-scoped, RLS), plus `outbox_events` / `processed_events` / `rls_violation_log` / `keys_refresh_pending` (operational, RLS-exempt).

### 2.2 Out of scope (owned elsewhere)

| Concern | Owner | Why not here |
|---|---|---|
| Minting/deleting the `platform-automation` Keycloak client; **refreshing** its Keycloak-side key cache | **Realm Provisioner** | RP is the sole Keycloak Admin API writer (RP-INV-1). This service generates/custodies key material and serves it as a JWKS; RP triggers Keycloak's cache refresh after each mint/rotate/revoke (RP-17, `ClearServiceAccountKeysCache`, corrected 2026-09-18/EXT-6 — see rev 1.3). |
| Human-user tokens / JWT issuance / login | **Keycloak** | This service issues no user tokens; it never sits on a human login path. |
| Authorization decisions for the principal | **AuthZ Enrichment** | The principal carries no roles; it is a non-member (O&M AUTH-9). This service issues credentials, never grants. |
| Making the principal a member or granting it a role | **Org & Membership** | Structurally barred (O&M AUTH-9 + membership FK). Not this service's concern. |
| Tenant-owned bots; general user PAT self-service | **Post-launch** | A future extension of the same tables (§2.4); not MVP surface. |

### 2.3 The credential-authority distinction (design note)

Two services touch the automation principal's credential and the split is deliberate: the **Token Service** is the credential *system-of-record* (it generates the material, versions it, custodies it in OpenBao, and tracks status), while the **Realm Provisioner** is the sole *writer* to Keycloak (it mints the `client-jwt` client and, after each rotate/revoke, clears Keycloak's cached keys with RP-17 so Keycloak re-fetches this service's JWKS; only RP may write Keycloak). Neither can complete a rotation alone — the same two-halves pattern the Realm Provisioner already uses with O&M for RP-9 (MFA reset) and RP-16 (session revocation). Keycloak remains the runtime **validator** of the signed `client_assertion` at the client-credentials token endpoint; this service is never on the hot token-issuance path. The lifecycle mechanics of this split are detailed in §6.

### 2.4 Post-launch extensions (documented, not built)

The `service_account_credentials` table and the lifecycle API are shaped so that two Phase-2 capabilities extend them without a redesign: **tenant-owned bots** (additional `service_account_principals` rows with `principal_type = 'tenant_bot'`, tenant-self-service-managed) and **user PATs** (opaque, Argon2id-hashed tokens where this service becomes the introspection authority, `principal_type = 'user_pat'`). MVP ships only `principal_type = 'platform_automation'`, one per tenant, and no self-service surface.

### 2.5 Core invariants

| # | Invariant |
|---|---|
| TS-INV-1 | **This service never writes Keycloak.** It has no gocloak dependency and no Keycloak Admin credential; RP-INV-1 is preserved. |
| TS-INV-2 | **No plaintext secret is ever persisted in Postgres or logged.** Secret material exists only in OpenBao and transiently in the TS-1 response body (mesh-mTLS, returned once). |
| TS-INV-3 | **One live credential version per principal.** At most one additional `rotating` version may be valid simultaneously, only within the bounded overlap window (§6.2/§8.2). |
| TS-INV-4 | The automation principal holds **no roles and no membership** (O&M AUTH-9); this service issues credentials only and makes no authorization decision. |
| TS-INV-5 | Every credential state transition (issue/rotate/revoke) is recorded locally **and** emitted on `iam.serviceaccount.events` through the outbox; audit visibility never depends on a live downstream call (§7). |
| TS-INV-6 | The service is reachable only in-mesh on `/api/v1/internal/*` under the reserved `iam-system` principal (the EXT-6 JWKS route excepted: it carries no identity headers and serves public keys only); it exposes no tenant-facing surface at MVP (§5.6). |
| TS-INV-7 | **Revocation is two-halves, like rotation.** A TS-2 revoke removes this service's custody/metadata of a version (the key drops out of this service's JWKS on the next fetch) but does not itself invalidate the key at Keycloak; enforcing that requires the paired Realm Provisioner action, `ClearServiceAccountKeysCache` (corrected 2026-09-18/EXT-6, was "remove/rotate the Keycloak client secret" — see rev 1.3). This paired action is required for **every** revoke path, including overlap-expiry (§8.3) — there is no Keycloak-side TTL that enforces it automatically — except offboarding, where realm deletion is itself the enforcement action. See §5.4/§8.7. |

---

## 3. Architecture and Package Layout

The service follows the platform Clean Architecture / Ports-and-Adapters layout (HLD §15.3): dependencies point inward, `core/domain` imports nothing external, and wiring lives only in the four composition roots — `cmd/server/main.go` (the internal HTTP API + outbox runner), `cmd/consumer/main.go` (the `TenantMembershipsPurged` offboarding consumer, §7.1), `cmd/rotator/main.go` (the overlap-expiry rotation-sweep CronJob, §8.3), and `cmd/scheduler/main.go` (the automatic cadence-driven rotation CronJob, §16 TSQ-6 Resolved, TS-D14). Inward-only dependency direction is enforced in CI by `.go-arch-lint.yml` / `.github/scripts/arch-lint.sh` (§3.2). All four binaries ship in one distroless image (`/iam-token-service-{server,consumer,rotator,scheduler}`; the server is the default `ENTRYPOINT`, the other workloads set `command`). The Helm migrate hook Job runs the server binary with `MIGRATE_ONLY=true` (§13.4).

**A fourth composition root, `cmd/scheduler`, implements the §16 TSQ-6 Resolved / TS-D14 automatic cadence-driven rotation** (landed 2026-09-18) — see the repo tree below. It drives cadence-based automatic rotation by calling TS-1 through the existing `service` layer (unlike `cmd/rotator`, which is deliberately walled off from `service`, §3.2) using a dedicated service identity, and ships in the same shared image.

```
iam-token-service/
├── cmd/
│   ├── server/                            # internal HTTP API composition root + outbox runner (HLD §15.3)
│   │   ├── main.go / wiring.go
│   │   ├── exporters.go                   # DB-state exporters: rotation_overlap_active, iam_rls_violations_total (TS-D18),
│   │   │                                  #   keys_refresh_pending / _oldest_age_seconds (TS-D23)
│   │   └── swagger_info.go                # swaggo @info metadata for the generated OpenAPI spec
│   ├── consumer/                          # SQS consumer composition root (TenantMembershipsPurged, §7.1)
│   │   ├── main.go
│   │   ├── inbound_schema.go              #   validateConsumed + buildSQSConsumer (GlueDecoder, §7.1)
│   │   └── dlq.go                         #   straight-to-DLQ router for permanent rejects (§7.1)
│   ├── rotator/                           # scheduled maintenance CronJob: overlap-expiry sweep (§8.3),
│   │   ├── main.go / helpers.go           #   orphan-material reconcile (§8.6), retention prunes (§3.3.3, §4.2)
│   │   ├── sweep.go / orphan_reconciler.go #   per-tenant sweep + inline RP-17 (§8.3); orphan reclaim under the principal lock (§8.6)
│   │   ├── keys_refresh.go                #   keys_refresh_pending retry pass, tenant batching, run-budget helpers (§8.8)
│   │   └── prune.go                       #   budgeted prunes: processed_events (inbox.Store.Prune), outbox_events, rls_violation_log (§8.9)
│   └── scheduler/                         # automatic cadence-driven rotation CronJob (§16 TSQ-6 Resolved, TS-D14)
│       ├── main.go                        #   composition root: wires CredentialService directly, unlike rotator
│       ├── scan.go                        #   runCadenceScan/scanTenant/rotateOneDue — due-list scan, intent marker, IssueOrRotate, inline RP-17
│       ├── keys_refresh.go                #   same retry pass/helpers as cmd/rotator's (kept as an identical copy)
│       └── helpers.go                     #   startJobSpan/withTenantGUC/logError (mirrors cmd/rotator's helpers.go)
├── internal/
│   ├── core/
│   │   ├── domain/                        # entities, value objects, domain errors
│   │   │   ├── principal.go               # ServiceAccountPrincipal, PrincipalType, PrincipalStatus
│   │   │   ├── credential.go              # Credential, CredentialStatus, version/overlap rules (§6.2)
│   │   │   ├── events.go                  # DomainEvent payload types (the 5 published events, §7.4)
│   │   │   └── errors.go                  # ErrPrincipalNotFound, ErrRotationInFlight, ErrPrincipalRevoked, ...
│   │   ├── port/                          # interfaces required by the core
│   │   │   ├── principal_repository.go
│   │   │   ├── credential_repository.go
│   │   │   ├── reconciler_repository.go   # BYPASSRLS cross-tenant enumeration for cmd/rotator + cmd/scheduler (RLS-7)
│   │   │   ├── secret_store.go            # OpenBao KV v2 put/get/delete of secret material (§6.3)
│   │   │   ├── realm_provisioner_client.go # RP-17 RefreshKeys (cmd/rotator, cmd/scheduler)
│   │   │   ├── event_publisher.go
│   │   │   ├── event_publisher_context.go # ctx-scoped publisher accessor (outbox inside the tx)
│   │   │   ├── inbox.go                   # Inbox{ProcessOnce} / InboxPruner — exactly-once consumer port over platform-events' pkg/inbox
│   │   │   ├── tx_runner.go               # TxRunner — runs fn inside a DB tx, binds RLS GUC + tx-bound EventPublisher into ctx
│   │   │   └── logger.go                  # logging port
│   │   └── service/                       # use cases
│   │       ├── credential_service.go      # IssueOrRotate (TS-1), Revoke (TS-2) — the credential lifecycle (§6)
│   │       ├── principal_service.go       # Register (TS-4), ReadPrincipal (TS-3), FindPrincipalBySub (TS-5), ReadPlatformAutomation (TS-6)
│   │       ├── jwks_service.go            # PublicKeys — the EXT-6 JWKS route (§5.4)
│   │       ├── secret_generator.go        # DefaultKeyGenerator — RSA-2048 keypair (EXT-6)
│   │       └── tracing.go                 # use-case spans (§11.3)
│   ├── adapter/
│   │   ├── inbound/
│   │   │   ├── http/                      # Gin handlers, DTOs, router, middleware, Swagger UI
│   │   │   │   ├── credential_handler.go  # TS-1 (issue/rotate), TS-2 (revoke)
│   │   │   │   ├── principal_handler.go   # TS-3 (read), TS-4 (register), TS-5, TS-6
│   │   │   │   ├── jwks_handler.go        # EXT-6 JWKS route (no header auth; global/per-tenant/unknown-tenant buckets, known-tenant refresher)
│   │   │   │   ├── router.go              # route registration (all under /api/v1/internal)
│   │   │   │   ├── middleware.go          # RequireIdentityHeaders, GUCBridgeMiddleware (system principal),
│   │   │   │   │                          #   RequireTenantPathMatch, RequireJSONContentType (§5.1)
│   │   │   │   ├── errors.go              # HandleError — domain/PgError → §17 wire shape
│   │   │   │   ├── asyncapi.go            # serves the embedded api/asyncapi.yaml
│   │   │   │   └── swagger_initializer.go / swagger_theme.go   # Swagger UI (gated, §12.1)
│   │   │   └── consumer/                  # SQS consumer — TenantMembershipsPurged (§7.1)
│   │   │       ├── offboarding_consumer.go # OffboardingConsumer{Handle}; New() wires the SQS consumer from eventcfg.SQSConfigEnv
│   │   │       └── dedup.go               # processOnce/ackUnknown — inbox claim + cascade in one transaction
│   │   └── outbound/
│   │       ├── postgres/                  # db.go (withPool, wrapConnErr), migrate.go, logger_adapter.go
│   │       │   ├── principal_repository.go / credential_repository.go / reconciler_repository.go
│   │       │   ├── inbox_repository.go    # InboxRepository — one platform-events inbox.Store per consumer (TS-D19)
│   │       │   ├── rls_violation_repository.go # RLSViolationRepository — CountSince/CursorBefore/Prune over rls_violation_log (TS-D18)
│   │       │   ├── keys_refresh_repository.go  # KeysRefreshRepository — MarkPending/MarkIntent/Clear (app pool), ListPending/PendingStats (reconciler pool)
│   │       │   ├── jwks_tenants_repository.go  # JWKSTenantsRepository — tenants with an active/rotating credential (reconciler pool, TS-D23)
│   │       │   └── migrations/            # 000001_schema (consolidated base), 000002_rotation_cadence, 000003_rls_violation_log, 000004_hardening, 000005_keys_refresh_pending (.up/.down.sql)
│   │       ├── openbao/                   # KV v2 secret store (OpenBao SDK; Kubernetes auth, §10.5)
│   │       ├── httpx/                     # shared outbound-HTTP transport (traceparent injection + client spans, §11.3)
│   │       ├── realmprovisioner/          # RP-17 relay client (§16 TSQ-6 Resolved) — this service's one outbound HTTP call to another IAM service
│   │       ├── eventbus/                  # SNS publisher + Glue codec + ValidatingCodec (enqueue-time JSON Schema check)
│   │       │   └── schemas/               # embedded JSON Schemas: the 5 events + TenantMembershipsPurged (consumed)
│   │       │       ├── service_account_registered.json / service_account_credential_issued.json
│   │       │       ├── service_account_credential_rotated.json / service_account_credential_revoked.json
│   │       │       ├── service_account_revoked.json
│   │       │       └── tenant_memberships_purged.json
│   │       └── metrics/                   # metrics.go (Tier 1/2/3 collectors, §11.2), library.go (InitLibraryMetrics), port decorators
│   │                                      #   (credentialservice.go, secretstore.go, realmprovisioner.go → platform_dependency_request_seconds)
├── pkg/
│   └── requestctx/                        # request-scoped tenant/user/trace context (exported package)
│       └── context.go
├── api/
│   ├── asyncapi.yaml                      # AsyncAPI 3.0 spec (iam.serviceaccount.events) — hand-authored source of truth (§7.3)
│   └── embed.go                           # go:embed of asyncapi.yaml (served by http/asyncapi.go)
├── docs/
│   ├── swagger/                           # OpenAPI 3 REST spec — GENERATED by `make swag` (swaggo), checked in;
│   │   ├── swagger.yaml / swagger.json / docs.go   #   CI `make swag-check` fails the build on any drift
│   ├── lld/iam-lld-token-service.md       # this document
│   ├── observability/                     # README.md (how the standard is implemented), metric-registry.md
│   │                                      #   (GENERATED by `make metrics-inventory`), runbooks.md (one per alert), migration.md
│   ├── observability-registry-proposals.md # Tier-1/Tier-2 registry submissions (§11.2)
│   └── architecture/                      # README.md + mermaid/
├── deploy/
│   ├── helm/                              # Chart.yaml, values.yaml, templates/ (§13.4):
│   │   └── templates/                     #   deployment-{server,consumer}, cronjob-{rotator,scheduler}, job-migrate (hook),
│   │                                      #   serviceaccount (per workload + migrate hook), secret, networkpolicy,
│   │                                      #   authorizationpolicy (+ PeerAuthentication), validate (render guards),
│   │                                      #   servicemonitor, podmonitor (CronJobs), prometheusrule, prometheusrule-slo,
│   │                                      #   pdb, hpa, httproute / ingress / securitypolicy, _env.tpl, _helpers.tpl
│   ├── openbao/                           # OpenBao policy.hcl + role.tf.example (Kubernetes auth, audience `openbao`, §10.5)
│   ├── iam/                               # IRSA policies: policy-server.json (SNS + Glue read), policy-consumer.json (SQS + DLQ send);
│   │                                      #   rotator/scheduler/migrate get none (§10.5)
│   └── monitoring/                        # app-alerts.yml, slo-rules.yml (§11.1), schema-registry-alerts.yml
├── test/                                  # unit/{service,consumer,metricsstandard}, postgres/ (testcontainers: RLS, repositories, consumer,
│                                          #   rls_violation_log, lock_timeout, migration_roundtrip, keys_refresh, jwks_tenants,
│                                          #   credential_concurrency, hardening_migration), integration/, e2e/, contract/, dbseed/ (§14)
├── scripts/                               # init-db.sql, init-floci.sh, merge_coverage.py
├── .github/                               # workflows (ci, validate-quality, validate-test, schema-registry, release, …) + scripts
│                                          #   (arch-lint, swag-check, no-gocloak, no-secret-log + its self-test test_check_no_secret_log.py,
│                                          #   set-local-only, gincommon-observability, metrics-taxonomy, metricslint.sh + metrics-inventory.py);
│                                          #   dependabot.yml
├── ARCHITECTURE.md  README.md  CHANGELOG.md  CONTRIBUTING.md
└── Dockerfile  docker-compose.yml  Makefile  go.mod  go.sum  .golangci.yml  .go-arch-lint.yml
```

As with the sibling services, the REST OpenAPI spec is **generated** by `swag` (swaggo) from Gin-handler doc-comment annotations into `docs/swagger/{swagger.yaml,swagger.json,docs.go}` via `make swag`; the generated artifacts are checked in and CI's `make swag-check` fails the build on any diff, so the committed spec cannot drift from the handler annotations. The AsyncAPI document (`api/asyncapi.yaml`) has no annotation-driven generator and is hand-authored as its own source of truth (§7.3). Every reference in this document to "the OpenAPI 3 contract" means the generated `docs/swagger/swagger.yaml`, not a hand-maintained `api/` file.

### 3.1 Shared library dependencies (HLD §15.4)

```
require (
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon           v1.6.0
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2              v2.1.0
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2            v2.0.0
    github.com/openbao/openbao/api/v2                              v2.x.x  // OpenBao KV v2 client (Vault-API-compatible)
    github.com/aws/aws-sdk-go-v2/service/glue                      v1.x.x  // Glue Schema Registry client
)
```

`iam-keycloakclient` is **deliberately not** a dependency — the Token Service never touches the Keycloak Admin API (TS-INV-1), and CI enforces that no `gocloak` import exists (§3.2). Gin middleware, logging, and tracing come from `platform-gincommon`; the pgx pool, RLS GUC injection, migrations, and pg-error helpers from `platform-pgcommon`; the outbox runner, SNS publisher, and SQS consumer from `platform-events`. The OpenBao SDK provides KV v2 read/write/delete of secret material (§6.3, §10.5); the AWS Glue SDK provides schema encode/decode for `iam.serviceaccount.events` payloads (§7.3.1). There is **no Valkey / cache dependency** — this service holds no read cache (§6 has no cache path; the only external state is OpenBao and Postgres).

### 3.2 Dependency rules (enforced in CI)

- `core/domain` imports nothing outside itself.
- `core/port` imports only `core/domain`.
- `core/service` imports only `core/domain` and `core/port`.
- `adapter/*` implements `core/port` and uses `core/domain` types; nothing in `core/` imports `adapter/`.
- Enforced by `go-arch-lint` in `ci.yml` (zero violations).

Three additional grep-based CI gates, unique to this service's invariants:

- **No-`gocloak` gate** (`.github/scripts/check-no-gocloak.sh`) — fails the build if any `gocloak`/Keycloak-Admin import appears anywhere in the module. This is the inverse of the Realm Provisioner's gate (RP *requires* `gocloak`; this service *forbids* it), and makes TS-INV-1 structural rather than conventional.
- **`SET LOCAL`-only RLS gate** (RLS-6, §4.3) — forbids any session-scoped `SET app.tenant_id`; the GUC must be transaction-local.
- **Secret-logging gate** (`.github/scripts/check-no-secret-log.sh` → `check-no-secret-log.py`) — fails the build if a credential/secret field name, a PEM/private-key name or a `-----BEGIN` literal reaches a log (`port.Logger`/Zap or `slog` call shape), an `fmt` sink, an error constructor or a span attribute (TS-INV-2, §11.4; widened in TS-D22). The wrapper first runs the checker's own self-test (`test_check_no_secret_log.py`) and fails if the checker no longer catches its known-bad cases.

`make gates` also runs two observability gates shared with the siblings, and `make metrics-lint` a third check (all in `make ci` and `validate-quality.yml`):

- **gincommon-observability gate** (`.github/scripts/check-gincommon-observability.sh`) — logs, metrics and traces enter the process only through `platform-gincommon`: it rejects `slog`/stdlib `log`/direct Zap, `prometheus.DefaultRegisterer`/`promauto`, a bare `promhttp.Handler()`, `InitTracingFromEnv()` and a hand-rolled `TracerProvider`.
- **metrics-taxonomy gate** (`.github/scripts/check-metrics-taxonomy.py`) — three-tier prefix classification and `_total`/`_seconds` suffixes (§11.2).
- **metrics-lint** (`.github/scripts/metricslint.sh`) — `platform-gincommon`'s `metricslint` over a real `/metrics` scrape (written by `test/unit/metricsstandard`) and over the metric references in `deploy/` and `docs/`, plus a drift check of the generated `docs/observability/metric-registry.md` (`.github/scripts/metrics-inventory.py`; regenerate with `make metrics-inventory`).

### 3.3 Shared library integration scope

This section specifies exactly how the three platform libraries are integrated — which exported packages are used, where they are wired, and which features are in or out of scope. All three are private Go modules under `github.com/BCBP-SOLUTIONS-FZC-LLC/` consumed via the versions pinned in §3.1; none is deployed on its own.

Integration touch-point map:

```mermaid
flowchart LR
    subgraph svc["iam-token-service"]
        MAIN["cmd/server/main.go<br/>composition root"]
        HTTP["adapter/inbound/http<br/>Gin handlers (TS-1..TS-6, JWKS)"]
        CONS["adapter/inbound/consumer<br/>SQS (TenantMembershipsPurged)"]
        PG["adapter/outbound/postgres<br/>repositories"]
        OB["adapter/outbound/openbao<br/>KV v2 secret store"]
        EB["adapter/outbound/eventbus<br/>publisher + outbox"]
    end

    GIN["platform-gincommon"]
    PGC["platform-pgcommon"]
    EVT["platform-events"]
    BAO["OpenBao (K8s auth)"]

    MAIN -->|NewLogger, ObservabilityMiddlewares,<br/>HealthHandler, MetricsHandler,<br/>InitTracingWithConfig, NewSpanTracer, Shutdown| GIN
    HTTP -->|ProtectedMiddlewares, RequestContext,<br/>header constants| GIN
    MAIN -->|NewPool, migrate.Runner,<br/>pgmetrics.InitWithIdentity| PGC
    PG -->|RunInTx, WithGUCSet,<br/>pg-error helpers| PGC
    HTTP -->|GUCSet bridge,<br/>GUCSetFromContext| PGC
    MAIN -->|NewSNSPublisher,<br/>outbox.ApplySchema, outbox.NewRunner| EVT
    EB -->|events.NewEnvelope,<br/>outbox.Enqueue| EVT
    CONS -->|NewSQSConsumerWithClient<br/>TenantMembershipsPurged| EVT
    PG -->|inbox.NewStore,<br/>Store.Process / Store.Prune| EVT
    OB -->|KV v2 put/get/delete<br/>Kubernetes auth login| BAO
```

#### 3.3.1 `platform-gincommon` — HTTP middleware, logging, tracing

The Token Service is a **Gin HTTP service only** (internal API) — it exposes no gRPC server. Only the `gincommon` and `logger` packages are used; `grpccommon` is out of scope.

| Symbol | Where | Use in Token Service |
|---|---|---|
| `logger.NewLogger(env)` | `main.go` | Builds the structured `port.Logger` injected into the pool, middleware config, outbox runner, and OpenBao client |
| `gincommon.Config{Logger, ServiceName, BuildVersion, Domain, Environment, RequestTimeout}` | `main.go` | One config feeding all middleware stacks; `ServiceName = "iam-token-service"` (required — `ObservabilityMiddlewares` panics on empty), `Domain = "iam"` (`OBSERVABILITY_DOMAIN` overrides; required — panics when missing), `Environment = APP_ENV` (must be `local`/`dev`/`test`/`staging`/`prod` — panics on e.g. `production`). `RequestTimeout = 30s` (gincommon appends `TimeoutMiddleware` as the innermost observability handler, so metrics/access log/span record the same 503 the client gets). Tracing is initialised with the same identity via `InitTracingWithConfig` |
| `gincommon.ObservabilityMiddlewares(cfg)` | engine (every route) | Panic recovery, request id, tracing, correlation headers, metrics, access log, request timeout (order below) |
| `gincommon.ProtectedMiddlewares(cfg)` | `/api/v1/internal` group | `RequireAuth → ContextMiddleware`, wrapped by this service's own middleware (order below). Not on the JWKS route |
| `gincommon.HealthHandler()` | `/healthz` | Liveness; registered before auth |
| `gincommon.RequestContext(c)` | `GUCBridgeMiddleware` | Retrieves the platform request context (user, tenant); the service then stores its own `requestctx.RequestContext` for handlers |
| `gincommon.HeaderUserID` / `HeaderTenantID` / `HeaderTenantRoles` | `RequireIdentityHeaders` | Header names; `x-tenant-roles` is deleted before gincommon sees it (TS-D23) |
| `gincommon.InitTracingWithConfig(TracingConfig{ServiceName, BuildVersion, Domain, Environment, Logger})` / `gincommon.Shutdown(logger)` | `main.go` (every binary) | OTLP tracer init with the same identity as metrics (`service.namespace`, `deployment.environment.name` on the resource) + graceful flush on SIGTERM; call after the HTTP server stops, before `pool.Close()`. `OTEL_EXPORTER_OTLP_ENDPOINT=none` / `OTEL_TRACES_EXPORTER=none` disables export (v1.6.0) |
| `gincommon.NewTracer(scope)` / `gincommon.NewSpanTracer(scope)` / `gincommon.SpanTraceID(ctx)` | `cmd/*`, consumer adapter | Job/consumer spans, `pgcommon.Config.Tracer` for `db.query` spans, and `trace_id` log correlation — no direct OpenTelemetry import |
| `gincommon.MetricsHandler()` | `/metrics` (every binary) | Serves `MetricsRegisterer`'s registry with `ContinueOnError` — one failing collector does not blank the scrape |
| `gincommon.MetricsRegisterer()` | `main.go` | Returns the `prometheus.Registerer` that middleware HTTP metrics register against; passed to `metrics.Register(...)` so `iam_token_service_*` business counters land in the same registry |

**Exact middleware order** (from the library):

```
MaxBytesReader(1 MiB)                                                          (service, engine-wide)
  → PanicRecovery → RequestID → Tracing → CorrelationHeaders → Metrics → Logging → Timeout   (Observability, engine-wide)
  /api/v1/internal/* only:
  → RequireIdentityHeaders → RequireAuth → ContextMiddleware                   (service guard, then gincommon Protected)
  → GUCBridgeMiddleware → RequireJSONContentType → RequireTenantPathMatch       (service)
```

`RequireIdentityHeaders` answers `401 missing_identity_headers` itself when `x-user-id` or `x-tenant-id` is missing, repeated or not a UUID, and deletes `x-tenant-roles` (unused here) so gincommon cannot reject a request on it. Anything that passes it also passes gincommon's `RequireAuth`, so the client always sees this service's §17 code. `GUCBridgeMiddleware` then accepts **only** the reserved system principal (`…00a1`, "iam-system") as `x-user-id` (any other UUID is the same `401`), stores the request context and binds the GUC set (§3.3.2). `RequireJSONContentType` rejects a POST body without `application/json` (`415`); `RequireTenantPathMatch` rejects a path `:id` that differs from `x-tenant-id` (`403 tenant_path_mismatch`). The JWKS route is registered outside this group (§5.4). The service performs **no JWT validation**: it trusts mesh headers, which is safe only because the Istio `AuthorizationPolicy` restricts which workloads may reach which route (§10.2). The `RequirePermission` / RBAC helpers are deliberately not used: this service makes no per-user authorization decision (TS-INV-4).

#### 3.3.2 `platform-pgcommon` — pool, RLS GUC injection, transactions, errors

The entire data-access substrate and the enforcement point for Layer-2 tenant isolation (§10.1).

| Symbol | Where | Use in Token Service |
|---|---|---|
| `pgcommon.NewPool(ctx, Config{...})` | `main.go` | The single `*pgcommon.Pool`, injected into all repositories and the outbox runner; pings on creation → fails fast on bad DSN |
| `pgcommon.Config.PGBouncerMode` (env-driven) | `main.go` | From `PG_BOUNCER_MODE`; **`false`** in dev/test (direct Postgres), **`true`** in production/staging (SimpleProtocol + transaction-local GUC injection) |
| `pgcommon.Config.GUCProvider = pgcommon.GUCSetFromContext` | `main.go` | Wires the pool to pull the per-request `GUCSet` from context on every acquire |
| `pgcommon.WithGUCSet(ctx, GUCSet{UserID, TenantID})` | `/api` GUC-bridge middleware | Stores the system-principal + target-tenant identity for pool-time RLS injection; identity is already validated upstream so `WithGUCSet` (not `WithValidatedGUCSet`) is used |
| `pgcommon.RunInTx(ctx, pool, opts, fn)` | repositories / services | Every write; injects `app.user_id` / `app.tenant_id` as `SET LOCAL` (txn-local) and commits the credential-state row + outbox row atomically |
| `pgcommon.RunInTxWithRetryOpts` | `postgres.TxRunner.RunInTx` (every service write) | `40001`/`40P01` retry with backoff+jitter (`writeRetryOpts`); each closure re-reads its rows, so a retry never reuses state a failed attempt mutated (§9.1) |
| `pgcommon.IsUniqueViolation` / `IsForeignKeyViolation` / `IsCheckViolation` / `IsDeadlock` / `IsSerializationFailure` / `ConstraintName` | error mapping | Maps pg `SQLSTATE` → HTTP codes (§17) — never raw string matching. `uq_sac_one_active` violation → `409`; `uq_sac_version` → idempotent-retry path (§9.2) |
| `pgcommon.Pool.Health(ctx)` | `/readyz` | Pool liveness; each `/readyz` dependency check runs under a 2 s deadline (`readinessCheckTimeout`, below the probe's 3 s `timeoutSeconds`) and fails at once while the pod is draining (TS-D21/TS-D23) |
| `pgcommon.Pool.DrainAndClose(ctx)` | `main.go` shutdown | Graceful shutdown of the app and reconciler pools, called after `/readyz` has failed for `SHUTDOWN_DRAIN_DELAY`, the listeners have closed and the outbox runner / consumer has stopped (§13.4) |
| `pgcommon.ConfigFromEnv() (Config, []ConfigWarning)` | `main.go` | Builds `Config` from `DATABASE_URL`/`PG_*`; structured warnings logged at startup, not silently discarded |
| `migrate.Runner{DSN, Logger}.Up(ctx)` | `pgadapter.Migrate` | Runs `postgres/migrations/`, appending `lock_timeout=30s`. In Kubernetes only the Helm hook Job runs it (`MIGRATE_ONLY=true`); workloads start with `RUN_MIGRATIONS=false` (§4.4, §13.4) |
| `pgmetrics.InitWithIdentity(pgmetrics.IdentityFromLabels(gincommon.MetricsConstLabels()), gincommon.MetricsRegisterer())` | `metrics.InitLibraryMetrics` (every `main.go`) | Registers the `platform_db_*` collectors with the service identity before `NewPool` (which then registers each pool's connection gauges); returns `RegistrationWarning`s (logged) and fails startup on an invalid identity |
| `pgcommon.Config.Tracer = gincommon.NewSpanTracer(serviceName)` | every `main.go` (app + reconciler pools) | Per-query `db.query` OTel spans on the TracerProvider `InitTracingWithConfig` installed; no hand-rolled pgx tracer |
| `pgcommon.Config` `StatementTimeout` / `LockTimeout` (`PG_STATEMENT_TIMEOUT` / `PG_LOCK_TIMEOUT`) | `ConfigFromEnv`, copied into `SystemPoolConfig` | Applied per transaction (`SET LOCAL`) on the app and reconciler pools; never to the migration DSN (TS-D20) |

**The GUC names are fixed by the library**: `app.user_id`, `app.tenant_id`. Under `PGBouncerMode=true` they are set with `set_config(..., true)` (transaction-local) at the start of each `RunInTx`/`WithConn`, so they clear at commit and cannot leak across the pooled backend — the mechanism the §4.3 RLS policies depend on (RLS-6). `app.tenant_roles` is **not** used — this service makes no role-based decision (TS-INV-4), so only `app.user_id` (the system principal) and `app.tenant_id` (the target tenant) are injected. **Pool sizing:** `MinConns: 0` under PgBouncer transaction pooling; `MaxConns: 10` per pod (this service is trivially small, §21). Under `PGBouncerMode=true`, migrations bypass PgBouncer (`MIGRATION_DATABASE_URL` on the direct 5432 port; advisory locks are session-scoped, §4.4).

#### 3.3.3 `platform-events` — outbox, SNS publisher, SQS consumer

The Token Service is a **single-topic producer** (`iam.serviceaccount.events`) **and** a **single-subscription consumer** (`TenantMembershipsPurged`), so it wires the full outbox + SNS-publisher + SQS-consumer surface.

| Symbol | Where | Use in Token Service |
|---|---|---|
| `outbox.ApplySchema(ctx, runner)` | `pgadapter.Migrate` (the migrate hook Job) | Creates `outbox_events` / `outbox_dead_letters` via the library's embedded migrations — **run before the domain migrations reference them** (MIG-2, §4.4). The table is library-owned; this service only grants its app role on it + customizes the payload column (§4.2) |
| `events.NewEnvelope(type, source, payload, opts...)` | `eventbus/publisher.go` | Builds the CloudEvents-aligned envelope; `source = "iam-token-service"`, `ID` is UUID v7 (dedup key). Options: `WithTenantID`, `WithTraceID` |
| `outbox.Enqueue(ctx, tx, env)` | inside `RunInTx` | Writes the event row in the **same** `pgx.Tx` as the credential-state change (no dual-write, EVT-1). Payload is plain, schema-validated JSON (Glue encoding happens at publish time, §7.3.1) |
| `events.NewSNSPublisher(SNSConfig{TopicARN, Region}, opts...)` | `main.go` | Single SNS publisher → `iam.serviceaccount.events`; **no RoutingPublisher** (one topic). `events.WithCodec(glueCodec)` sets Glue wire-format encoding at publish time |
| `outbox.NewRunner(Config{Pool, Publisher, PollInterval:500ms, BatchSize:50, MaxAttempts:5, DrainTimeout:30s, ...})` | `main.go` | Background publisher; `Stop()` drained before pool close (LIFO defers). All tunables `OUTBOX_*`-env-configurable (§12) |
| `outbox.Runner.PrunePublished(ctx, olderThan, limit)` | `cmd/rotator` | Deletes successfully-published rows older than `OUTBOX_PRUNE_OLDER_THAN` (default 168h), `PRUNE_BATCH_LIMIT` per call, on every rotator run, so `outbox_events` does not grow unbounded |
| `events.NewSQSConsumerWithClient(eventcfg.SQSConfigFromEnv(env, log), client, handler, eventcfg.SQSConsumerOptions(env)..., events.WithConsumerCodec(GlueDecoder{}))` | `cmd/consumer/inbound_schema.go` | **One active subscription: `TenantMembershipsPurged`** on `iam.tenant.events` (queue `tenant-lifecycle-tokensvc-q`, §7.1), dispatched to `OffboardingConsumer.Handle` (§8.4). `SQS_*` options from `eventcfg.LoadSQS` (concurrency, visibility, handler/drain timeouts); `SQS_QUEUE_DEPTH_INTERVAL` (default 60s, `0s` disables) enables the library's `platform_queue_depth` / `platform_dlq_depth` sampler (§11.5) |
| `inbox.NewStore(pool, consumer)` / `Store.Process(ctx, env, fn)` / `Store.Prune(ctx, retention, batch)` | `postgres.InboxRepository` (consumer), `cmd/rotator` (prune) | Exactly-once offboarding over `processed_events` (TS-D19): `Process` claims the envelope id as the first statement of one RLS-scoped transaction and runs the cascade inside it — a duplicate (including a concurrent copy blocked on the claim's row lock) is a no-op, any error rolls the claim back; it counts `platform_duplicate_messages_total`. `Prune` deletes rows older than `PROCESSED_EVENTS_TTL_DAYS`, batched until none remain |
| `events.InitMetrics(identity, gincommon.MetricsRegisterer())` / OTel | `metrics.InitLibraryMetrics` | Publish/consume/outbox Prometheus (`platform_*`, identity from `gincommon.MetricsConstLabels`) + tracing from the library; `platform_dlq_messages_total{operation="outbox_publish",reason="max_attempts"}` (alert on newly dead-lettered events, §11.5) |

**HMAC signing/verification is out of scope** — this service is reached only as authenticated in-mesh HTTP (Realm Provisioner, operators) and SNS→SQS transport, never a raw external webhook. The `Envelope` is published with its UUID v7 `ID` as the dedup key so the Audit consumer stays idempotent (HLD §9.3). Retryable AWS errors (`ThrottlingException`, `ServiceUnavailable`, …) are reschedule-without-attempt-advance (library v1.1.0), so transient SNS throttling never dead-letters a credential-lifecycle event.

---

## 4. Data Model

Database: `serviceaccount` on the shared RDS PostgreSQL Multi-AZ instance (HLD §7.1). Fronted by PgBouncer in transaction-pooling mode. The pool is created with `pgcommon.NewPool(... PGBouncerMode: <PG_BOUNCER_MODE>, GUCProvider: pgcommon.GUCSetFromContext, MinConns: 0, MaxConns: 10 ...)` — `PGBouncerMode` is env-driven (`true` in production/staging where PgBouncer fronts the DB, `false` in dev/test; §3.3.2); `MinConns: 0` applies under PgBouncer transaction pooling (direct-Postgres mode defaults it to `2`); `MaxConns: 10` per pod is ample for this service's trivial write volume (§21). The `pgcrypto` extension is required for `gen_random_uuid()`.

The schema extends the HLD §5.8/§7.4 baseline for non-human principals. Unlike User Profile, this service stores **no human PII and no secret material** — only a service-account principal registry and credential *metadata* (an OpenBao path reference, never the secret).

**Entity-relationship overview.** `service_account_principals` is the hub (one row per tenant automation principal); its own primary key is `id`, and it carries the composite unique key `(id, tenant_id)` that the child FK targets. `service_account_credentials` hangs off it via a **composite `(principal_id, tenant_id)` foreign key** referencing `service_account_principals(id, tenant_id)` — not `principal_id` alone — so the DB itself prevents a credential row from referencing a principal in a different tenant; the FK carries `ON DELETE CASCADE`. The relationship is 1:N (a principal accrues one credential row per version; §6.2), constrained so at most one is `active` at a time (`uq_sac_one_active`, TS-INV-3). `openbao_path` is a *reference* to material in OpenBao — an external system of record, shown dashed — **not** a secret column and not a DB FK. `outbox_events`, `processed_events`, `rls_violation_log` (migration `000003`, TS-D18), `keys_refresh_pending` (migration `000005`, TS-D22/TS-D23) and `schema_migrations` are standalone operational tables (not tenant-scoped, no FK). `keys_refresh_pending.tenant_id` is a plain key, not an FK: a marker may outlive its tenant's offboarding. Mermaid has no enum type, so status columns appear as plain text.

```mermaid
erDiagram
    SERVICE_ACCOUNT_PRINCIPALS ||--o{ SERVICE_ACCOUNT_CREDENTIALS : "has versions (1:N, ≤1 active)"
    SERVICE_ACCOUNT_CREDENTIALS ||..o| OPENBAO : "openbao_path → material (soft ref, not FK)"

    SERVICE_ACCOUNT_PRINCIPALS {
        uuid id PK "service-generated; FK target with tenant_id"
        uuid tenant_id "RLS scope, NOT NULL"
        uuid principal_sub "= keycloak service-account sub (from RP, TS-4)"
        text keycloak_client_id "'platform-automation' or tenant-scoped, MVP (§25, rev 1.1)"
        principal_type principal_type "ENUM: platform_automation (+tenant_bot,user_pat post-launch)"
        principal_status status "ENUM: active|revoked"
        integer record_version "optimistic lock, CHECK > 0"
        timestamptz created_at
        timestamptz updated_at
        timestamptz deleted_at
    }

    SERVICE_ACCOUNT_CREDENTIALS {
        uuid id PK
        uuid principal_id "FK (principal_id, tenant_id) -> principals"
        uuid tenant_id "FK anchor, RLS scope, NOT NULL"
        integer version "monotonic per principal; UNIQUE (principal_id, version)"
        credential_status status "ENUM: active|rotating|revoked (≤1 active)"
        text openbao_path "deterministic KV v2 path; NOT the secret (§6.3)"
        uuid granted_by "x-user-id of the actor (iam-system on cron/RP paths)"
        uuid rotation_id "caller idempotency key for the issue/rotate (§9.2); NULL on offboarding-revoke"
        integer record_version "optimistic lock, CHECK > 0"
        timestamptz issued_at
        timestamptz updated_at
        timestamptz rotated_at
        timestamptz expires_at "rotation-overlap cutoff for a 'rotating' row (§6.2)"
        timestamptz revoked_at
        timestamptz deleted_at
    }

    PROCESSED_EVENTS {
        text event_id PK "composite PK — also accepts external broker message IDs"
        text consumer PK "composite PK with event_id ('tenant_offboarding')"
        timestamptz processed_at
    }

    RLS_VIOLATION_LOG {
        bigserial id PK "exporter cursor (TS-D18)"
        text table_name
        uuid row_tenant_id
        uuid app_tenant_id
        text violation_type "missing_or_invalid_guc|cross_tenant_access"
        text session_role
        text application_name
        text query_text
        timestamptz occurred_at
    }

    KEYS_REFRESH_PENDING {
        uuid tenant_id PK "one owed RP-17 refresh per tenant (TS-D22)"
        timestamptz requested_at "only moves forward; Clear(upTo) compares it"
        timestamptz intent_until "NULL = committed marker; set = scheduler intent (TS-D23)"
    }
```

### 4.1 Extensions and enums

```sql
CREATE EXTENSION IF NOT EXISTS pgcrypto;   -- gen_random_uuid()

CREATE TYPE principal_type    AS ENUM ('platform_automation');            -- MVP; 'tenant_bot','user_pat' added post-launch (§2.4)
CREATE TYPE principal_status  AS ENUM ('active', 'revoked');
CREATE TYPE credential_status AS ENUM ('active', 'rotating', 'revoked');
```

Enums are used (rather than `CHECK` constraints) for the same reasons the siblings do: stricter, faster on index scans, type-level enforcement. Adding a post-launch `principal_type` value is an additive `ALTER TYPE principal_type ADD VALUE 'tenant_bot'` (no full table rewrite in PostgreSQL ≥ 12, §4.4).

### 4.2 Tables

#### `service_account_principals`

```sql
CREATE TABLE service_account_principals (
  id                  uuid NOT NULL DEFAULT gen_random_uuid(),
  tenant_id           uuid NOT NULL,
  principal_sub       uuid NOT NULL,                 -- the Keycloak service-account client's user sub (from RP, TS-4)
  keycloak_client_id  text NOT NULL,                 -- 'platform-automation' or tenant-scoped, MVP (§25, rev 1.1)
  principal_type      principal_type NOT NULL DEFAULT 'platform_automation',
  status              principal_status NOT NULL DEFAULT 'active',
  record_version      integer NOT NULL DEFAULT 1 CHECK (record_version > 0),  -- optimistic-lock token; bumped by trigger (§4.5)
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  deleted_at          timestamptz,
  CONSTRAINT service_account_principals_pkey PRIMARY KEY (id),
  CONSTRAINT uq_sap_id_tenant               UNIQUE (id, tenant_id),          -- FK target for the composite child FK
  CONSTRAINT uq_sap_active_principal        UNIQUE (tenant_id, principal_type) -- one automation principal per tenant
);

-- Indexes
CREATE INDEX idx_sap_tenant        ON service_account_principals (tenant_id);
CREATE INDEX idx_sap_sub           ON service_account_principals (tenant_id, principal_sub);   -- reconcile-by-sub (RP-18)
CREATE INDEX idx_sap_status        ON service_account_principals (tenant_id, status) WHERE deleted_at IS NULL;
```

Notes:

- **`principal_sub` is supplied by the Realm Provisioner (TS-4), never generated here.** The Token Service is a *projection* of the RP-minted Keycloak service-account identity, the same way User Profile projects the human Keycloak `sub`. This service never mints an identity (TS-INV-1).
- **`id` is service-generated** (a surrogate key, `gen_random_uuid()`) — unlike User Profile, whose PK *is* the Keycloak `sub`. Here the Keycloak identity is carried in `principal_sub`, and `id` is the stable local handle the credential rows and TS-3/TS-4 paths key on. The composite `uq_sap_id_tenant` exists solely so the child table's `(principal_id, tenant_id)` FK has a unique target that also structurally scopes it to one tenant.
- **One `platform_automation` principal per tenant** (`uq_sap_active_principal` on `(tenant_id, principal_type)`). Post-launch `tenant_bot`/`user_pat` principals (§2.4) are additional rows under the same tenant with a different `principal_type`, so the per-tenant-per-type uniqueness composes without a schema change.
- **`keycloak_client_id`** is either the frozen base name `platform-automation` (a dedicated-realm tenant) or that base name suffixed with the owning tenant's UUID (a shared-realm/trial tenant — a shared realm cannot hold two clients with the same `clientId`), validated by `domain.ValidPlatformAutomationClientID` (§5.1/§10.3, rev 1.1); it is stored (rather than assumed) so the OpenBao path (§6.3) and any future multi-client principal derive from a real column, not a constant.
- **`status` lifecycle**: `active` → `revoked` (terminal at offboarding, §8.4). A `revoked` principal rejects new issue/rotate (`422 principal_revoked`, §5.5). There is no `disabled` state — the automation principal is either live or gone.
- **`record_version`** is the optimistic-concurrency token used by §9.1 (a monotonic counter, not `updated_at` — timestamps collide under sub-millisecond concurrent writes); it and `updated_at` are maintained by the §4.5 trigger.

#### `service_account_credentials`

```sql
CREATE TABLE service_account_credentials (
  id             uuid NOT NULL DEFAULT gen_random_uuid(),
  tenant_id      uuid NOT NULL,
  principal_id   uuid NOT NULL,
  version        integer NOT NULL,                    -- monotonically increasing per principal (§6.2)
  status         credential_status NOT NULL DEFAULT 'active',
  openbao_path   text NOT NULL,                       -- deterministic KV v2 path; NOT the secret (§6.3, §10.5)
  granted_by     uuid NOT NULL,                       -- x-user-id of the actor (iam-system on cron/RP paths)
  rotation_id    uuid,                                -- the caller's idempotency key for the issue/rotate that created this row (§9.2); NULL for the offboarding-revoke path
  record_version integer NOT NULL DEFAULT 1 CHECK (record_version > 0),
  issued_at      timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),   -- last status transition (rotate/sweep/revoke); maintained by the trigger (§4.5)
  rotated_at     timestamptz,
  expires_at     timestamptz,                          -- nullable; rotation-overlap cutoff for a 'rotating' row (§6.2)
  revoked_at     timestamptz,
  rotation_cadence_days integer,                        -- cadence in effect when this 'active' row was issued/rotated (TS-D10, default 90); NULL once superseded (§16 TSQ-6 Resolved)
  next_rotation_at      timestamptz,                     -- when this 'active' row is next due for cmd/scheduler's automatic rotation (TS-D14); NULL once superseded (rotating/revoked) or for pre-migration rows
  deleted_at     timestamptz,
  CONSTRAINT service_account_credentials_pkey PRIMARY KEY (id),
  CONSTRAINT fk_sac_principal FOREIGN KEY (principal_id, tenant_id)
      REFERENCES service_account_principals (id, tenant_id) ON DELETE CASCADE,
  CONSTRAINT uq_sac_version   UNIQUE (principal_id, version)
);

-- Indexes
CREATE INDEX idx_sac_tenant    ON service_account_credentials (tenant_id);
CREATE INDEX idx_sac_principal ON service_account_credentials (principal_id, version DESC);  -- TS-3 metadata read (§8.5)
CREATE INDEX idx_sac_overlap   ON service_account_credentials (expires_at)
                                  WHERE status = 'rotating' AND expires_at IS NOT NULL;       -- the §8.3 overlap-expiry sweep scans exactly this
CREATE UNIQUE INDEX uq_sac_one_active
  ON service_account_credentials (principal_id) WHERE status = 'active' AND deleted_at IS NULL;
CREATE UNIQUE INDEX uq_sac_rotation_id
  ON service_account_credentials (principal_id, rotation_id) WHERE rotation_id IS NOT NULL;   -- TS-1 idempotency (§9.2)
CREATE INDEX idx_sac_next_rotation ON service_account_credentials (next_rotation_at)
  WHERE status = 'active' AND next_rotation_at IS NOT NULL AND deleted_at IS NULL;             -- cmd/scheduler's due-list scan (TS-D14, §16 TSQ-6 Resolved)
```

Notes:

- **No secret column — this is the crux (TS-INV-2).** `openbao_path` references the material in OpenBao (§6.3/§10.5); the plaintext exists only in OpenBao and transiently in the one-time TS-1 response. A `text` column holding a secret would violate the core invariant and be caught by the secret-logging CI gate's sibling review.
- **`uq_sac_one_active`** (partial unique index) enforces at most one `active` credential per principal (TS-INV-3, CONC-2). A `rotating` prior version bounded by `expires_at` may coexist during the overlap window (§6.2) — it is not `active`, so it does not conflict with the partial index.
- **`uq_sac_rotation_id`** (partial unique index) makes TS-1 idempotent per `rotation_id`: a replayed issue/rotate observes the existing row for that `(principal_id, rotation_id)` and returns the same version **without generating new material** (§9.2) — a re-generation would orphan an OpenBao entry and double-emit.
- **`version`** is monotonically increasing per principal (`uq_sac_version`), never reused; the offboarding cascade hard-deletes the rows rather than recycling versions.
- **`expires_at`** is set only on a `rotating` row (the overlap cutoff); `idx_sac_overlap` is the narrow partial index the §8.3 sweep scans, so the sweep never reads `active`/`revoked` rows.
- **`granted_by`** records the actor for audit attribution — the reserved `iam-system` principal on the cron/RP paths, an operator id on an operator-initiated rotation (§5.2).
- **`rotation_cadence_days`/`next_rotation_at`** (§16 TSQ-6 Resolved) are set on the `active` row at issue/rotate time (default `rotation_cadence_days` = 90, TS-D10) and cleared back to `NULL` the instant that row is demoted to `rotating` or revoked — only the current `active` version is ever "due". `idx_sac_next_rotation` is the partial index `cmd/scheduler`'s due-list scan reads (TS-D14); TS-3 also surfaces both columns read-only (§5.4). Not backfilled for pre-migration rows.

#### Operational tables (RLS-exempt)

`outbox_events` — **the schema is created and owned by `platform-events`, not by this service's migrations.** `outbox.ApplySchema` runs first in `pgadapter.Migrate` (the migrate hook Job, §4.4) and creates the table (and `outbox_dead_letters`) with the library's own column shape; this service's migration only (a) `GRANT`s the `serviceaccount_app` role `SELECT/INSERT/UPDATE/DELETE` on it (guarded by `IF EXISTS`, since the table is created out-of-band) and (b) applies the same customization the sibling services apply — `ALTER TABLE outbox_events ALTER COLUMN payload TYPE text` plus a `BEFORE INSERT` normalize-payload trigger, working around pgx encoding `[]byte` as bytea-hex under PgBouncer's SimpleProtocol mode. `outbox.Enqueue` writes rows inside the state-change transaction and `outbox.Runner` publishes/marks/prunes them (§7.4). `processed_events` (composite PK `(event_id, consumer)`, 8-day retention) is **service-owned DDL**, as is `schema_migrations` (migrate bookkeeping):

```sql
CREATE TABLE processed_events (
  event_id     text NOT NULL,                     -- TEXT, not uuid: dedupes replayed SQS/SNS deliveries whose broker message IDs are external strings
  consumer     text NOT NULL,                     -- 'tenant_offboarding'
  processed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (event_id, consumer)                -- composite; the table's sole job is idempotency (§7.6)
);
CREATE INDEX processed_events_processed_at_idx ON processed_events (processed_at);  -- prunes the retention sweep's range scan
```

**Idempotency pattern** (platform-events' `pkg/inbox`, TS-D19): `inbox.Store.Process` inserts the claim `ON CONFLICT DO NOTHING` as the **first** statement of the cascade's own transaction and inspects rows-affected — `1` = new (run the cascade in the same transaction), `0` = duplicate (no-op). Safe under concurrent consumers because the PK serialises duplicate deliveries at the DB level: a concurrent copy blocks on the claim's row lock and then sees the duplicate; a failed cascade rolls the claim back with it. The table's DDL stays service-owned (unchanged). Pruning of rows older than `PROCESSED_EVENTS_TTL_DAYS` (default 8) is `inbox.Store.Prune` — batched deletes (`PRUNE_BATCH_LIMIT`, default 10000), looped until none remain — on every `cmd/rotator` run (§13.1).

`rls_violation_log` (migration `000003_rls_violation_log`, TS-D18) is also service-owned DDL and RLS-exempt — a policy on it would recurse, because `log_rls_violation()` writes it from inside the RLS predicate (§4.3):

```sql
CREATE TABLE rls_violation_log (
  id               bigserial   PRIMARY KEY,                -- the cmd/server exporter's cursor
  table_name       text        NOT NULL,
  row_tenant_id    uuid,
  app_tenant_id    uuid,
  violation_type   text        NOT NULL CHECK (violation_type IN ('missing_or_invalid_guc', 'cross_tenant_access')),
  session_role     text        DEFAULT SESSION_USER,
  application_name text        DEFAULT current_setting('application_name', true),
  query_text       text,                                    -- left(current_query(), 2048)
  occurred_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX rls_violation_log_occurred_at_idx ON rls_violation_log (occurred_at);
ALTER TABLE rls_violation_log DISABLE ROW LEVEL SECURITY;
REVOKE ALL ON rls_violation_log FROM PUBLIC;
```

Grants: `SELECT` to `serviceaccount_reconciler` (the exporter's cross-tenant read, RLS-7) and `admin_readonly`; `serviceaccount_app` has **no** table grant — it writes only through `log_rls_violation()` and deletes only through `prune_rls_violation_log(ttl_days, limit)`, both `SECURITY DEFINER` (only the prune function is granted `EXECUTE` to the app role). Retention: `RLS_VIOLATION_LOG_TTL_DAYS` (default 30), pruned by `cmd/rotator`; since migration `000004` the prune function refuses a TTL under 7 days, so a compromised app credential cannot erase recent evidence (TS-D21).

`keys_refresh_pending` (migration `000005_keys_refresh_pending`, TS-D22; `intent_until` TS-D23) is service-owned DDL and RLS-exempt. It is the durable record that an RP-17 Keycloak key-cache refresh is owed for a tenant (§8.8):

```sql
CREATE TABLE keys_refresh_pending (
  tenant_id    uuid        PRIMARY KEY,
  requested_at timestamptz NOT NULL DEFAULT now(),   -- only ever moves forward (GREATEST(old, clock_timestamp()))
  intent_until timestamptz                            -- NULL = committed marker; non-NULL = cmd/scheduler intent marker
);
CREATE INDEX keys_refresh_pending_requested_at_idx ON keys_refresh_pending (requested_at);  -- the retry pass reads oldest-first
ALTER TABLE keys_refresh_pending DISABLE ROW LEVEL SECURITY;
REVOKE ALL ON keys_refresh_pending FROM PUBLIC;
-- GRANT SELECT, INSERT, UPDATE, DELETE ON keys_refresh_pending TO serviceaccount_app;   (marker writes)
-- GRANT SELECT ON keys_refresh_pending TO serviceaccount_reconciler;                    (retry pass + gauge, RLS-7)
```

Notes:

- **Writers:** only `cmd/rotator` (in the same transaction as each sweep revoke) and `cmd/scheduler` (an intent marker before each `IssueOrRotate`, re-written as committed after it). Both write on the app pool. `MarkPending` sets `intent_until = NULL`. `MarkIntent` never downgrades a committed marker or an expired intent. A marker is deleted only by `Clear(tenant, upTo)` with `requested_at <= upTo`, so a newer request is never cleared.
- **Readers:** the retry pass's `ListPending` (reconciler pool; committed markers, plus intent markers whose `intent_until` has passed, oldest first, with a flag saying whether the tenant still has a principal) and `cmd/server`'s gauge exporter (`PendingStats`, reconciler pool).
- **No tenant FK** and no RLS policy: one row per tenant, never tenant-queried, and a marker for a tenant offboarded since it was written must still be readable so the retry pass can drop it (§8.8).

### 4.3 Row-Level Security

Per HLD §7.2 (Layer 2), every tenant-scoped table carries an RLS policy keyed on the `app.tenant_id` GUC, which `platform-pgcommon` injects transaction-locally (`SET LOCAL`) under PgBouncer transaction-pooling mode. The connection role lacks `BYPASSRLS`. The `true` (missing-ok) flag on `current_setting('app.tenant_id', true)` is critical: without it, any connection that hasn't had the GUC set would raise an error rather than silently returning `NULL`; with `missing_ok = true`, an unset GUC evaluates to `NULL`, `tenant_id = NULL::uuid` is always false, and the policy **denies all rows** rather than erroring or allowing everything — the correct fail-closed behaviour.

```sql
ALTER TABLE service_account_principals  ENABLE ROW LEVEL SECURITY;
ALTER TABLE service_account_credentials ENABLE ROW LEVEL SECURITY;

-- FORCE RLS: enforces policies even for the table owner, preventing accidental bypass.
ALTER TABLE service_account_principals  FORCE ROW LEVEL SECURITY;
ALTER TABLE service_account_credentials FORCE ROW LEVEL SECURITY;

-- DEFAULT DENY: strip all PUBLIC privileges; serviceaccount_app is granted only what it needs.
REVOKE ALL ON service_account_principals  FROM PUBLIC;
REVOKE ALL ON service_account_credentials FROM PUBLIC;

-- app_tenant_id(): STABLE SECURITY DEFINER GUC parser; fail-closed to NULL on any error.
CREATE OR REPLACE FUNCTION app_tenant_id() RETURNS uuid
LANGUAGE plpgsql STABLE SECURITY DEFINER AS $$
DECLARE v text;
BEGIN
  v := current_setting('app.tenant_id', true);
  IF v IS NULL OR v = '' THEN RETURN NULL; END IF;
  RETURN v::uuid;
EXCEPTION WHEN OTHERS THEN RETURN NULL;
END;
$$;

-- rls_check_tenant(): the policy predicate used identically in USING and WITH CHECK.
CREATE OR REPLACE FUNCTION rls_check_tenant(p_tenant_id uuid, p_table_name text)
RETURNS boolean
LANGUAGE plpgsql STABLE STRICT SECURITY DEFINER
SET search_path = public AS $$
DECLARE v_app uuid;
BEGIN
  v_app := app_tenant_id();
  IF v_app IS NULL     THEN RETURN false; END IF;   -- fail-closed: no GUC → no rows / write rejected
  IF p_tenant_id <> v_app THEN RETURN false; END IF;-- cross-tenant → hidden / rejected
  RETURN true;
END;
$$;

-- FOR ALL with BOTH USING (read) and WITH CHECK (write) stated explicitly, named "<table>_rls".
CREATE POLICY service_account_principals_rls ON service_account_principals
  USING      (rls_check_tenant(tenant_id, 'service_account_principals'))
  WITH CHECK (rls_check_tenant(tenant_id, 'service_account_principals'));
CREATE POLICY service_account_credentials_rls ON service_account_credentials
  USING      (rls_check_tenant(tenant_id, 'service_account_credentials'))
  WITH CHECK (rls_check_tenant(tenant_id, 'service_account_credentials'));

-- outbox_events, processed_events, rls_violation_log, keys_refresh_pending are NOT tenant-scoped (operational) → no RLS.
-- Migration 000004 re-creates app_tenant_id() with SET search_path = pg_catalog, public, revokes it from
-- PUBLIC and grants EXECUTE to serviceaccount_app only (behaviour unchanged, TS-D21).
```

**Violation logging (migration `000003_rls_violation_log`, TS-D18).** `rls_check_tenant()` keeps the predicate above and, on each `false`, also calls `log_rls_violation(table, row_tenant_id, type)` — `SECURITY DEFINER`, error-swallowing, sampled at 1% (`app.rls_violation_sample_rate` overrides) — into the RLS-exempt `rls_violation_log` (`missing_or_invalid_guc` | `cross_tenant_access`). `cmd/server` counts new rows over the `serviceaccount_reconciler` pool into the Tier 2 `iam_rls_violations_total{violation_type}` (the metric iam-org-membership / iam-user-profile / iam-audit-log emit); `cmd/rotator` prunes past `RLS_VIOLATION_LOG_TTL_DAYS` through `prune_rls_violation_log()`. A cross-tenant write rejected by `WITH CHECK` is not logged: the error rolls the log row back with the caller's transaction.

**Invariant: `tenant_id` is never NULL on either tenant-scoped table.** Both declare `tenant_id uuid NOT NULL` — load-bearing for RLS correctness (a nullable value would make the comparison `NULL`, silently hiding the row rather than rejecting the integrity bug), and the composite child FK `(principal_id, tenant_id)` means no credential row can exist without a valid `(id, tenant_id)` pair in `service_account_principals`. Preserving this is a hard constraint on every future migration.

**Least-privilege roles.** The API server (`cmd/server`) and the offboarding consumer connect as a dedicated `serviceaccount_app` role holding only `SELECT, INSERT, UPDATE, DELETE` on its own tables — no DDL (migrations run as `serviceaccount_migrator` in the Helm migrate hook Job, §4.4), no `BYPASSRLS`, no access to other services' schemas. Every write goes through an RLS-scoped `RunInTx` with `app.tenant_id` bound to the acting tenant.

The **scheduled jobs** (`cmd/rotator`: overlap-expiry sweep §8.3, orphan-material reconcile §8.6, `keys_refresh_pending` retry pass §8.8; `cmd/scheduler`: the due-list scan §8.2) are inherently **cross-tenant batch** work — they must find candidate rows across every tenant, which a tenant-GUC-scoped read cannot do. They therefore use a dedicated **read-only** `serviceaccount_reconciler` role (`NOLOGIN BYPASSRLS`, `SELECT`-only on the two `service_account_*` tables, no `INSERT/UPDATE/DELETE`) **only to enumerate the candidate set** — the `(tenant_id, principal_id, version, openbao_path)` tuples due for action. Each resulting **write** (a revoke, a rotation, an orphan delete under the principal lock) is then performed as `serviceaccount_app` in a normal RLS-scoped `RunInTx` with `app.tenant_id` set to that row's tenant, so no write ever bypasses RLS (RLS-7). This is the same enumerate-under-BYPASSRLS-then-scoped-write pattern the sibling reconcilers use (Delegation's snapshot exporter, RP's cross-tenant administrative reads). `cmd/server` also holds a reconciler pool, read-only, for its DB-state exporters (§11.2) and the JWKS known-tenant list (§5.4); it never writes through it. `admin_readonly` remains a separate `SELECT`-only role for human operator tooling; the service's own jobs use `serviceaccount_reconciler`, never `admin_readonly`, and the API path never uses either.

**Provisioning writes and the RLS actor.** The internal endpoints (TS-1..TS-6) read, create or mutate rows under the *target* tenant. The Realm Provisioner / operator / cron calls them with the target tenant's `x-tenant-id` and the reserved system principal `x-user-id` (`00000000-0000-0000-0000-0000000000a1`, "iam-system"), so the GUC-bridge builds `GUCSet{UserID: system, TenantID: target}` — which makes the INSERT's `WITH CHECK` succeed for the new row. No row is ever inserted without a tenant context. The system principal is recognised only on `/api/v1/internal/*` and is rejected elsewhere. Because any pod that reaches the port can set that header, the real caller restriction is the Istio `AuthorizationPolicy` plus NetworkPolicy (§10.2).

| # | Invariant |
|---|---|
| RLS-1 | Both tenant-scoped tables run `FORCE` RLS + default-deny; CI (`test/postgres/rls_test.go`, testcontainers) verifies `relrowsecurity = true AND relforcerowsecurity = true` for both, that `serviceaccount_app` has `rolbypassrls = false`, and that `serviceaccount_reconciler` has `rolbypassrls = true` **with no write grant** on the tenant-scoped tables (RLS-7). |
| RLS-2 | A missing/malformed `app.tenant_id` GUC yields zero rows and permits no writes (fail-closed) — the `app_tenant_id()` `EXCEPTION` guard + the policy's `IF v_app IS NULL RETURN false`. |
| RLS-3 | `WITH CHECK` is stated identically to `USING`, so a cross-tenant INSERT/UPDATE is rejected, not just hidden — a credential row can never be written with a foreign `tenant_id`. |
| RLS-5 | Internal endpoints (TS-1..TS-6) execute under the reserved system principal (`x-user-id = …00a1`) + the target tenant's `x-tenant-id`; accepted **only** on `/api/v1/internal/*`. |
| RLS-6 | `app.tenant_id` is bound `SET LOCAL` per checkout, auto-reset at COMMIT/ROLLBACK; never leaks across a pooled backend. A session-scoped `SET app.tenant_id` is a CI-forbidden pattern. |
| RLS-7 | **`BYPASSRLS` is read-only and enumeration-only.** The `serviceaccount_reconciler` cron role may `SELECT` across tenants to build a candidate set, but holds no write privilege; every write (revoke/delete) is performed as `serviceaccount_app` in an RLS-scoped `RunInTx` bound to the row's own `tenant_id`. No code path writes a tenant-scoped table while bypassing RLS; CI asserts `serviceaccount_reconciler` has no `INSERT/UPDATE/DELETE` grant. |

### 4.4 Migrations

Migrations live in `internal/adapter/outbound/postgres/migrations/` and run via `pgadapter.Migrate` (`outbox.ApplySchema`, then `platform-pgcommon`'s `migrate.Runner{DSN, Logger}.Up(ctx)`, which appends `lock_timeout=30s`). In Kubernetes that happens once per release in the migrate hook Job; a binary started without `RUN_MIGRATIONS=false` (local runs, tests) still migrates at startup. Dev-stage posture: there are **five** migrations. `000001_schema.up.sql`/`.down.sql` is the single consolidated base — extensions, enums, `app_tenant_id()`/`rls_check_tenant()`, the two `service_account_*` tables with their indexes, the `touch_row` trigger + attachments (§4.5), RLS, `processed_events`, and the `serviceaccount_app`/`serviceaccount_migrator`/`serviceaccount_reconciler`/`admin_readonly` roles, in that order (`serviceaccount_reconciler` is `NOLOGIN BYPASSRLS` with `SELECT`-only grants on the two `service_account_*` tables — the cross-tenant enumeration role for the scheduled jobs, §4.3/RLS-7). `000002_rotation_cadence` adds `rotation_cadence_days`/`next_rotation_at` + `idx_sac_next_rotation` (§4.2, §16 TSQ-6). `000003_rls_violation_log` adds `rls_violation_log`, `log_rls_violation()`, the logging `rls_check_tenant()` and `prune_rls_violation_log()` with their grants (§4.2/§4.3, TS-D18). `000004_hardening` grants `serviceaccount_app` the outbox dead-letter table and ordering sequence, pins `app_tenant_id()`'s `search_path` and revokes it from `PUBLIC`, and makes `prune_rls_violation_log()` refuse a TTL under 7 days (TS-D21). `000005_keys_refresh_pending` adds the RLS-exempt `keys_refresh_pending(tenant_id PK, requested_at, intent_until)` table (§4.2): a durable "RP-17 key-cache refresh owed" marker for `cmd/rotator`/`cmd/scheduler` (app: SELECT/INSERT/UPDATE/DELETE; reconciler: SELECT; TS-D22). Its nullable `intent_until` separates the scheduler's pre-rotation intent markers (ignored by the retry pass until they expire) from committed markers (NULL, retried at once; TS-D23). In Kubernetes the migrations run once per release in a pre-install/pre-upgrade hook Job (`MIGRATE_ONLY=true`); the long-running workloads start with `RUN_MIGRATIONS=false` and never hold the migrator DSN (TS-D21). `outbox.ApplySchema` (owned by `platform-events`) runs **separately, before the domain migration references `outbox_events`**, so the domain migration's `GRANT`/`ALTER` on that table is `IF EXISTS`-guarded (§4.2, MIG-2). Nothing is deployed to any environment (§19), so migrations are **outright** (no expand/contract dance); once a real deployment exists, column drops split across two releases (deprecate-then-drop) for rolling-deploy compatibility.

DDL runs via `MIGRATION_DATABASE_URL` on the direct 5432 port, bypassing PgBouncer (transaction pooling is incompatible with DDL, whose advisory locks are session-scoped); the `serviceaccount_app` role cannot `CREATE` on `public`. `serviceaccount_migrator` owns the tables and is granted `BYPASSRLS` explicitly and narrowly (so a data migration is not blocked by `FORCE` RLS with no GUC set); `serviceaccount_app` is **never** granted `BYPASSRLS` — verified by `TestAppRoleLacksBypassRLS` on every CI run. `test/postgres/migration_roundtrip_test.go` runs every down migration and re-applies it, so a broken `.down.sql` fails CI (§14.2).

**RLS integration tests** (run in CI against real PostgreSQL via `testcontainers`, `test/postgres/rls_test.go`) — the canonical fail-closed matrix, run after every migration:

```sql
-- Case 1: Missing GUC → fail-closed (0 rows, no error)
RESET app.tenant_id;
SELECT count(*) FROM service_account_credentials;               -- expect: 0

-- Case 2: Cross-tenant write → rejected by WITH CHECK
SET LOCAL app.tenant_id = 'aaaa...-0001';
INSERT INTO service_account_principals (id, tenant_id, principal_sub, keycloak_client_id)
VALUES (gen_random_uuid(), 'bbbb...-0002', gen_random_uuid(), 'platform-automation');  -- expect: ERROR (WITH CHECK)

-- Case 3: Malformed GUC → fail-closed (0 rows, no error — EXCEPTION handler catches bad cast)
SET LOCAL app.tenant_id = 'not-a-uuid';
SELECT count(*) FROM service_account_principals;                -- expect: 0
```

| # | Invariant |
|---|---|
| MIG-1 | `outbox_events` is library-owned (`outbox.ApplySchema`); this service never defines its schema, only grants its app role on it and applies the payload-column customization (§4.2). |
| MIG-2 | `outbox.ApplySchema` runs before the domain migration references `outbox_events`, so every `GRANT`/`ALTER` against it is `IF EXISTS`-guarded. |
| MIG-3 | Post-launch `principal_type` values are added by additive `ALTER TYPE … ADD VALUE` (no table rewrite, PostgreSQL ≥ 12); the tenant-scoped-`NOT NULL`-`tenant_id` invariant (§4.3) must be preserved by every migration. |

### 4.5 Triggers — `updated_at` and optimistic-lock version

`updated_at` and `record_version` are maintained by the database, not application code, so every write path (including a rare direct/operator UPDATE) keeps them correct and the §9.1 optimistic-concurrency guard is reliable. The trigger carries a `WHEN (OLD.* IS DISTINCT FROM NEW.*)` guard so it fires **only when a row actually changes** — an idempotent TS-1 replay that produces an identical row writes nothing, so `record_version` does not churn and a concurrent optimistic-lock holder is never spuriously invalidated. No trigger reads or writes a secret or a secret-derived value.

```sql
CREATE OR REPLACE FUNCTION touch_row() RETURNS trigger AS $$
BEGIN
  NEW.updated_at     := now();
  NEW.record_version := OLD.record_version + 1;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER service_account_principals_touch_row  BEFORE UPDATE ON service_account_principals
  FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION touch_row();
CREATE TRIGGER service_account_credentials_touch_row BEFORE UPDATE ON service_account_credentials
  FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION touch_row();
```

Both tenant-scoped tables carry `updated_at` and `record_version`, maintained by `touch_row()`. On `service_account_credentials`, `updated_at` records the row's last status transition (issue is an INSERT; rotate → `rotating`, sweep/revoke → `revoked` are UPDATEs that fire the trigger). Because the version bump happens in a `BEFORE UPDATE` trigger, the §9.1 guard (`WHERE id=$1 AND record_version=$2`) compares against the pre-update value and the new value is written atomically in the same statement. There is **no** `sync_*` or projection trigger (this service has no derived column analogous to User Profile's primary-email mirror).

### 4.6 Data ownership summary

This service owns the two `service_account_*` tables (and `processed_events`, `rls_violation_log` and `keys_refresh_pending`, service-owned DDL) and the `iam.serviceaccount.events` topic; it owns **no** Keycloak state and **no** membership/role state. The `outbox_events` table is **library-owned** (`platform-events`, via `outbox.ApplySchema`) — this service only writes rows into it and customizes its payload column (§4.2). OpenBao is an external system of record for the secret *material*, referenced by path only (§6.3). Keycloak is an external system of record for the client identity and the `client_assertion` *validation*, written only by the Realm Provisioner (TS-INV-1).

---

## 5. API Contract

### 5.1 Conventions

- **API version** in the path: all routes are under `/api/v1`, and — unlike User Profile — **every route is additionally under `/internal`** (`/api/v1/internal/...`), because this service has no public/tenant-facing surface at MVP (§5.6). Breaking changes ship under a new prefix (`/api/v2/internal`) with both served during a deprecation window; additive changes stay on `v1`. The catalogue (§5.3) and specs write paths **in full** so every row is copy-paste-exact.
- Every route sits behind gincommon's observability stack and a 1 MiB request-body limit. The six TS routes add `RequireIdentityHeaders → RequireAuth → ContextMiddleware → GUCBridgeMiddleware → RequireJSONContentType → RequireTenantPathMatch` (exact order and roles in §3.3.1). `/healthz`, `/readyz`, the docs routes and the JWKS route are registered outside that group.
- **Header rules.** `x-user-id` and `x-tenant-id` must each appear exactly once and be a UUID, and `x-user-id` must be the reserved `iam-system` principal `00000000-0000-0000-0000-0000000000a1`; anything else is `401 missing_identity_headers` (no `details`). `x-tenant-roles` is ignored and stripped before gincommon reads it (TS-D23). There is **no JWT parsing** in this service. The headers are trusted only because the Istio `AuthorizationPolicy` and STRICT mTLS limit which workloads can reach which route (§10.2); on its own the header is forgeable by any pod that reaches the port.
- Identity is never taken from the request body or path for authorization. `tenant_id` for every query is the GUC, not a parameter — the `:id` path segment is the target tenant, validated to equal the `x-tenant-id` GUC before any checkout (a mismatch is `403`).
- Content type `application/json`; a POST with a body of any other type is `415 unsupported_media_type`. Timestamps are RFC 3339 UTC. Errors use the flat `gincommon.ErrorResponse` shape (§17).
- **Input validation** is applied in the service layer before any write: `rotation_id` must be a UUID; `overlap_seconds` is an integer clamped to `[0, 900]` (§6.2); `principal_sub` must be a UUID; `keycloak_client_id` is validated by `domain.ValidPlatformAutomationClientID` — the frozen base name `platform-automation` or that name suffixed with the target tenant's UUID (§25, rev 1.1); path `:id`/`:principal_id`/`:version` are typed-parsed. Validation failures return `400 invalid_request` with the specific field in `details` (§17).
- **Mutation responses include `record_version`** so callers can round-trip the optimistic-concurrency token (§9.1). No response body ever includes a stored secret; only the TS-1 response carries a freshly-generated plaintext, exactly once (§5.6, TS-INV-2).
- One route class only: **Internal routes** (`/api/v1/internal/...`) — reached only by other backend services on the mesh (Realm Provisioner, O&M, operators via the internal gateway, the Workflow Service for TS-6) and this service's own `cmd/scheduler` (through `CredentialService` in-process, not over HTTP). Subject to mTLS + headers, restricted by NetworkPolicy and the Istio `AuthorizationPolicy`, authorized as in §5.2. The EXT-6 JWKS route shares the prefix but not the header model (§5.4). There are **no** public/edge routes.

### 5.2 Authorization rules per route

**All routes are internal (`/api/v1/internal/*`) and the six TS routes use a single authorization model** — there is no user-facing route class and no per-user/role decision (TS-INV-4). The JWKS route is the one exception: it takes no identity headers and is reachable by Keycloak (§5.4, §10.2). The rules:

- Every route is reachable only from in-mesh service callers: the NetworkPolicy admits the mesh namespaces (Realm Provisioner, O&M, the operator gateway), Keycloak's namespace and, optionally, the Workflow Service's (`networkPolicy.workflowNamespaceSelector`, TS-D17). NetworkPolicy works per port, not per path, so the Istio `AuthorizationPolicy` (required outside local/dev/test, §10.2) narrows Keycloak to the JWKS route and Workflow to TS-6; the mesh namespaces keep every route. The public Envoy listener does not route `/internal/*`. `cmd/scheduler` calls `CredentialService` in-process and needs no ingress rule (§16 TSQ-6 Resolved, TS-D14).
- Every TS route requires the reserved system principal `x-user-id` (`00000000-0000-0000-0000-0000000000a1`, "iam-system") and the **target tenant's** `x-tenant-id`, establishing the RLS write actor (§4.3). The system principal is accepted **only** on `/internal/*` and is rejected elsewhere. A missing, repeated or non-UUID header, or any other `x-user-id`, is `401 missing_identity_headers` before any DB checkout (RLS-5).
- **Caller-by-route** (advisory, not enforced per-caller — all callers present the same system principal): the **Realm Provisioner** calls TS-4 (register) and TS-1 (issue at provisioning); **operators / O&M tooling** call TS-1 (rotate) and TS-2 (revoke); this service's **`cmd/scheduler`** (the dedicated cadence-scheduling identity, TS-D14) runs the same TS-1 use case in-process as the `iam-system` actor; **O&M / operators / RP-18** call TS-3 (read); **org-membership** calls TS-5 (find-by-sub, AUTH-9 defense-in-depth, TS-D16); the **Workflow Service**'s connector workers call TS-6 (read the tenant's automation subject, TS-D17). `cmd/rotator` calls no route of this service — it drives Postgres/OpenBao directly (§3). Both CronJobs call the Realm Provisioner's RP-17 route outbound (§8.8). No route accepts a tenant-facing principal, and no route makes an authorization *grant* — this service issues credentials, it does not authorize the principal (TS-INV-4).

### 5.3 Endpoint catalogue

| # | Method & path | Purpose | AuthZ | Idempotent |
|---|---|---|---|---|
| TS-1 | `POST /api/v1/internal/tenants/:id/service-accounts/:principal_id/credentials` | **Issue or rotate** the principal's credential — generate a new RSA-2048 keypair, write the private key into OpenBao at `max(version)+1`, mark it `active`, move the prior `active` → `rotating` with `expires_at = now()+overlap`, and **return the private key once** (`Cache-Control: no-store`). Emits `…CredentialIssued`/`…CredentialRotated` | system principal (RP / operator / O&M; `cmd/scheduler` in-process) | key (per `rotation_id`, within `ROTATION_REPLAY_WINDOW`) |
| TS-2 | `POST /api/v1/internal/tenants/:id/service-accounts/:principal_id/credentials/:version/revoke` | Revoke a specific credential version immediately (`status='revoked'`, delete its OpenBao material). Emits `…CredentialRevoked` | system principal (operator / O&M) | yes |
| TS-3 | `GET /api/v1/internal/tenants/:id/service-accounts/:principal_id` | Read the principal + credential **metadata** (versions, status, timestamps, OpenBao *path*) — **never a secret** | system principal (O&M / operator / RP-18) | read |
| TS-4 | `POST /api/v1/internal/tenants/:id/service-accounts` | **Register** the principal after RP mints the Keycloak client — records `principal_sub`, `keycloak_client_id`. Emits `ServiceAccountRegistered` | system principal (Realm Provisioner) | yes (on `(tenant_id, principal_type)`) |
| TS-5 | `GET /api/v1/internal/tenants/:id/service-accounts?principal_sub=<uuid>` | **Find by Keycloak sub** — looks up a principal by `principal_sub` rather than this service's own internal `principal_id` (AUTH-9, TS-D16). Returns identity/status only (`principalResponseBody`), never credential metadata — deliberately lighter than TS-3. A query-param lookup on the existing collection path, not a new path segment (a static "by-sub" segment at the same tree position as TS-3's `:principal_id` would conflict in gin's router) | system principal (org-membership) | read |
| TS-6 | `GET /api/v1/internal/tenants/:id/service-accounts/platform-automation` | **Read the tenant's automation principal** — the reverse of TS-5: resolves "which subject is tenant T's automation principal" from the tenant id alone (TS-D17). Returns identity including `principal_sub` and `keycloak_client_id`, never credential metadata. Addressed by the frozen principal name, the same segment the JWKS route already uses | system principal (Workflow Service) | read |
| TS-H | `GET /healthz`, `GET /readyz` | Liveness / readiness (DB, OpenBao and outbox checks, 2 s each; `503` while draining) | none (pre-auth) | read |
| TS-D | `GET /asyncapi`, `GET /asyncapi.yaml`, `GET /swagger/*any` | Rendered + raw contracts | gated (§12.1) | read |
| — | `GET /api/v1/internal/tenants/:id/service-accounts/platform-automation/jwks.json` | EXT-6: public JWK Set Keycloak's client-jwt authenticator fetches (§5.4, §6.1). The one deliberately unauthenticated-by-header route — see §10.2 | none (Keycloak's outbound fetch carries no caller identity); rate-limited; Keycloak namespace only, via the `AuthorizationPolicy` | read |

`GET …/service-accounts` is **not** a listing endpoint — TS-5's `principal_sub` query parameter is required, and the response is a single principal or `404`, never a collection. There is still no way to list every principal for a tenant at MVP — at MVP there is exactly one (`uq_sap_active_principal`), which TS-6 reads by its frozen name; a post-launch multi-principal surface (§2.4) would add a listing.

### 5.4 Key endpoint specifications

#### TS-4 `POST /api/v1/internal/tenants/:id/service-accounts` — register (Realm Provisioner)

Idempotent create from the Realm Provisioner, **after** RP has minted the `platform-automation` Keycloak client (RP-1/RP-2). Body carries the Keycloak service-account `sub` and client id:

```jsonc
// POST /api/v1/internal/tenants/acme-uuid/service-accounts
{ "principal_sub": "7f3c...", "keycloak_client_id": "platform-automation" }
```

```jsonc
// 201 Created (or 200 on idempotent repeat)
{ "principal_id": "2b1f...", "tenant_id": "acme-uuid",
  "principal_type": "platform_automation", "status": "active", "record_version": 1 }
```

Behaviour: **idempotent on `(tenant_id, principal_type)`** (`uq_sap_active_principal`) — an `INSERT ... ON CONFLICT (tenant_id, principal_type) DO UPDATE ... WHERE <principal_sub or keycloak_client_id differs>` followed by a read (rev 1.1). A repeat call whose `principal_sub`/`keycloak_client_id` are **unchanged** is a true no-op and returns the existing principal as `200`, never a duplicate, never re-emitting. A repeat call whose `principal_sub`/`keycloak_client_id` **differ** from the stored row — RP-3 conversion re-registering against a newly-minted dedicated-realm Keycloak client — updates the row in place (same `principal_id`) and **re-emits** `ServiceAccountRegistered`, since silently keeping the stale trial-realm identity would be incorrect. This **must precede** the first TS-1 for the principal (TS-1 against an unknown `principal_id` is `404 principal_not_found`). `principal_sub` is stored verbatim (RP-supplied, never generated here); `keycloak_client_id` is validated by `domain.ValidPlatformAutomationClientID` (§25, rev 1.1).

#### TS-1 `POST …/service-accounts/:principal_id/credentials` — issue / rotate (the crux)

Issue the first credential or rotate to the next version, and **return the generated secret exactly once** (TSQ-3 Resolved, §16). This is the only endpoint that ever returns a plaintext secret.

```jsonc
// POST /api/v1/internal/tenants/acme-uuid/service-accounts/2b1f.../credentials
{ "rotation_id": "c0ffee...-uuid", "overlap_seconds": 300 }
```

```jsonc
// 201 Created (200 on a rotation_id replay) — Cache-Control: no-store, Pragma: no-cache
{ "version": 4,
  "secret": "-----BEGIN RSA PRIVATE KEY-----\n…",                // PEM private key; field name kept for wire stability (EXT-6)
  "openbao_path": "iam/serviceaccount/acme-uuid/platform-automation/v4",
  "expires_prior_at": "2026-09-15T10:05:00Z",                     // when the superseded `rotating` key leaves the JWKS
  "record_version": 1 }
```

Behaviour:

- **Issue vs rotate.** The first-ever call for a principal is an *issue* (no active row to demote, emits `ServiceAccountCredentialIssued`); every later call is a *rotate* (moves the prior `active` → `rotating` with `expires_at = now() + overlap_seconds`, emits `ServiceAccountCredentialRotated` with `prior_version`/`expires_prior_at`). The new version is always `MaxVersion + 1` over **every** row of the principal, revoked included, so a version number is never reused (TS-D21).
- **Write ordering and locking** (fail-closed, §9.1/§9.3). Outside the lock: a `404`/`422` pre-check, the replay check below, and RSA-2048 key generation. Then one `RunInTx`: `SELECT … FOR UPDATE` on the principal row → re-check `revoked` and the replay → read the active row (and, for `cmd/scheduler`, require it to still be `ExpectActiveVersion`, else `409 optimistic_lock_conflict`) → write the private key to OpenBao at the new version, bounded at 5 s → close any other still-open overlap (TS-INV-3) → demote the prior `active` → insert the new `active` row (with `next_rotation_at`) → enqueue the event → commit. After the commit an opportunistic sweep revokes the principal's own expired `rotating` rows (§8.3). An OpenBao failure or timeout returns `502 secret_store_unavailable` with nothing committed; a crash after the write but before the commit leaves material no row claims, which the next TS-1 overwrites or the §8.6 reconciler reclaims (CUST-3).
- **Idempotency per `rotation_id`** (`uq_sac_rotation_id`, §9.2). A retry under the **same** `rotation_id` returns `200` with the same version and the same key **without generating new material**, but only within `ROTATION_REPLAY_WINDOW` (default 15m, `0` = unlimited) of the credential's `issued_at`; later it is `409 credential_replay_expired`, and once that version is revoked it is `409 credential_replay_revoked`. Every replay is logged at Info and counted in `credential_replays_total{result}` (TS-D23). A **different** `rotation_id` waits for the principal lock; it gets `409 rotation_in_flight` only if the lock is still held after `PG_LOCK_TIMEOUT` (with `details.active_rotation_id`, best effort).
- **`overlap_seconds`** is clamped server-side to `[0, 900]` regardless of the request (§6.2, TS-CONFIG-4). `0` is a hard cutover in this service's bookkeeping; omitted, it defaults to `ROTATION_DEFAULT_OVERLAP_SECONDS` (300, §12).
- **No caching.** The response carries `Cache-Control: no-store` and `Pragma: no-cache`, so no proxy or client cache keeps the key (TS-INV-2).
- The caller discards `secret` (Keycloak fetches the public half from the JWKS route) and then has the Realm Provisioner run RP-17 (`ClearServiceAccountKeysCache`) so Keycloak recognizes the new key; this service never writes Keycloak (TS-INV-1).

#### TS-2 `POST …/credentials/:version/revoke` — revoke one version

```jsonc
// 200 OK
{ "version": 3, "status": "revoked", "revoked_at": "2026-09-15T10:05:00Z", "keycloak_invalidation": "caller_responsibility" }
```

Revokes one credential version at **this service's** half of the credential: sets `status='revoked'`, `revoked_at=now()`, **deletes that version's OpenBao material** (CUST-2), emits `ServiceAccountCredentialRevoked`. It runs in one transaction that first locks the principal row (`LockForUpdate`, the same lock TS-1 takes) and re-reads the version under it; the OpenBao delete precedes the UPDATE and is bounded at 5 s, because every other writer for the principal waits on that lock (TS-D23). Idempotent — a re-revoke of an already-`revoked` version, or one that a concurrent revoke or the sweep won, is a `200` no-op. An unknown version is `404 principal_not_found`; a lock still held after `PG_LOCK_TIMEOUT` is `409 rotation_in_flight`; an OpenBao failure is `502 secret_store_unavailable` with nothing committed.

**TS-2 alone does not stop the key from authenticating at Keycloak (the two-halves rule, TS-INV-7).** Keycloak is the runtime validator and only the Realm Provisioner writes Keycloak (§6.1, TS-INV-1); TS-2 removes this service's *custody and metadata* of a version — the revoked key drops out of this service's JWKS response on the very next fetch — but Keycloak itself keeps validating a key it has already cached until RP explicitly evicts it (**corrected 2026-09-18, EXT-6**: there is no Keycloak-side TTL that does this automatically — see §6.2/TS-D4). This is true for **every** revoke path, including the ones that were previously described as "automatic": the **overlap-expiry sweep** (§8.3) and **offboarding** (§8.4; RP deletes the whole realm, which is itself the enforcement action, so no separate cache-clear is needed there). The **overlap-expiry sweep now requires RP's `ClearServiceAccountKeysCache` (RP-17) to actually take effect at Keycloak** — the sweep deletes OpenBao material and updates Postgres, but Keycloak's cache is a separate, RP-owned step. It is **especially not** sufficient on its own for a **break-glass / suspected-compromise revoke of a live `active` version**: cutting off a leaked key requires the *paired RP action* — see §8.7 and §16 TSQ-2. The `keycloak_invalidation: "caller_responsibility"` field in the response is the explicit reminder that the second half is the caller's; concretely, that responsibility is now "call `ClearServiceAccountKeysCache`," not "apply a new secret."

#### TS-3 `GET …/service-accounts/:principal_id` — read metadata

```jsonc
// 200 OK — metadata only; no secret, ever
{ "principal_id": "2b1f...", "tenant_id": "acme-uuid",
  "keycloak_client_id": "platform-automation", "principal_type": "platform_automation",
  "status": "active", "record_version": 2,
  "credentials": [
    { "version": 4, "status": "active",   "openbao_path": "iam/serviceaccount/acme-uuid/platform-automation/v4",
      "issued_at": "2026-09-15T10:00:00Z",
      "rotation_cadence_days": 90, "next_rotation_at": "2026-12-14T10:00:00Z" },
    { "version": 3, "status": "rotating", "openbao_path": "iam/serviceaccount/acme-uuid/platform-automation/v3",
      "issued_at": "2026-06-17T09:00:00Z", "expires_at": "2026-09-15T10:05:00Z" }
  ] }
// 404 principal_not_found if no principal for (tenant_id, principal_id)
```

Returns the principal plus its full credential-version list as **metadata only** — versions, statuses, timestamps, and the OpenBao *path* string. `rotation_cadence_days`/`next_rotation_at` (§16 TSQ-6 Resolved) are present only on the `active` entry — `null` on every `rotating`/superseded entry, since only the current version is ever "due" (§4.2). It runs a single RLS-scoped `SELECT` join over `service_account_principals` + `service_account_credentials` (`idx_sac_principal`), **never reads OpenBao**, and **never returns a secret** (§5.6). This is the service's only steady-state read path (§21).

#### TS-5 `GET …/service-accounts?principal_sub=<uuid>` — find by Keycloak sub (AUTH-9, TS-D16)

```jsonc
// GET /api/v1/internal/tenants/acme-uuid/service-accounts?principal_sub=7f3c...
// 200 OK — identity/status only, no credential metadata
{ "principal_id": "2b1f...", "tenant_id": "acme-uuid",
  "principal_type": "platform_automation", "status": "active", "record_version": 2 }
// 404 principal_not_found if no principal for (tenant_id, principal_sub) — the
// expected, common outcome, since almost every subject checked is a real
// human user, not the tenant's automation principal
```

Added post-sign-off (TS-D16) to give org-membership's AUTH-9
service-account-not-grantable defense-in-depth check the one identifier
it actually has: a subject's Keycloak `sub`, seen everywhere else as
`user_id`. This service never generates `principal_sub` itself
(TS-INV-1) — it is RP-supplied at TS-4 registration — so a caller cannot
derive "is this subject a service account" from anything but a
sub-keyed lookup here. Deliberately returns `principalResponseBody`
(identity/status), never `readPrincipalResponseBody`'s credential list —
a caller checking membership-grantability has no legitimate use for
credential metadata, and TS-3 already exists for that. Same RLS-scoped
`FindByPrincipalSub` read as TS-3's `FindByID`, just keyed differently
(§4.2). `GET …/service-accounts` is not otherwise a valid path — the
`principal_sub` query parameter is required; its absence or a
non-UUID value is `400 invalid_request`.

#### TS-6 `GET …/service-accounts/platform-automation` — read the tenant's automation principal (TS-D17)

```jsonc
// GET /api/v1/internal/tenants/acme-uuid/service-accounts/platform-automation
// 200 OK — identity only, no credential metadata
{ "principal_id": "2b1f...", "tenant_id": "acme-uuid", "principal_sub": "7f3c...",
  "keycloak_client_id": "platform-automation", "principal_type": "platform_automation",
  "status": "active", "record_version": 2 }
// 404 principal_not_found — the tenant has no automation principal (not yet
// minted, or offboarded)
```

Added for the Workflow Service (TS-D17): a connector worker serving many
tenants names the tenant's automation principal as the acting principal on
every step-completion callback, so it needs "which subject is tenant T's
automation principal" — TS-5 answers the reverse. RP-18 can compose the same
answer from RP's own client id plus TS-3, but a running worker should not
depend on the provisioner. `FindByType` on `(tenant_id, principal_type)`
(`uq_sap_active_principal`, RLS-scoped, §4.2).

**Subject stability.** `principal_sub` is **stable across credential
rotation** — TS-1/TS-2 (on-demand, `cmd/scheduler`'s 90-day cadence, the
overlap sweep) only touch `service_account_credentials`, never the principal
row, and RP-17 only refreshes Keycloak's key cache. It is **not stable across
a re-mint**: RP-3 (convert) mints the client in the new dedicated realm and
RP-4 (revert-conversion) mints it back in the shared trial realm. Each is a
new Keycloak service-account user, so a new `sub` (Keycloak can't pin a
service-account user's `sub` across realms), and the prior user is deleted
with its client or realm. TS-4's carry-over overwrites `principal_sub` in
place and re-emits `ServiceAccountRegistered`; the prior sub is not
retained. **`principal_id` is stable across both**, so it is the identifier
to keep on a long-lived record, alongside the sub at the time for
correlation. A caller that caches a tenant's sub should invalidate it on
`ServiceAccountRegistered` (`iam.serviceaccount.events`), which carries the
new `principal_sub`. Everything is hard-deleted on tenant offboarding (§8.4).

#### JWKS `GET …/service-accounts/platform-automation/jwks.json` — public keys for Keycloak (EXT-6)

```jsonc
// 200 OK — Cache-Control: no-cache, X-Content-Type-Options: nosniff
{ "keys": [ { "kty": "RSA", "kid": "<credential row id>", "use": "sig", "alg": "RS256", "n": "…", "e": "AQAB" } ] }
```

Keycloak's `client-jwt` authenticator (`use.jwks.url=true`) fetches this route; the request carries no identity headers, so it is registered outside the protected group and binds only the path tenant as the RLS GUC. `JWKSService.PublicKeys` lists the tenant's active `platform_automation` principal and returns one JWK per `active` or `rotating` credential whose overlap has not expired, reading each private key from OpenBao and publishing only its public half. A tenant with no principal gets `200` with an empty set.

- **Unreadable keys.** If the `active` key, or every live key, cannot be read, the route answers `503 jwks_keys_unavailable`: Keycloak keeps its previously cached keys on a failed fetch, which is safer than caching a set without the active key (TS-D23). A set missing only `rotating` keys is still `200`. Every unreadable live key is counted in `iam_token_service_jwks_key_errors_total` (pages); a request cancelled by the client is not counted.
- **Rate limits** (`429 rate_limited`, counted in `jwks_rate_limited_total` and `jwks_rate_limited_by_bucket_total{bucket}`). Every request spends its tenant's own bucket (`JWKS_RATE_LIMIT_PER_TENANT_RPS`/`_BURST`, 5/10, LRU-bounded at 10,000 tenants) and one shared bucket. A *known* tenant spends the global bucket (`JWKS_RATE_LIMIT_RPS`/`_BURST`, 20/40); any other tenant id spends only the small unknown-tenant bucket (`JWKS_RATE_LIMIT_UNKNOWN_TENANT_RPS`/`_BURST`, 2/5), so a flood of random tenant ids cannot starve Keycloak's real fetches. "Known" means every tenant with an `active` or `rotating` credential, reloaded from the database over the reconciler pool every `JWKS_KNOWN_TENANTS_REFRESH` (15s), plus tenants this replica has served since the last reload (TS-D23).

### 5.5 Status codes

| Code | When |
|---|---|
| `200` | Successful read (TS-3, TS-5, TS-6, JWKS), revoke (TS-2), TS-1 `rotation_id` replay, or idempotent no-op register/revoke |
| `201` | First-time creation — credential issued/rotated (TS-1) or principal registered (TS-4, first time) |
| `400` | Malformed request — bad UUID, `overlap_seconds` non-integer, bad content type (`invalid_request`) |
| `401` | Missing, repeated or non-UUID identity headers, or an `x-user-id` other than the iam-system principal UUID (`missing_identity_headers`, RLS-5) |
| `403` | Path `:id` tenant ≠ `x-tenant-id` (`tenant_path_mismatch`) |
| `415` | A request body without `Content-Type: application/json` (`unsupported_media_type`) |
| `429` | JWKS route rate limit (`rate_limited`) |
| `404` | Principal not found within the caller's tenant (`principal_not_found`) |
| `409` | `rotation_in_flight` (the principal's row lock is held past `PG_LOCK_TIMEOUT`; carries `details.active_rotation_id` when an active credential exists), `optimistic_lock_conflict` (§9.1), `credential_replay_revoked` / `credential_replay_expired` (TS-1 `rotation_id` replay, §9.2) |
| `422` | `principal_revoked` — issue/rotate against a `revoked` principal |
| `502` | `secret_store_unavailable` — OpenBao read/write/delete failed |
| `500` | `internal_error` — any unclassified failure |
| `503` | `db_unavailable` (Postgres unavailable); on the JWKS route, `jwks_keys_unavailable` (the active key, or every key, unreadable — TS-D23); `/readyz` while a dependency is down or the pod is draining |

Per-route error codes (every TS route can also return `401 missing_identity_headers`, `403 tenant_path_mismatch`, `400 invalid_request` for a bad path id, `503 db_unavailable` and `500 internal_error`):

| Route | Route-specific codes |
|---|---|
| TS-1 issue/rotate | `400 invalid_request` (`rotation_id`, `overlap_seconds`), `415 unsupported_media_type`, `404 principal_not_found`, `422 principal_revoked`, `409 rotation_in_flight` / `credential_replay_expired` / `credential_replay_revoked`, `409 optimistic_lock_conflict` (a concurrent write, or `cmd/scheduler`'s `ExpectActiveVersion` no longer matching), `502 secret_store_unavailable` |
| TS-2 revoke | `415 unsupported_media_type` (only if a non-JSON body is sent), `404 principal_not_found` (unknown version), `409 rotation_in_flight`, `409 optimistic_lock_conflict`, `502 secret_store_unavailable` |
| TS-3 read | `404 principal_not_found` |
| TS-4 register | `400 invalid_request` (`principal_sub`, `keycloak_client_id`), `415 unsupported_media_type`, `409 rotation_in_flight` (the principal row is locked past `PG_LOCK_TIMEOUT`, TS-D22) |
| TS-5 find by sub | `400 invalid_request` (missing or non-UUID `principal_sub`), `404 principal_not_found` |
| TS-6 read automation principal | `404 principal_not_found` |
| JWKS | `400 invalid_request` (non-UUID tenant), `429 rate_limited`, `503 jwks_keys_unavailable`, `503 db_unavailable`. No identity-header codes |

The full machine-readable error taxonomy is in §17. The OpenAPI 3 contract is generated into `docs/swagger/swagger.yaml` (§3) and is the source of truth for request/response schemas; this section is the human-readable summary.

### 5.6 What this service does NOT expose

No public/tenant-facing routes at MVP (§5.1). No endpoint ever returns a *stored* secret: TS-3 returns metadata + the OpenBao *path* string only, and only TS-1 returns a freshly-generated plaintext, exactly once (TS-INV-2). There is no endpoint that reads material back from OpenBao to a caller. A Swagger UI covers the internal contract, gated identically to `/asyncapi`: always mounted in local/dev/test, and in every other environment (staging included) mounted only with `DOCS_ENABLED=true` and protected by `DOCS_AUTH_TOKEN` (§12.1). The JWKS route returns public keys only.

---

## 6. Credential Lifecycle and Secret Custody

This is the service's distinctive concern — the analogue of the Event Consumer's realm→tenant resolution (that LLD's §6). It draws together the two-halves authority split (§2.3), the versioning/overlap rules (TS-INV-3), and the OpenBao custody model (§10.5) into one place.

### 6.1 The two-halves model

A credential's life is split across exactly two services and neither can complete a transition alone:

1. **Token Service (system-of-record).** Generates an RSA-2048 keypair (`DefaultKeyGenerator`, renamed 2026-09-18/EXT-6 — see rev 1.3), writes the PEM-encoded private key to OpenBao at the next version, versions and status-tracks the credential in Postgres, and returns the plaintext private key to the caller exactly once. It also serves the public half of every live (`active`/`rotating`) credential as a JWKS (`JWKSService`, `GET .../service-accounts/platform-automation/jwks.json`) — a capability, not a one-shot handoff, since Keycloak fetches it directly rather than receiving a value from RP.
2. **Realm Provisioner (sole applier).** Discards the private-key plaintext immediately (it never needs it — Keycloak fetches the public key itself from the JWKS above); its own action is calling `ClearServiceAccountKeysCache` (RP-17) so Keycloak re-fetches the JWKS and recognizes the new key / forgets a removed one, because only RP may write Keycloak (RP-INV-1).

Keycloak is the runtime **validator** — it fetches the JWKS and checks the presented `client_assertion` at the client-credentials token endpoint. This service is never on that hot path. The handshake is the same producer-and-applier shape RP already uses with O&M for RP-9/RP-16, adapted (2026-09-18, EXT-6) from "RP applies a value" to "RP triggers a Keycloak-side refresh."

### 6.2 Versioning and the rotation-overlap window

`version` is monotonically increasing per principal (`uq_sac_version`). At any instant there is **exactly one `active` version** (`uq_sac_one_active`, TS-INV-3). A rotation creates `version+1` as `active` and moves the prior `active` to `rotating` with `expires_at = now() + overlap_seconds` (clamped `[0, 900]`). During the overlap window **both** keys validate at Keycloak — both are present in this service's JWKS response (`JWKSService.PublicKeys`, §5.4-equivalent), verified empirically (EXT-6) against a real Keycloak instance to genuinely overlap, not a Keycloak-side TTL — so an in-flight caller still holding the old key is not cut off mid-request, **provided** the Realm Provisioner has already called `ClearServiceAccountKeysCache` after the rotation (Keycloak does not self-refresh a `jwks.url` client's key cache). When the window closes, the `rotating` version is revoked (its public key drops out of the JWKS response on the next fetch) and its OpenBao material deleted (§8.3) — RP calls `ClearServiceAccountKeysCache` again so Keycloak actually forgets the removed key, since a cached key keeps authenticating indefinitely otherwise. A zero `overlap_seconds` is a hard cutover in this service's own bookkeeping, but is only enforced at Keycloak once RP's cache-clear lands. A second rotation inside an open window ends the earlier overlap at once (its `expires_at` is set to now), so at most the new `active` key and the one just demoted are served (TS-INV-3, TS-D21). The JWKS route skips a `rotating` key whose `expires_at` has passed even before the sweep revokes it. For the automatic paths the RP-17 call is made by `cmd/rotator`/`cmd/scheduler` themselves and tracked by a durable `keys_refresh_pending` marker until it succeeds (§8.8).

### 6.3 OpenBao path scheme and custody

Secret material is written to KV v2 under the deterministic path `iam/serviceaccount/<tenant_id>/<keycloak_client_id>/v<version>` (frozen, §25). Postgres stores only that path string, never the material. Access is authorized by an OpenBao policy bound via the **Kubernetes auth method** (each workload's own ServiceAccount, presenting a projected token with audience `openbao` → an OpenBao role bound to the four workload ServiceAccounts → a path-scoped policy), never AWS IAM (HLD §11.2, §10.5). A version's material is deleted from OpenBao on revoke (TS-2, §8.3), when the reconciler finds it orphaned or left behind by a revoke (§8.6), and on offboarding, which deletes everything under the tenant's prefix (§8.4).

### 6.4 Custody invariants

| # | Invariant |
|---|---|
| CUST-1 | The plaintext exists in exactly two transient places — the OpenBao KV entry and the single TS-1 response body — and one durable place, OpenBao. Postgres and logs never hold it (TS-INV-2). |
| CUST-2 | A credential version's OpenBao material is deleted no later than the version's revoke; a `revoked` row never has live OpenBao material. |
| CUST-3 | The OpenBao path is a pure function of `(tenant_id, keycloak_client_id, version)` — reconstructible without a Postgres read, so a metadata/material divergence is detectable and reconcilable by rotation (§13). |

---

## 7. Event Architecture

### 7.1 Inbound — SQS consumer

The Token Service has **one active inbound SQS subscription: `TenantMembershipsPurged` on `iam.tenant.events`**, which drives the tenant-offboarding cleanup (§8.4). All other Token Service state changes originate from its own internal API (TS-1, TS-2, TS-4) or its own CronJobs (`cmd/rotator`, `cmd/scheduler`), not from inbound events.

| Queue | Event | State change |
|---|---|---|
| `tenant-lifecycle-tokensvc-q` (DLQ `tenant-lifecycle-tokensvc-q-dlq`, `maxReceiveCount=5`) | `TenantMembershipsPurged` — Core's tenant hard-delete / GDPR-wipe signal (renamed from `TenantOffboarded` under ADR-0008, TS-D9) | **Hard-delete** the tenant's `service_account_*` rows (FK-cascade principal → credentials) and **delete all OpenBao material** for the tenant, then emit `ServiceAccountRevoked`. Exactly-once via platform-events' inbox over `processed_events` (`consumer = 'tenant_offboarding'`), keyed on the envelope `id`: the claim, the OpenBao deletes (before commit), the row deletes and the outbox events run in one transaction (TS-D19). |

The queue is created and owned by this service, with an SNS filter policy on `EventType = TenantMembershipsPurged` so no other `iam.tenant.events` type is delivered. It follows the `tenant-lifecycle-<svc>-q` naming (the `lifecycle` segment deliberate, mirroring the siblings' equivalent queues), not the plain HLD §9.1 `<topic>-<consumer>-q` convention.

**Why the Token Service consumes this event (cross-service PII/credential completeness).** Tenant offboarding must destroy the tenant's credentials everywhere they live. The Realm Provisioner deletes the Keycloak realm (and with it the `platform-automation` client) and Org & Membership scrubs its own rows, but **the Token Service holds the tenant's credential metadata in its own database and its secret material in OpenBao** that neither of those reaches. Each service destroys the data **it** owns on the same terminal `TenantMembershipsPurged` event — the GDPR-correct boundary (§15.2).

The Token Service **does not** consume `MembershipRevoked` — a per-user membership removal never touches a service-account principal (the principal is a non-member by construction; O&M AUTH-9, TS-INV-4).

**Consumer pipeline (rev 1.4).** `cmd/consumer` builds its consumer with `events.NewSQSConsumerWithClient` plus `events.WithConsumerCodec(eventbus.GlueDecoder{})`: O&M publishes `TenantMembershipsPurged` through its own `GlueCodec`, and the decoder strips the self-describing 18-byte header (`[0x03][0x00][16-byte version UUID]`) with no Glue client or registry — this process still needs no Glue credentials. Each message then runs, outermost first:

1. **DLQ router** (`cmd/consumer/dlq.go`) — a permanent reject is `SendMessage`'d to `tenant-lifecycle-tokensvc-q-dlq` (the decoded envelope with `dataschema` cleared; `EventType` + `DLQReason` attributes) and the source message acked. Two reasons exist: `schema_violation` (step 3) and `invalid_envelope_id` (a `TenantMembershipsPurged` whose envelope id is missing or invalid, step 4 — it cannot be deduplicated, and acking it would silently skip a GDPR erasure; TS-D22). Each send is logged and counted in `iam_token_service_consumer_dlq_rejects_total{reason}`; `invalid_envelope_id` pages (`IAMTokenServiceOffboardingInvalidEnvelopeRejected`, TS-D23). The DLQ URL is read once at startup from the queue's own `RedrivePolicy` (no env var). If it can't be resolved, or a send fails, the message falls back to normal retry + SQS redrive after `maxReceiveCount = 5`.
2. **Cascade metrics** — `iam_token_service_offboarding_cascade_total{result}` (a rejected payload counts as `error`). The proposed Tier-2 `iam_offboarding_cascade_total{outcome}` is not emitted until it is ratified in the Platform Observability Registry.
3. **`validateConsumed`** (`cmd/consumer/inbound_schema.go`) — the payload is checked against the embedded `tenant_memberships_purged.json` (this service's own contract: `tenant_id` required, open schema). No schema for the type → pass through to `ackUnknown`. A violation never reaches the cascade or `processed_events`: it increments `iam_token_service_consumed_schema_violations_total{consumer,event_type}` (pages via `IAMTokenServiceConsumedSchemaViolation` — that tenant's credentials were **not** cleaned up) and routes to the DLQ as `DLQReason=schema_violation`.
4. **`OffboardingConsumer.Handle`** — the cascade (§8.4). An unknown event type is acked (forward compatibility), even with a bad id; its `event_type` label folds to `other` outside a closed set.

`cmd/consumer` also serves `/healthz`, `/readyz` and `/metrics`. On SIGTERM `/readyz` returns `503` for `SHUTDOWN_DRAIN_DELAY` (default 5s) before the receive loop drains (`SQS_DRAIN_TIMEOUT`, 15s); a receive loop that exits on its own makes the process exit non-zero so Kubernetes restarts it (TS-D21).

### 7.2 Serialization format

Event payloads are serialized as **JSON** (UTF-8) — the format registered in the Glue Schema Registry, referenced in `api/asyncapi.yaml`, and used on the wire in SNS/SQS.

**Rationale:** credential-lifecycle events are very low-frequency (a handful per tenant per quarter — provision, rotate, revoke), so payload size and parse-speed differences between JSON and binary formats are immaterial. JSON is natively readable in CloudWatch Logs, SQS dead-letter queues, and incident post-mortems without a decoder — which matters especially for security-adjacent credential events. AWS Glue Schema Registry has first-class JSON Schema support that works cleanly with the AsyncAPI spec, and the binary-toolchain overhead (`protoc`, generated stubs) is unjustified for one internal Go consumer (Audit). The envelope (`id`, `type`, `source`, `time`, `tenant_id`) comes from the shared `platform-events` CloudEvents-aligned envelope; the `EventType` SNS message attribute is **PascalCase** because it is what subscribers filter on.

### 7.3 AsyncAPI contract

The event contract for `iam.serviceaccount.events` is specified in **`api/asyncapi.yaml`** (AsyncAPI 3.0) — the single source of truth for channel name, envelope structure, per-event JSON payload schemas, SQS consumer bindings, and schema version metadata. It is hand-authored (there is no annotation-driven equivalent of `swag` for AsyncAPI, the same gap the REST surface does not have — §3). The canonical spec is **referenced here, not reproduced**, to avoid drift; consult the file for the exact channel/operation/message/schema definitions. It is served at `/asyncapi` (gated, §12.1).

**CI gate (`platform-schemagov`).** Schema governance is delegated to the shared `schema-gov` CLI (pinned Docker image). On every PR, `schema-gov validate --asyncapi api/asyncapi.yaml` runs in the schema-registry workflow alongside the OpenAPI/Swagger check (§13.5); the spec must pass (JSON Schema structural validity + AsyncAPI 3.0 lifecycle) before merge. The same image provides `diff` (breaking-change detection), `register` (idempotent Glue registration — the only safe path), and `enforce-lifecycle`. `api/asyncapi.yaml` must be updated in the same commit as any Glue registry version bump — the CI gate enforces consistency.

#### 7.3.1 AWS Glue Schema Registry

**`api/asyncapi.yaml` is the design-time contract; AWS Glue Schema Registry (ap-south-1) is the runtime enforcement point.** Each event type's JSON Schema is registered as a separate schema version in a single Glue registry named `iam-serviceaccount-events`. The registry name and ARN are injected via env vars (§12). Registration is performed by `schema-gov register` in CI — never hand-rolled `aws glue` calls.

**Registry layout:**

| Glue registry | Glue schema name | Glue data format | Version |
|---|---|---|---|
| `iam-serviceaccount-events` | `ServiceAccountRegistered` | JSON | 1 |
| `iam-serviceaccount-events` | `ServiceAccountCredentialIssued` | JSON | 1 |
| `iam-serviceaccount-events` | `ServiceAccountCredentialRotated` | JSON | 1 |
| `iam-serviceaccount-events` | `ServiceAccountCredentialRevoked` | JSON | 1 |
| `iam-serviceaccount-events` | `ServiceAccountRevoked` | JSON | 1 |

The event-type constant is PascalCase and **is** the Glue schema name, the AsyncAPI message definition, the `EventType` SNS attribute name *and* value, and the envelope `type` field — one naming form, no translation (matching the User Profile rev-0.46 model).

**Version resolution is by definition, not "latest" (rev 1.4).** At startup `NewGlueCodec` calls `glue:GetSchemaByDefinition` for each of the five produced schemas with this binary's embedded schema, sent in exactly the string `schema-gov register` uploads — Python `json.dumps(schema, separators=(",", ":"))`: compact, key order preserved, non-ASCII `\uXXXX`-escaped (`registeredDefinition` = `json.Compact` + `asciiEscape`, pinned against real Python per schema) — and requires status `AVAILABLE`. Each build therefore stamps the version that actually describes its payloads (correct through deploy/registration races, rollbacks, and schemas registered ahead of deploy); the UUID is fixed for the process lifetime — no refresher, no per-event Glue call. A definition not registered yet makes `cmd/server` panic at startup until `schema-registry.yml` registers it.

**Encoding happens at SNS-publish time, not before `outbox.Enqueue`** (the `platform-events` model since v1.4.0, §3.3.3). `Publisher.Enqueue` marshals plain JSON, validates it via the `ValidatingCodec` against the registered JSON Schema (the returned bytes discarded — only the validation side effect is used), and calls `outbox.Enqueue`; `outbox_events.payload` therefore always holds plain JSON. Glue wire-format encoding (an 18-byte version-prefixed header) happens later, when the outbox runner hands the record to `events.NewSNSPublisher`'s configured `events.WithCodec(glueCodec)` hook, immediately before the SNS publish — so the payload column is never corrupted under `PGBouncerMode` (§3.3.2). The Audit consumer decodes with the Glue SDK, strips the version prefix, fetches the (locally cached) schema, and validates before its handler runs; invalid payloads go to its DLQ.

**Schema evolution rules:**
- Additive changes (new optional fields) → register a new Glue schema version + update `api/asyncapi.yaml` in the same PR.
- Breaking changes (remove/rename fields, change types) → new Glue schema name + new AsyncAPI message definition; bump the major version. After sign-off (§25) this is a breaking change under the schema-evolution discipline.

**Wiring (mirrors the sibling pattern — enqueue uses the `ValidatingCodec`, Glue encoding happens only at publish time):**

```go
// Enqueue time (eventbus/publisher.go) — plain JSON, validate-only:
return pgcommon.RunInTx(ctx, pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
    if err := repo.RotateCredential(ctx, tx, cred); err != nil { return err }   // state write
    payload, _ := json.Marshal(rotatedPayload)
    if _, _, err := validatingCodec.Encode(ctx, "ServiceAccountCredentialRotated", payload); err != nil { return err } // schema check
    env := events.NewEnvelope("ServiceAccountCredentialRotated", "iam-token-service",
        json.RawMessage(payload), events.WithTenantID(rc.TenantID), events.WithTraceID(rc.TraceID))
    return outbox.Enqueue(ctx, tx, env)     // same tx as the state write (EVT-1)
})

// Publish time (cmd/server/wiring.go) — Glue encoding via the separate snsCodec:
publisher := events.NewSNSPublisher(snsConfig, events.WithCodec(glueCodec))
```

**Cost:** AWS Glue Schema Registry is free in ap-south-1 (Data Catalog free tier); no per-encode/decode fee.

### 7.4 Outbound — `iam.serviceaccount.events` via the outbox

The Token Service publishes to a **single** SNS topic, so it uses a plain `events.NewSNSPublisher` with no RoutingPublisher (HLD §9.2). Every credential state change writes its event to `outbox_events` in the **same transaction** as the business write (HLD §9.2, EVT-1): the credential-row transition and the outbox row commit together, so an event is never published for a rotation that rolled back, and no committed transition lacks its event. The `outbox.Runner` polls at 500 ms and relays to `iam.serviceaccount.events` at-least-once; event-type names are PascalCase (the `EventType` SNS attribute subscribers filter on).

**SNS/SQS fan-out (HLD §9.1).** The `iam.serviceaccount.events` topic fans out to one consumer queue at MVP; each queue is created and owned by its consuming service, each with a DLQ (`maxReceiveCount=5`):

| Consumer | SQS queue | DLQ |
|---|---|---|
| Audit Log | `serviceaccount-audit-q` | `serviceaccount-audit-q-dlq` |

No downstream **authorization** consumer subscribes — the automation principal carries no roles, so nothing in AuthZ Enrichment reacts to these events (TS-INV-4). They are **audit-only**. A post-launch tenant-bot/PAT surface (§2.4) would not change this topic's audit-only character.

### 7.5 Published events

The canonical payload schemas live in `api/asyncapi.yaml` (§7.3) and are registered in Glue (§7.3.1). Serialization is JSON (§7.2). The table is a summary reference.

| Event Type | Emitted when | Payload (key fields) | Consumers |
|---|---|---|---|
| `ServiceAccountRegistered` | TS-4 registers a principal | `tenant_id, principal_id, principal_sub, keycloak_client_id, principal_type, created_at` | Audit |
| `ServiceAccountCredentialIssued` | TS-1 first credential for a principal (`version = 1`) | `tenant_id, principal_id, version, issued_at` | Audit |
| `ServiceAccountCredentialRotated` | TS-1 rotation (`version > 1`) | `tenant_id, principal_id, version, prior_version, expires_prior_at` | Audit |
| `ServiceAccountCredentialRevoked` | TS-2, or the overlap-expiry sweep (`cmd/rotator` or TS-1's opportunistic sweep) | `tenant_id, principal_id, version, revoked_at` | Audit |
| `ServiceAccountRevoked` | Principal fully revoked (offboarding; one per principal, no per-version event) | `tenant_id, principal_id, revoked_at` | Audit |

**No secret, and no secret-derived value, ever appears in any payload** (TS-INV-2, EVT-2) — a negative contract test asserts no payload contains a `secret`/material field (§14.3). Payloads are self-contained snapshots (the full current value of the changed fields), not deltas, so a consumer applying them out of order converges to the correct latest state.

Event invariants:

| # | Invariant |
|---|---|
| EVT-1 | Every credential state transition emits exactly one event through the outbox, in the **same transaction** as the state write (TS-INV-5, §7.4). |
| EVT-2 | No event payload carries a secret or a secret-derived value (TS-INV-2). |
| EVT-3 | Redelivery is safe: each envelope `id` (UUID v7) is stable and Audit dedups on it (HLD §9.3); the inbound offboarding cascade dedups via platform-events' inbox over `processed_events` (TS-D19). |
| EVT-4 | Audit visibility does not depend on a live call — the outbox decouples emission from Audit availability (§10.6, §20.1). |

### 7.6 Idempotency and ordering

Publishing is at-least-once (outbox + SNS); the Audit consumer is idempotent via `processed_events` keyed on the envelope `id` (UUID v7, HLD §9.3). The inbound offboarding cascade is exactly-once via platform-events' inbox over `processed_events` (`consumer = 'tenant_offboarding'`, TS-D19). There is **no ordering requirement** — each credential operation is idempotent per `rotation_id` (TS-1, §9.2) or per version (TS-2), and payloads are self-contained snapshots (§7.5), so out-of-order or redelivered events converge. A consumer that rejects a Glue-decode failure routes it to its DLQ rather than retrying indefinitely — a persistent schema mismatch is a registry misconfiguration, not a transient error.

---

## 8. Key Request Flows

### 8.1 Provisioning handshake (tenant create)

```mermaid
sequenceDiagram
    participant RP as Realm Provisioner
    participant KC as Keycloak
    participant TS as Token Service
    participant OB as OpenBao
    RP->>KC: mint `platform-automation` service-account client (RP-1/RP-2, sole writer)
    RP->>TS: TS-4 register {principal_sub, keycloak_client_id}
    TS-->>RP: 201 (principal registered)
    RP->>TS: TS-1 issue {rotation_id}
    TS->>OB: write private key @ v1
    TS-->>RP: 201 {version:1, secret (PEM private key, once)}
    Note over RP: discard the private key (never needed: Keycloak reads the public half)
    KC->>TS: GET …/platform-automation/jwks.json (first client-jwt login)
    TS->>OB: read key @ v1
    TS-->>KC: 200 {keys:[v1 public JWK]}
```

The client is minted with Keycloak's `client-jwt` authenticator and `use.jwks.url=true` pointing at this service's JWKS route (EXT-6). Keycloak fetches the JWKS lazily on the first login, so no RP-17 cache-clear is needed at mint time; it is needed after every later rotate/revoke (§8.2, §8.3).

### 8.2 Rotation (RP-17)

Triggered one of two ways: an **operator or O&M tooling** (the on-demand path, also used for break-glass/suspected-compromise, §8.7), or **`cmd/scheduler`** automatically once a credential's `next_rotation_at` has passed (§16 TSQ-6 Resolved, TS-D14). **`cmd/rotator` never rotates**; it is cleanup-only (§8.3/§8.6/§8.9).

Either way the rotation is two halves. TS-1 (§5.4) generates a new RSA-2048 keypair, writes the PEM private key to OpenBao at the next version under the principal lock, marks it `active` (with `next_rotation_at = now() + rotation_cadence_days`, §4.2), moves the prior version to `rotating` with `expires_at = now() + overlap`, and returns the private key once. The caller discards the key — RP never receives it (TS-INV-1); Keycloak fetches the public half from the JWKS route. Then RP-17 (`ClearServiceAccountKeysCache`) must run so Keycloak re-fetches the JWKS and recognizes the new key; Keycloak does not self-refresh a `jwks.url` client (EXT-6, rev 1.3, TS-D4). On the operator path the operator or O&M tooling asks RP for RP-17; on the scheduler path `cmd/scheduler` calls RP-17 itself.

**`cmd/scheduler` run** (`cmd/scheduler/scan.go`):

1. **Retry owed refreshes** from earlier runs (§8.8), using at most half the run budget.
2. **Enumerate** due `active` credentials across tenants as `serviceaccount_reconciler` (`idx_sac_next_rotation`, most overdue first, at most `SCHEDULER_BATCH_LIMIT`, default 500) and group them by tenant.
3. **Per tenant**, start only if at least 30 s of run budget remains (20 s for a rotation plus 10 s for RP-17) and the run has not been signalled. For each due row:
   - write the tenant's **intent marker** (`MarkIntent`, §8.8);
   - call `CredentialService.IssueOrRotate` in-process under a fresh `rotation_id`, with `ROTATION_DEFAULT_OVERLAP_SECONDS` and `ExpectActiveVersion` = the version found due, inside an RLS-scoped transaction for that tenant (RLS-7);
   - unless the rotation was refused outright, re-write the marker as **committed** (`MarkPending`).
   `principal_revoked`, `principal_not_found` (an offboarding race), `rotation_in_flight` and `optimistic_lock_conflict` (an operator rotated first) count as **skipped**, not failed (TS-D21/TS-D23).
4. **RP-17 once per tenant**, inline, on a context detached from the run deadline and bounded at 10 s. On success the marker is cleared up to the value observed; on failure it is re-marked committed, every rotation of that tenant counts as `failed` (`iam_token_service_cadence_rotation_total{result="failed"}`, pages), and the next run's retry pass repeats RP-17. If nothing committed for the tenant, the intent marker is withdrawn only when it was fresh and every attempt was refused outright.

The TS-D15 two-halves gap (a committed rotation whose RP-17 failed, with `next_rotation_at` already advanced) therefore **self-heals** through the marker once the Realm Provisioner is healthy; it still pages, and a backlog older than 15 minutes raises `IAMTokenServiceKeysRefreshBacklog`. On SIGTERM no new tenant starts, the in-flight tenant finishes (including its RP-17), and the rest are deferred to the next run. A run with any failure exits 1 and fails the Job (§13.4).

The expired `rotating` row is revoked later by the overlap-expiry sweep (§8.3) or by the next TS-1 for the principal. A lost TS-1 response is retried under the same `rotation_id` and returns the same version — no double-rotation.

### 8.3 Overlap-expiry sweep (rotation CronJob)

`cmd/rotator` runs on a schedule (default every 5 minutes, `concurrencyPolicy: Forbid`). Each run:

1. **Retries owed refreshes** (§8.8) with at most half of the run budget. A failure there counts towards the sweep's `failed` result.
2. **Enumerates**, as the read-only `serviceaccount_reconciler` role (`BYPASSRLS`, §4.3), every `rotating` credential across all tenants whose `expires_at` has passed (`idx_sac_overlap`, oldest first, at most `ROTATOR_BATCH_LIMIT`, default 500), and groups the rows by tenant. A failed enumeration fails the run: if it failed every run, expired keys would silently stay valid.
3. **Per tenant**, starts only if at least 30 s of run budget remains (20 s for a revoke plus 10 s for RP-17) and the run has not been signalled. For each row, one RLS-scoped `RunInTx` as `serviceaccount_app` bound to that tenant (RLS-7): re-read the row (anything no longer `rotating` is **skipped**), delete its OpenBao material, set `status='revoked'`, upsert the tenant's committed `keys_refresh_pending` marker, and enqueue `…CredentialRevoked` — all committed together. The sweep does not take the principal row lock; an `optimistic_lock_conflict` at the write (a concurrent TS-2 or rotator run won) is also **skipped**, not failed (TS-D13).
4. **RP-17 once per tenant**, inline, on a context detached from the run deadline and bounded at 10 s. Without it the revoke is cosmetic: Keycloak keeps trusting a cached key until RP-17 lands (TS-INV-7, §6.2). On success the marker is cleared up to the value the revokes wrote. On failure every revoke of that tenant counts as `failed` (the run records `iam_token_service_rotation_sweep_total{result="error"}`, alert `IAMTokenServiceRotationSweepFailures`, and the Job fails) and the marker stays for the next run's retry pass.

Rows not reached (batch limit, budget, SIGTERM) are **deferred**: they are still `rotating` and expired, so the next run enumerates them again. The JWKS route already stops serving an expired overlap key before the sweep revokes it (§5.4). The revoke is idempotent, so a missed run is caught by the next.

The next TS-1 for a principal also opportunistically sweeps that principal's own expired `rotating` rows after its commit (`CredentialService.sweepExpiredRotating`, under the request's tenant GUC, no `BYPASSRLS`, on a context detached from the request and bounded at 10 s). That path writes no marker and calls no RP-17: it relies on the TS-1 caller's mandatory RP-17 for the new key (§8.2), which also makes Keycloak forget the key just revoked, since the JWKS excludes revoked credentials. `cmd/rotator`'s standalone sweep has no such caller, which is why it calls RP-17 itself.

### 8.4 Offboarding cleanup

On `TenantMembershipsPurged`, the consumer deletes every OpenBao entry under the tenant's prefix, hard-deletes the tenant's `service_account_*` rows (FK-cascade from principal to credentials), then emits `ServiceAccountRevoked` per principal. No RP-17 call is needed: RP deletes the realm, which is itself the Keycloak-side enforcement (TS-INV-7). Inert-if-delayed, like the sibling cascades: nothing reads a departed tenant's credential.

`OffboardingConsumer.Handle` (over `port.Inbox`, TS-D19): for `TenantMembershipsPurged`, a missing/invalid envelope id is a permanent reject sent straight to the DLQ (`DLQReason=invalid_envelope_id`, TS-D22) — acking it would silently skip a GDPR erasure; for any other type it is acked (it cannot be deduplicated, and a producer addition must never DLQ-storm); an unrecognized type goes to `ackUnknown`; the payload is decoded (`tenant_id` required); the GUC set is bound to the tenant + system principal; then `processOnce` → `InboxRepository.ProcessOnce` (`inbox.Store.Process`) runs **one** transaction on the RLS-scoped app pool (`SET LOCAL app.tenant_id`, RLS-6):

1. claim `processed_events` (`INSERT … ON CONFLICT DO NOTHING`) — `0` rows → duplicate, ack, no side effect;
2. `LockByTenant` — the tenant's principal rows `SELECT … FOR UPDATE`, the same row lock TS-1/TS-2 take, so no issue/rotate/revoke for the tenant can write material until this transaction ends (TS-D21) — then `ListByPrincipal` per principal, collecting every `openbao_path`;
3. OpenBao `Delete` of each path, then a walk of the tenant's whole prefix (`iam/serviceaccount/<tenant_id>/`, at most 4 levels) deleting every remaining leaf — this also removes material a crashed or racing TS-1 wrote but never committed. Material-first, still **before** the commit (§9.3/§15.2: once the principal rows are gone the §8.6 reconciler can never find this material);
4. `DeleteByTenant` (FK-cascades the credentials) and enqueue `ServiceAccountRevoked` per principal;
5. COMMIT.

Any failure rolls back the claim with the cascade, so the redelivery repeats it (OpenBao `Delete` of a missing path is a no-op). A principal lock still held after `PG_LOCK_TIMEOUT` surfaces as `rotation_in_flight` and is retried the same way. The OpenBao calls run on the consumer's handler deadline (`SQS_HANDLER_TIMEOUT`, 45 s, below the 60 s visibility timeout). A concurrent copy of the same message blocks on the claim's row lock and then sees a duplicate, so the cascade runs once. Trade-off: the OpenBao HTTP calls run while the transaction is open — acceptable for a rare, per-tenant event.

### 8.5 Read metadata (TS-3)

O&M, an operator, or RP-18 reads the principal and its credential-version metadata for display or reconciliation. The handler runs a single RLS-scoped `SELECT` join over `service_account_principals` + `service_account_credentials`; it never touches OpenBao and returns no secret. This is the service's only steady-state read path (§21).

### 8.6 Orphaned-material reconciliation (rotation CronJob)

The TS-1 write is ordered **material-first, then row, then commit** (§9.3), so a crash between the OpenBao write and the Postgres commit leaves an OpenBao entry at `…/v<n>` with **no committed credential row**. TS-2 and the sweep are material-first in the destructive direction, so a crash there can leave the opposite divergence. `cmd/rotator`'s orphan reconciler (`runOrphanMaterialReconciler`, every rotator run, after the sweep) reclaims the first and reports the second:

1. **Enumerate** every principal with its credential rows across all tenants as the read-only `serviceaccount_reconciler` role (`BYPASSRLS`, §4.3). Start at an offset derived from the run's start time and wrap around, so a run that runs out of budget does not always leave the same principals unvisited. A principal starts only with at least 20 s of budget left and not after SIGTERM; the rest are **deferred**. A failed enumeration fails the run.
2. **List** OpenBao (KV v2 `LIST`) under the principal's current prefix `iam/serviceaccount/<tenant_id>/<keycloak_client_id>/` and under the parent of every stored `openbao_path` (a re-mint that changed `keycloak_client_id` leaves older versions under the old prefix).
3. **Per version found:**
   - a live (`active`/`rotating`) row at that path → `ok`;
   - a `revoked` row whose material survived (a revoke that crashed between its delete and its commit, then committed on retry) → delete it (CUST-2); `revoked` is terminal, so no lock is needed;
   - no row at all → an orphan candidate, deleted **only under the principal's row lock**: in an RLS-scoped app-pool transaction, `LockForUpdate` the principal, re-check that still no row claims the version, then delete with a 5 s bound. TS-1 holds the same lock across its OpenBao write and its commit, so material without a row under the lock is a genuine orphan. This replaces the old "below the max committed version" heuristic and also covers a principal with no rows at all (TS-D22).
4. **Per live row whose material was not found:** an unlocked re-check drops rows revoked since the snapshot. The rest are confirmed **under the principal lock**: re-read the row, then `LIST` the row's own prefix (not `Read`, which cannot tell a missing path from an OpenBao outage and would pull key plaintext into the process). Only a row still live with no material at that point counts as `missing_material`, so a TS-2 caught between its delete and its commit no longer pages (TS-D23).

A busy lock (`55P03` → `rotation_in_flight`) or a principal gone since the snapshot is **skipped** for this run, never failed. Outcomes are counted in `iam_token_service_material_reconcile_total{result}` (`orphan_deleted`, `missing_material`, `ok`, `error`). `missing_material` and any failure make the run exit 1; `missing_material` pages (`IAMTokenServiceOrphanMaterialMissing`) and is resolved by **rotation** (regenerate, then RP-17), never by fabricating material (§6.4/CUST-3).

### 8.7 Break-glass / suspected-compromise revoke (two-halves, TS-INV-7)

The operator on-demand revoke path (TSQ-2) exists for suspected compromise of a live `active` private key. Because Keycloak is the validator, caches the JWKS, and only RP writes Keycloak (§6.1), **stopping a leaked key is a two-service operation** — a TS-2 revoke removes it from the JWKS, but Keycloak keeps trusting its cached copy until RP-17 clears the cache.

```mermaid
sequenceDiagram
    participant OP as Operator / O&M
    participant TS as Token Service
    participant OB as OpenBao
    participant RP as Realm Provisioner
    participant KC as Keycloak
    OP->>TS: TS-1 rotate {rotation_id, overlap_seconds:0}   %% mint a clean replacement first
    TS->>OB: write private key @ v+1
    TS-->>OP: 201 {version:v+1, secret (once)}
    Note over OP: discard the private key
    OP->>TS: TS-2 revoke {compromised version}             %% delete its material + mark revoked
    TS->>OB: delete material @ compromised version
    TS-->>OP: 200 {status: revoked, keycloak_invalidation: caller_responsibility}
    OP->>RP: RP-17 refresh keys (ClearServiceAccountKeysCache)
    RP->>KC: POST /admin/realms/{realm}/clear-keys-cache
    KC->>TS: GET …/jwks.json (re-fetch on next login)
    TS-->>KC: 200 {keys: [v+1 only]}
```

The **recommended order is rotate-then-revoke-then-refresh** (mint a clean replacement *before* removing the compromised key) so the principal is never left with no working credential. With `overlap_seconds=0` the compromised version is demoted with an already-expired overlap, so the JWKS stops serving it at once; the explicit TS-2 then deletes its material. Revoking first is possible too, at the cost of a brief automation outage until the replacement is issued. Either way the load-bearing step is the **RP-17 cache-clear that makes Keycloak forget the compromised key** — TS-2 is the custody/metadata half, not the auth-plane half (TS-INV-7). This operator path writes no `keys_refresh_pending` marker; only the CronJobs do (§8.8). The operator runbook is §24; the O&M/operator tooling that drives this is TSQ-2's MVP surface.

### 8.8 RP-17 refresh markers (`keys_refresh_pending`)

Under EXT-6 a revoked or superseded key keeps authenticating at Keycloak until RP-17 lands, and there is no Keycloak-side TTL. `keys_refresh_pending` (§4.2) is the durable record that a tenant is owed an RP-17 call, so a failed call, a deadline kill or a SIGKILL between a commit and its refresh is retried instead of lost (TS-D22, TS-D23). Only `cmd/rotator` and `cmd/scheduler` use it.

| Step | Who | What |
|---|---|---|
| Committed marker | `cmd/rotator` sweep | `MarkPending` in the **same transaction** as each revoke (§8.3): `intent_until = NULL`, `requested_at = GREATEST(old, clock_timestamp())` |
| Intent marker | `cmd/scheduler` | `MarkIntent` in its own transaction **before** `IssueOrRotate` (§8.2): `intent_until = now() + max(4 min, remaining run time + 40 s)`. It never downgrades an existing committed marker or an expired intent |
| Re-mark committed | `cmd/scheduler` | `MarkPending` after `IssueOrRotate` (unless the rotation was refused outright), and again after a failed RP-17. This closes the race in which a concurrent sweep cleared the intent before the rotation committed (TS-D23) |
| Clear | both | After RP-17 succeeds, `Clear(tenant, upTo)` deletes the row only if `requested_at <= upTo`, the value observed before the call, so a newer request survives for its own refresh. A failed clear is logged; it costs one harmless extra RP-17 next run |
| Retry pass | both, at the start of every run | `ListPending` (reconciler pool, oldest first, up to the batch limit) returns committed markers at once and intent markers only after `intent_until` has passed. Each gets RP-17 and, on success, a clear. The pass stops when less than one RP-17 timeout of its share (half the run's remaining budget) is left; the rest stay for the next run. A failure keeps the marker and counts as failed. If the tenant has no principal left (offboarded since), the marker is dropped instead of failing every run forever |
| Visibility | `cmd/server` | `iam_token_service_keys_refresh_pending` and `_oldest_age_seconds`, refreshed every `KEYS_REFRESH_EXPORTER_INTERVAL` (30s) over the reconciler pool; `IAMTokenServiceKeysRefreshBacklog` above 15 minutes |

RP-17 calls go through `internal/adapter/outbound/realmprovisioner`: `POST {REALM_PROVISIONER_BASE_URL}/api/v1/internal/tenants/{id}/service-account/keys/refresh` with the `iam-system` headers, a 3 s timeout per attempt, at most 2 attempts with a 300 ms backoff, retrying only an unreachable RP or `429`/`502`/`503`/`504`; any `2xx` is success. Each call is wrapped in a 10 s detached context and recorded as `platform_dependency_request_seconds{dependency="realm_provisioner",operation="refresh_keys"}`.

### 8.9 Retention prunes (rotation CronJob)

After the sweep and the reconciler, every `cmd/rotator` run prunes the three operational tables that grow without bound:

| Table | Mechanism | Retention |
|---|---|---|
| `processed_events` | `inbox.Store.Prune` for consumer `tenant_offboarding`, batches of `PRUNE_BATCH_LIMIT` (10000) until none remain | `PROCESSED_EVENTS_TTL_DAYS` (8) |
| `outbox_events` (published rows) | `outbox.Runner.PrunePublished` (the runner is never started; this process does not publish) | `OUTBOX_PRUNE_OLDER_THAN` (168h) |
| `rls_violation_log` | `prune_rls_violation_log(ttl, limit)` (`SECURITY DEFINER`, refuses a TTL under 7 days), repeated while a full batch comes back | `RLS_VIOLATION_LOG_TTL_DAYS` (30) |

Each step starts only with at least 5 s of run budget left and not after SIGTERM. A step that is not started, or is cut off by the deadline or a signal, is **deferred** (committed batches stay deleted; the in-flight one rolls back), not failed. Any other error is **failed**. The rotator exits 1 if the sweep, the retry pass, the reconciler (failure or `missing_material`) or any prune failed; deferred work alone exits 0.

---

## 9. Concurrency, Consistency, and Failure Handling

### 9.1 Optimistic concurrency

Every UPDATE carries the read `record_version`; a mismatch is a `409 optimistic_lock_conflict` (CONC-1). The `trg_touch_*` triggers bump `record_version` and `updated_at` on each UPDATE (§4.5). Issue/rotate, revoke and the offboarding cascade serialize per principal on a row lock. Each runs in one transaction that starts with `SELECT … FOR UPDATE` on the principal row (`PrincipalRepository.LockForUpdate`; offboarding uses `LockByTenant`). It then re-reads state, takes the next version from `MaxVersion` over every row (revoked included, so a version number is never reused), writes the OpenBao material, and commits. Two concurrent TS-1 calls with different `rotation_id`s therefore get consecutive versions instead of racing to the same OpenBao path, and the second caller's overlap logic sees the first's committed rotation. A caller that cannot get the lock within `PG_LOCK_TIMEOUT` (SQLSTATE `55P03`) gets `409 rotation_in_flight` (TS-D21). Under the same `rotation_id`, a concurrent call returns the first's result idempotently. The lock is held as briefly as possible: TS-1 generates its keypair before the transaction, and every OpenBao call made under the lock (TS-1's write, TS-2's delete, the reconciler's orphan delete and missing-material `LIST`) is bounded at 5 s, because every other writer for the principal gives up after `PG_LOCK_TIMEOUT` (Helm: 2 s) (TS-D22/TS-D23). The lock-timeout `409` carries the frozen `details.active_rotation_id` when an active credential exists, looked up best-effort after the failure (TS-D23).

Who takes the lock: TS-1, TS-2 and TS-1's opportunistic sweep (`LockForUpdate`); the offboarding cascade (`LockByTenant`, all of the tenant's principals); the orphan reconciler, for an orphan delete or a missing-material confirmation (§8.6). `cmd/rotator`'s sweep does **not** lock the principal: it revokes one `rotating` row with an optimistic-lock UPDATE and treats a lost race as skipped (§8.3). TS-4 does not lock either, but its `ON CONFLICT DO UPDATE` waits on a locked principal row and maps a timeout to the same `409`. `cmd/scheduler` adds `ExpectActiveVersion`, so a cadence rotation racing an operator rotation is refused (`optimistic_lock_conflict`, counted as skipped) rather than rotating twice. The JWKS route reads without locking, so key fetches never queue behind a rotation.

### 9.2 Idempotency strategy

TS-1 is idempotent per `rotation_id` — the `rotation_id` is recorded with the issued version, so a retried call returns the same version and secret **without re-generating** material (a re-generation would orphan an OpenBao entry and double-emit). TS-2 is idempotent per version (re-revoke is a no-op). TS-4 is idempotent on `(tenant_id, principal_type)`. The inbound offboarding cascade is exactly-once via platform-events' inbox over `processed_events` (`consumer='tenant_offboarding'`; claim and cascade in one transaction, TS-D19, §8.4). Produced events dedup downstream on their stable `id` (EVT-3).

**Stale `rotation_id` replay (implementation-phase refinement, TS-D13).** A `rotation_id` is keyed to the row it created for that row's entire lifetime, not just "the current in-flight rotation" — a caller may legitimately replay a `rotation_id` from a rotation later superseded by a subsequent one. While that row is still `active` or `rotating` (inside its overlap window), the replay returns the same historical secret, which is correct idempotency-key semantics. Once the row reaches `revoked` (past overlap-expiry, or a TS-2/offboarding revoke), its OpenBao material is gone; the replay is classified explicitly as `409 credential_replay_revoked` rather than surfacing a misleading `502 secret_store_unavailable` from the failed OpenBao read. A replay is also bounded in time (TS-D22): later than `ROTATION_REPLAY_WINDOW` (default 15 minutes, `0` = unlimited) after `issued_at`, it returns `409 credential_replay_expired` instead of the key, so a leaked `rotation_id` cannot be used to re-read a live private key indefinitely. Every replay outcome (`served`, `expired`, `revoked`) is logged at Info (never the key) and counted in `iam_token_service_credential_replays_total{result}` (TS-D23).

**Concurrent-revoke idempotency (implementation-phase refinement, TS-D13).** TS-2's "idempotent" guarantee must hold against a genuine concurrent race (another TS-2 call, or the §8.3 overlap-expiry sweep, revoking the exact same row between this call's read and its own write), not only against a stale read taken before such a race began. TS-2 re-reads the row under the principal lock (an already-`revoked` row is a `200`) and, as defense in depth, re-checks it after an `optimistic_lock_conflict` from a writer that does not take the lock; `cmd/rotator`'s `revokeExpiredRotating` classifies that conflict as skipped. Neither surfaces `409` or counts a sweep failure for a race that already converged correctly.

### 9.3 Failure scenarios

| Failure | Handling |
|---|---|
| OpenBao unavailable or slow on TS-1 write | `502 secret_store_unavailable` (the in-lock write gives up after 5 s); nothing committed in Postgres (material-first, then row, then commit — a material orphan with no row is overwritten by the next TS-1 or reclaimed by the reconciler under the lock, §8.6). |
| Crash after OpenBao write, before Postgres commit | The row never commits. A retry under the same `rotation_id` computes the same next version and overwrites the material; otherwise the §8.6 reconciler deletes it under the principal lock. |
| Lost TS-1 response (secret generated, caller never saw it) | Caller retries same `rotation_id` → same version + same secret returned; no double-rotation (§8.2). |
| Different `rotation_id` mid-rotation | Serialized on the principal row lock (§9.1); a caller still waiting after `PG_LOCK_TIMEOUT` gets `409 rotation_in_flight` and retries. |
| Optimistic-lock conflict | `409 optimistic_lock_conflict`; caller re-reads and retries. |
| Issue against a revoked principal | `422 principal_revoked`. |
| Offboarding event redelivered | The inbox claim on `processed_events` finds the id → duplicate, no-op. A concurrent copy blocks on the claim's row lock and then sees the duplicate, so the cascade runs once (TS-D19). If the first copy holds the lock longer than `PG_LOCK_TIMEOUT` (slow OpenBao deletes), the waiting copy fails with `55P03` instead and is redelivered after its visibility timeout, by which point it is a plain duplicate; the cascade still runs once. |
| Offboarding cascade fails mid-way (e.g. OpenBao) | The transaction rolls back with the claim; the redelivery repeats the cascade, and OpenBao `Delete` of an already-deleted path is a no-op. |
| Outbox relay lag / SNS outage | Events queue in `outbox_events`; the runner drains when SNS recovers; audit is delayed, never lost (EVT-4). |
| Concurrent revoke race (another TS-2 call, or the §8.3 sweep, revokes the same row first) | The loser's optimistic-lock write re-checks the row and returns the already-achieved idempotent outcome, not `409` (TS-D13, §9.2). |
| TS-1 `rotation_id` replay against a since-revoked version | `409 credential_replay_revoked`, not a misleading `502` from a failed OpenBao read (TS-D13, §9.2). |
| TS-1 `rotation_id` replay after `ROTATION_REPLAY_WINDOW` | `409 credential_replay_expired`; the key is not returned again; logged and counted (TS-D22/TS-D23). |
| RP-17 fails after a sweep revoke or a scheduler rotation | The commit stands; the tenant's `keys_refresh_pending` marker stays; the outcome is `failed` and the Job exits 1; the next run's retry pass repeats RP-17 until it succeeds (§8.8). |
| CronJob Pod SIGTERMed (drain, deadline) | No new tenant, principal or prune step starts; the in-flight tenant finishes on a detached context, including its RP-17 (grace period 45 s ≥ 20 s + 10 s); the rest is deferred to the next run. |
| CronJob Pod SIGKILLed after a commit | The marker written with the commit (sweep) or before it (scheduler intent) survives; the next run's retry pass calls RP-17 (§8.8). |
| Orphan reconciler finds the principal locked | `skipped` for this run, never `failed`; a TS-1/TS-2 is in flight (§8.6). |
| A TS-2 is between its OpenBao delete and its commit while the reconciler runs | The missing-material check is confirmed under the principal lock, so it sees either the committed revoke or the material; no false page (TS-D23). |
| JWKS: active key (or every key) unreadable | `503 jwks_keys_unavailable`; Keycloak keeps its cached keys; `jwks_key_errors_total` pages. Only rotating keys unreadable: `200` without them, still counted (§5.4). |
| `TenantMembershipsPurged` with a missing or invalid envelope id | Sent straight to the DLQ (`invalid_envelope_id`), never acked; pages, since the tenant's erasure has not run (§7.1). |

### 9.4 Consistency guarantees

Postgres is the consistency authority for credential *state*; OpenBao is authoritative for *material*. The two are bound by the write ordering in §9.3 and reconciled by rotation, never by reading a stale secret (§13). Within Postgres, all writes are transactional and RLS-scoped; the outbox binds event emission to the state write (§7.4).

### 9.5 Operational invariants

| # | Invariant |
|---|---|
| CONC-1 | Optimistic locking on `record_version` for both tenant-scoped tables; a stale write is rejected, never silently overwritten. |
| CONC-2 | At most one `active` credential per principal at all times (`uq_sac_one_active`), even under concurrent rotation (§9.1). |
| CONC-3 | An emitted event always corresponds to a committed state change (outbox, §7.4); there is no committed-partial state a consumer can observe. |
| CONC-4 | OpenBao material is written only by TS-1 under the principal row lock. Deletes made without that lock touch only material of rows that are already `rotating` and expired (`cmd/rotator`'s sweep) or already `revoked` (the §8.6 reconciler); every other delete holds the lock (TS-D21..TS-D23). |

---

## 10. Security

### 10.1 Tenant isolation — three layers

Layer 1 — the reserved `iam-system` principal + target `x-tenant-id` on every internal route (RLS-5), backed by the network controls in §10.2 (the header alone is forgeable). Layer 2 — `FORCE` RLS + default-deny on both tenant-scoped tables, keyed on the transaction-local `app.tenant_id` GUC (RLS-1/RLS-6, §4.3). Layer 3 — the composite FK `(principal_id, tenant_id)` ties every credential to a principal in the same tenant, so a cross-tenant credential row is structurally impossible.

### 10.2 Network isolation

**Why the network layer carries the authentication.** The TS routes authenticate only by the `x-user-id` header, and any pod that can reach the server's HTTP port can set it to the reserved system principal. Two namespaces must reach that port for a single route each: Keycloak (the JWKS fetch, EXT-6) and, optionally, the Workflow Service (TS-6, TS-D17). NetworkPolicy works per port, not per path, so on its own it would let a pod in Keycloak's namespace call TS-1/TS-2/TS-4 as the system principal. The Istio `AuthorizationPolicy` is what closes that gap (TS-D21/TS-D22).

**Istio `AuthorizationPolicy`** (`templates/authorizationpolicy.yaml`, `authorizationPolicy.*`). An `ALLOW` policy on the server pods:

| Source | Allowed |
|---|---|
| `meshNamespaces` (Realm Provisioner, O&M, operator gateway) | every route |
| `keycloakNamespaces` | `GET */service-accounts/platform-automation/jwks.json` only |
| `workflowNamespaces` (optional) | `GET */service-accounts/platform-automation` (TS-6) only |
| anyone (kubelet, Prometheus) | `/healthz`, `/readyz`, `/metrics` |

Outside local/dev/test the chart refuses to render unless `authorizationPolicy.enabled` is true with `meshNamespaces` and `keycloakNamespaces` set (`templates/validate.yaml`), whether or not NetworkPolicy is on. `source.namespaces` matches only mTLS-authenticated peers, so every caller, Keycloak included, must be in the mesh. With the policy enabled, server and consumer pods are force-injected, by default with native sidecars (`authorizationPolicy.nativeSidecar`) so Envoy outlives the app's drain; with classic sidecars the chart sets `terminationDrainDuration` to the grace period minus 10 s (TS-D23).

**`PeerAuthentication`** (`authorizationPolicy.strictMTLS`, default true): STRICT mTLS on the server pods, so a pod that never joined the mesh cannot bypass the policy over plaintext. The metrics port stays PERMISSIVE for a non-mesh Prometheus; NetworkPolicy already limits it to the monitoring namespace.

**NetworkPolicy** (`templates/networkpolicy.yaml`, one policy per workload plus one for the migrate hook Job). The render fails if `networkPolicy.enabled` is true without:

- `ingressNamespaceSelector` (the mesh namespaces) — an empty selector would match every namespace (TS-D13);
- `keycloakNamespaceSelector` — Keycloak's own namespace, admitted to the server port for the JWKS fetch (TS-D15);
- `monitoringNamespaceSelector` — the only source allowed on the metrics port;
- `egress.postgresCIDRs` — RDS is off-cluster, so a namespace selector never matches it.

`workflowNamespaceSelector` optionally admits the Workflow Service. Egress is limited to DNS, Postgres (`postgresPort` for PgBouncer and `postgresDirectPort` for the direct migrator/reconciler DSN), OpenBao, the OTel collector (each also to in-cluster namespaces, plus optional CIDRs `egress.{dnsCIDRs,openbaoCIDRs,otelCIDRs}`), HTTPS 443 (AWS APIs), istiod (`istiodNamespaceSelector`, port 15012) when `authorizationPolicy.enabled` or `cronjobs.istioInject` is on, and the Realm Provisioner's port for `cmd/rotator` and `cmd/scheduler` (RP-17). The CronJobs and the consumer accept no API ingress; the migrate Job accepts none at all and egresses only to DNS, Postgres and OTel.

**CronJobs and the migrate Job** are not mesh-injected by default (`cronjobs.istioInject: false`; the migrate Job always sets `sidecar.istio.io/inject: "false"`), since a classic sidecar keeps a Job from completing. With `cronjobs.istioInject: true` they get native sidecars.

The `/api/v1/internal/*` prefix is the only application surface; `/healthz`/`/readyz`/`/asyncapi`/`/swagger` are infra/docs routes (§12.1 gating).

### 10.3 Input validation

`rotation_id` must be a UUID; `overlap_seconds` is clamped to `[0, 900]`; `principal_sub` must be a UUID; `keycloak_client_id` is validated by `domain.ValidPlatformAutomationClientID` — the frozen base name `platform-automation` or that name suffixed with the target tenant's UUID (§25, rev 1.1). Path `:id`/`:principal_id`/`:version` are typed-parsed; a malformed identifier is `400` before any DB checkout. Request bodies are capped at 1 MiB, a POST body must be `application/json` (`415`), and identity headers must each appear once as a UUID (§5.1).

### 10.4 Authorization rules

Internal-only routes; the reserved system principal is accepted only on `/api/v1/internal/*` (RLS-5, AUTH-5 parity). Which workloads may present it, and on which routes, is enforced by the Istio `AuthorizationPolicy` (§10.2). The automation principal itself holds no roles and is a non-member (O&M AUTH-9) — this service issues credentials and makes no authorization decision (TS-INV-4). No route accepts a tenant-facing principal (§5.2).

### 10.5 Secret handling (the crux)

- **OpenBao is the only home for plaintext.** The PEM private key is written to KV v2 under the deterministic path `iam/serviceaccount/<tenant_id>/<keycloak_client_id>/v<version>`. Postgres stores only the path.
- **OpenBao authentication.** Kubernetes auth only; there is no static token field (TS-CONFIG-3). Each workload logs in with its own ServiceAccount's **projected token with audience `openbao`** (`openbao.tokenAudience`, mounted at `OPENBAO_K8S_TOKEN_PATH`), never the default API-server-audience token. The OpenBao role (`deploy/openbao/role.tf.example`) binds the four workload ServiceAccounts with `audience = "openbao"` to a path-scoped policy (`deploy/openbao/policy.hcl`). Login is single-flight and detached from the caller's context; a token is refreshed `min(30 s, lease/2)` before its lease ends; a `403` triggers one re-login and retry; each call uses a cloned client (`kvFor`) so concurrent requests never race on the token. A private CA is supported through `openbao.caCertSecret` (`BAO_CACERT`) (TS-D21/TS-D22).
- **Per-workload ServiceAccounts and IRSA.** With `serviceAccount.perWorkload: true` (default) the server, consumer, rotator and scheduler each run under their own ServiceAccount, and only server and consumer carry an IRSA role: `deploy/iam/policy-server.json` (SNS publish, Glue read) and `deploy/iam/policy-consumer.json` (SQS consume, DLQ send). Rotator, scheduler and the migrate Job get no AWS permissions. The migrate Job's ServiceAccount mounts no token. The chart refuses `workloadAnnotations` with `perWorkload: false` (except the `migrate` key), since one shared ServiceAccount cannot carry per-workload roles (TS-D21/TS-D22).
- **Returned once.** TS-1 returns the private key in a single response body over mesh mTLS, with `Cache-Control: no-store`; it is never cached or logged (CI secret-logging gate, §3.2), and TS-3 is metadata-only. A `rotation_id` replay can return it again only within `ROTATION_REPLAY_WINDOW` (15m), and every replay is audit-logged and counted (§9.2).
- **This service never writes Keycloak** (TS-INV-1) and holds no Keycloak Admin credential; only the Realm Provisioner writes the client and clears its key cache (RP-17). Keycloak receives only public keys, through the JWKS route.
- **Rotation bounds exposure.** A leaked key is cut off at the next rotation plus RP-17; `overlap_seconds` bounds how long a superseded key stays in the JWKS (§6.2).
- **Database credentials.** The DSNs come from a Kubernetes Secret (`database.existingSecretName`, or one the chart renders from `secretValues`). With `migrations.enabled` (the default) only the migrate hook Job receives the migrator DSN and every workload runs with `RUN_MIGRATIONS=false`; with it off, every workload gets the migrator DSN and migrates at startup (§13.4).

### 10.6 Audit

Every issue/rotate/revoke is audit-logged locally **and** evented (§7.4) through the outbox; audit visibility does not depend on the bus being live (TS-INV-5/EVT-4). No audit record carries a secret. TS-1 `rotation_id` replays are additionally logged at Info with tenant, principal, version and result (never the key) and counted in `credential_replays_total{result}` (TS-D23). The Audit Log Service consumes `iam.serviceaccount.events`; entry-type vocabulary is this service's produce contract, to be reconciled with the Audit Log LLD when it exists.

---

## 11. Observability

### 11.1 SLOs

This service is not on any latency-critical path — the hot token-issuance path is Keycloak's, not this service's (§6.1). Its SLOs are availability-oriented: internal API availability **99.9% monthly** (per-service, HLD §3.4); TS-1 issue/rotate p99 **≤ 500 ms** excluding the OpenBao round-trip; TS-3 read p99 **≤ 100 ms**. The rotation-overlap sweep is availability-first, not latency-bound — its objective is that no `rotating` version outlives `expires_at` by more than one cron interval.

These targets are implemented as recording rules + multi-window burn-rate alerts in `deploy/monitoring/slo-rules.yml` (rendered by the chart as `templates/prometheusrule-slo.yaml`; shape follows iam-org-membership's), over `platform_http_request_duration_seconds` / `platform_http_requests_total` (`by (domain, service, environment)`):

| SLO | Target | Source | Alerts |
|---|---|---|---|
| SLO-1 | TS-1 issue/rotate: 99% ≤ 500 ms (`le="0.5"`) | this section. The HTTP-layer SLI **includes** the OpenBao write the target excludes, so it is conservative — a burn explained by OpenBao alone is `IAMTokenServiceOpenBaoCallLatencyHigh`, not an SLO-1 breach | `IAMTokenServiceSLO1_TS1LatencyFastBurn` / `…SlowBurn` |
| SLO-2 | 99.9% of mutating requests (TS-1, TS-2, TS-4) non-5xx | this section / HLD §3.4 | `IAMTokenServiceSLO2_WriteErrorRateFastBurn` |
| SLO-3 | 99% of §8.3 sweep runs without an error result | operational **proxy** for the sweep objective above (no metric measures the age of the oldest expired `rotating` row); the 99% target is the rules file's, not this LLD's. Each run is its own short-lived pod whose counter never changes while scrapeable, so runs are counted with `max_over_time` per pod series, not `rate()` (TS-D22) | `IAMTokenServiceSLO3_SweepSuccessRateLow` |
| SLO-4 | TS-3 read: 99% ≤ 100 ms (`le="0.1"`, route `GET /api/v1/internal/tenants/:id/service-accounts/:principal_id`) | this section | `IAMTokenServiceSLO4_TS3LatencyFastBurn` / `…SlowBurn` |

The `platform_http_*` metrics are still `proposed` in the Platform Observability Registry, so the alerts built on them carry `metric_status: proposed`.

### 11.2 Metrics

**Enterprise Platform Observability Standard adoption (TS-D13, implementation-phase).** Metrics follow the platform-wide three-tier taxonomy — Tier 1 `platform_*` (semantics shared across every domain), Tier 2 `iam_*` (shared across IAM services only), Tier 3 `iam_token_service_*` (this service only, §25 frozen prefix). Tier 1/Tier 2 labels (`domain`/`service`/`environment`, `service`/`environment` respectively) are injected **centrally** by `metrics.Register(environment)` — instrumentation call sites never set them, so they cannot be omitted or misspelled. A CI gate (`make gates` → `metrics-taxonomy`) enforces naming/namespace compliance (prefix classification, counter `_total`/histogram `_seconds` suffixes).

Metric registration order is the same in every binary (and in `test/unit/metricsstandard`, which asserts it): `InitTracingWithConfig` → `ObservabilityMiddlewares` → `metrics.InitLibraryMetrics` (`events.InitMetrics`, `pgmetrics.InitWithIdentity`) → `metrics.Register` (adopts platform-events' already-registered Tier 1 collectors via `registerShared`, so both write one series) → pools. `/metrics` is `gincommon.MetricsHandler()` in all four binaries. `docs/observability/` documents the implementation (`README.md`), the generated inventory (`metric-registry.md`, `make metrics-inventory`), one runbook per alert (`runbooks.md`) and the rename/removal history (`migration.md`); `make metrics-lint` checks a real scrape and every reference in `deploy/`/`docs/` (§3.2).

Three Tier-3 metrics have a **registry-proposed** Tier-1/Tier-2 equivalent (full submission — semantic definition, labels, allowed values, aggregation expectations — in `docs/observability-registry-proposals.md`); neither side was renamed or removed, and dashboards/alerts remain on the Tier-3 name until a governance reviewer ratifies the proposal:

| Tier 3 (unchanged) | Tier 1/2 equivalent |
|---|---|
| `iam_token_service_openbao_call_duration_seconds{op}` | `platform_dependency_request_seconds{dependency="openbao",operation,outcome}` (Tier 1, shared with platform-events) — **dual-emitted** by the service's OpenBao decorator |
| `iam_token_service_processed_events_duplicates_total{consumer}` | `platform_duplicate_messages_total{queue,event_type}` (Tier 1) — counted **only** by platform-events' inbox (`Store.Process`); the service records just the Tier 3 counter, which stays the authoritative duplicate signal (TS-D19) |
| `iam_token_service_offboarding_cascade_total{result}` | `iam_offboarding_cascade_total{outcome}` (Tier 2 — proposed, **not emitted** until ratified) |

**Tier 2 emitted:** `iam_rls_violations_total{violation_type}` (`missing_or_invalid_guc` | `cross_tenant_access`; `metrics.AddRLSViolations` maps an unknown type to `other`) — built from the registry entry, the same metric iam-org-membership / iam-user-profile / iam-audit-log emit (TS-D18). `cmd/server`'s `runRLSViolationExporter` (`cmd/server/exporters.go`) reads new `rls_violation_log` rows over the reconciler pool every `RLS_VIOLATION_EXPORTER_INTERVAL` (default 1m) with an id cursor — each row counted once per pod; a restart re-counts at most one interval. Every server replica counts the same rows, so queries use `max`, not `sum`. Rows are 1%-sampled at the source (§4.3).

Full Tier-3 set (prefix `iam_token_service_*`):

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `iam_token_service_credentials_issued_total` | counter | `op` (`issue`/`rotate`/`revoke`) | Credential state transitions |
| `iam_token_service_rotation_overlap_active` | gauge | — | Live `rotating` versions (should trend to zero between rotations) |
| `iam_token_service_openbao_call_duration_seconds` | histogram | `op` (`write`/`delete`) | OpenBao KV latency (legacy; `read`/`list` are recorded only on the Tier-1 `platform_dependency_request_seconds{dependency="openbao"}`) |
| `iam_token_service_offboarding_cascade_total` | counter | `result` (`ok`/`error`) | Offboarding-cascade outcomes |
| `iam_token_service_rotation_sweep_total` | counter | `result` (`ok`/`error`) | One increment per `cmd/rotator` run: `error` if the sweep or the retry pass failed anything (§8.3, §8.8) |
| `iam_token_service_material_reconcile_total` | counter | `result` (`orphan_deleted`/`missing_material`/`ok`/`error`) | Orphaned-material reconciler outcomes (§8.6); `missing_material` is a page-worthy divergence. Skipped/deferred principals are logged, not counted |
| `iam_token_service_processed_events_duplicates_total` | counter | `consumer` | Deduped SQS redeliveries (§9.2) |
| `iam_token_service_unknown_event_acknowledged_total` | counter | `consumer`, `event_type` | Forward-compat acks of unrecognized event types |
| `iam_token_service_consumed_schema_violations_total` | counter | `consumer`, `event_type` | Inbound payloads that failed the embedded consumed schema and were sent straight to the DLQ (rev 1.4, §7.1) — pages |
| `iam_token_service_cadence_rotation_total` | counter | `result` (`rotated`/`skipped`/`failed`) | `cmd/scheduler` automatic rotations (§8.2, TS-D14); `failed` pages |
| `iam_token_service_jwks_key_errors_total` | counter | — | Live credentials the JWKS route could not serve (TS-D15) — pages |
| `iam_token_service_jwks_rate_limited_total` | counter | — | JWKS requests answered 429 by any limiter (TS-D21) |
| `iam_token_service_jwks_rate_limited_by_bucket_total` | counter | `bucket` (`tenant`/`global`/`unknown`) | Which limiter refused; the `unknown` bucket only absorbs tenants with no live credential, so alerts exclude it (TS-D23) |
| `iam_token_service_credential_replays_total` | counter | `result` (`served`/`expired`/`revoked`) | TS-1 `rotation_id` replays — an audit signal, since a served replay returns the private key again (TS-D23) |
| `iam_token_service_keys_refresh_pending` | gauge | — | Rows in `keys_refresh_pending` (RP-17 refreshes owed, intents included), exported by every server replica (TS-D23) |
| `iam_token_service_keys_refresh_oldest_age_seconds` | gauge | — | Age of the oldest owed RP-17 refresh; alerts above 15m (TS-D23) |
| `iam_token_service_consumer_dlq_rejects_total` | counter | `reason` (`schema_violation`/`invalid_envelope_id`/`other`) | Offboarding messages the consumer sent straight to the DLQ; `invalid_envelope_id` pages (GDPR erasure pending, TS-D23) |

The CronJob counters (`rotation_sweep_total`, `material_reconcile_total`, `cadence_rotation_total`) live in one short-lived pod per run, scraped by the PodMonitor during the run's `*_METRICS_SCRAPE_GRACE` window (§13.4); alerts read them with `max_over_time`, never `rate()`. `docs/observability/metric-registry.md` (generated, `make metrics-inventory`) is the authoritative list.

`platform_dependency_request_seconds` (Tier 1, `dependency`, `operation`, `outcome`) is also recorded by the service for OpenBao (`operation` = `write`/`read`/`delete`/`list`, every binary) and for the Realm Provisioner (`dependency="realm_provisioner"`, `operation="refresh_keys"`, `cmd/rotator` and `cmd/scheduler`, TS-D22).

Library-emitted Tier 1 series this service alerts on (`platform-events`, not this service directly): `platform_outbox_pending_events` (unrelayed outbox rows, bus health), `platform_dlq_messages_total`, and — on `cmd/consumer`, sampled every `SQS_QUEUE_DEPTH_INTERVAL` (default 60s, `0s` disables; `sqs:GetQueueAttributes` already granted) — `platform_queue_depth` / `platform_dlq_depth` for `tenant-lifecycle-tokensvc-q` and its DLQ.

Additionally, `platform-pgcommon`'s Tier 1 pool metrics (`platform_db_pool_connections{state=idle|acquired|constructing}`, `platform_db_pool_max_connections`, `platform_db_pool_empty_acquires_total`, plus `platform_db_queries_total` / `platform_db_query_duration_seconds` etc.) are registered automatically by `pgcommon.NewPool` for every pool this service opens, because `metrics.InitLibraryMetrics` (`pgmetrics.InitWithIdentity`) runs before the first pool is created. The `pool` label distinguishes the app pool (`default`, `PG_POOL_NAME`) from the BYPASSRLS reconciler pool (`reconciler`, `SystemPoolConfig`). §25 is unaffected.

### 11.3 Tracing

OpenTelemetry via `platform-gincommon` only: every binary calls `gincommon.InitTracingWithConfig` with the metrics identity (`APP_NAME` is the trace service name; `service.namespace` and `deployment.environment.name` on the resource; `OTEL_EXPORTER_OTLP_ENDPOINT=none` disables export but keeps trace IDs, gincommon v1.6.0). DB spans come from `pgcommon.Config.Tracer = gincommon.NewSpanTracer(...)`, job/consumer/exporter spans from `gincommon.NewTracer`, and `trace_id` reaches logs via `gincommon.SpanTraceID`. The request timeout is `gincommon.Config.RequestTimeout` (30s, server and consumer health router), so a timed-out request is recorded as the 503 the client got. On the internal API each use-case starts a span (`credential.issue_rotate`, `credential.revoke`, attrs `tenant_id`, `principal_id`, `version`, `op`) and propagates `trace_id` onto the produced envelope, so a rotation is traceable from the TS-1 call to the Audit projection.

### 11.4 Structured logs

Zap JSON via `platform-gincommon`'s `logger.NewLogger` (`port.Logger`; `LOG_LEVEL`, `LOG_SAMPLING`). Every operation logs `tenant_id`, `principal_id`, `version`, `op`, `result`, and OpenBao call outcomes. **No credential field is ever a log attribute** — the CI secret-logging gate (§3.2) fails the build if a credential field name, a PEM/private-key name or a `-----BEGIN` literal reaches a log, `fmt`, error-constructor or span-attribute sink.

### 11.5 Dashboards and alerts

Grafana dashboard: credential-transition rate by `op`, `rotation_overlap_active`, OpenBao call latency/error rate, offboarding-cascade / rotation-sweep / material-reconcile / cadence outcomes, `keys_refresh_pending`, JWKS 429s by bucket, and `platform_outbox_pending_events`.

Alerts live in `deploy/monitoring/app-alerts.yml`, kept in sync by hand with the chart's `templates/prometheusrule.yaml` (the file header says so); SLO burn-rate alerts are in §11.1. Every alert has a runbook in `docs/observability/runbooks.md`. Alerts built on kube-state-metrics or `absent()` carry static `domain`/`service`/`environment` labels, which the chart renders from `appEnv` (TS-D23). CronJob counters are read with `max_over_time` (§11.2).

| Area | Alert | Fires on | Severity |
|---|---|---|---|
| Availability | `IAMTokenServiceServerDown` / `…ConsumerDown` | target down 2m | critical / warning |
| | `IAMTokenServiceServerReplicasMissing` | fewer healthy server replicas than desired, 5m | warning |
| | `IAMTokenServiceHighErrorRate` / `…HighLatency` | HTTP 5xx ratio / p99 latency | warning |
| CronJobs | `IAMTokenServiceRotatorNotRunning` / `…SchedulerNotRunning` | no successful run in 15m (kube-state-metrics) | warning |
| | `IAMTokenServiceCronJobRunFailed` | the latest run of rotator or scheduler failed, nothing running, 2m (TS-D23) | warning |
| | `IAMTokenServiceRotationSweepFailures` | a rotator run recorded `rotation_sweep_total{result="error"}` in 1h | warning |
| | `IAMTokenServiceCadenceRotationFailures` | `cadence_rotation_total{result="failed"}` in 1h | critical |
| | `IAMTokenServiceOrphanMaterialMissing` | `material_reconcile_total{result="missing_material"}` in 1h (§8.6) | critical |
| | `IAMTokenServiceRotationOverlapStuck` | `rotation_overlap_active > 0` for 1h | warning |
| | `IAMTokenServiceKeysRefreshBacklog` | `keys_refresh_oldest_age_seconds > 900` for 5m — RP-17 still failing (§8.8) | warning |
| OpenBao | `IAMTokenServiceOpenBaoCallLatencyHigh` | OpenBao call latency | warning |
| | `IAMTokenServiceOpenBaoErrors` | > 5% of OpenBao calls fail over 10m, all binaries (TS-D23) | warning |
| JWKS | `IAMTokenServiceJWKSKeyErrors` | any `jwks_key_errors_total` increase in 15m | critical |
| | `IAMTokenServiceJWKSRateLimited` | 429s from the `tenant` or `global` bucket; the `unknown` bucket is excluded (TS-D23) | warning |
| Offboarding | `IAMTokenServiceOffboardingCascadeFailures` | `offboarding_cascade_total{result="error"}` in 15m | warning |
| | `IAMTokenServiceConsumedSchemaViolation` | any consumed-schema violation in 15m — the cascade did not run (§7.1) | critical |
| | `IAMTokenServiceOffboardingInvalidEnvelopeRejected` | any `consumer_dlq_rejects_total{reason="invalid_envelope_id"}` in 15m — GDPR erasure pending (TS-D23) | critical |
| | `IAMTokenServiceOffboardingQueueStalled` / `…OffboardingDLQBacklog` | `platform_queue_depth > 0` 15m / `platform_dlq_depth > 0` 30m (`metric_status: proposed`) | warning |
| | `IAMTokenServiceOffboardingQueueBacklogGrowing` | SQS oldest-message age (dormant until a CloudWatch exporter exists) | warning |
| | `IAMTokenServiceSQSReceiveErrors` | SQS receive errors | warning |
| Outbox / DB | `IAMTokenServiceOutboxStuck` | newly dead-lettered outbox events (`platform_dlq_messages_total{operation="outbox_publish",reason="max_attempts"}`), 15m | warning |
| | `IAMTokenServiceOutboxBacklogGrowing` | `platform_outbox_pending_events` growing, 15m — the earlier warning stage | warning |
| | `IAMTokenServicePostgresPoolExhaustion` | `platform_db_pool_empty_acquires_total` growth, per `pool`, 5m | warning |
| Tenant isolation | `IAMTokenServiceRLSCrossTenantAccess` | `iam_rls_violations_total{violation_type="cross_tenant_access"}` increase in 5m, no `for` (TS-D18) | critical |
| | `IAMTokenServiceRLSMissingGUC` | `missing_or_invalid_guc` over 15m, `for: 10m` | warning |

`deploy/monitoring/schema-registry-alerts.yml` covers the Glue schema registry separately. The RLS log is 1%-sampled, so one sample means more occurred; `rls_violation_log.query_text` names the path (read as `serviceaccount_reconciler`/`admin_readonly`).

---

## 12. Configuration

```yaml
# deploy/helm/values.yaml (excerpt) — every value maps to an env var a binary
# or a shared library actually reads (audited 2026-10-05, TS-D20; extended TS-D21..TS-D23)
database:
  pgBouncerMode: true                 # PG_BOUNCER_MODE
  existingSecretName: ""              # DATABASE_URL, MIGRATION_DATABASE_URL, RECONCILER_DATABASE_URL
  pool: { maxConns: 10, minConns: 0, slowQueryThreshold: 200ms, lockTimeout: 2s,
          statementTimeout: 5s, jobStatementTimeout: 60s }   # PG_* (platform-pgcommon)
openbao: { addr: <OPENBAO_ADDR>, authRole: iam-token-service, kvMount: iam,
           tokenAudience: openbao, caCertSecret: "" }   # Kubernetes auth (§10.5); OPENBAO_K8S_TOKEN_PATH, BAO_CACERT
rotation: { defaultOverlapSeconds: 300, defaultCadenceDays: 90, replayWindow: 15m }  # clamped [0,900] (§6.2); ROTATION_REPLAY_WINDOW
jwks: { rateLimitRPS: 20, rateLimitBurst: 40, perTenantRPS: 5, perTenantBurst: 10,
        unknownTenantRPS: 2, unknownTenantBurst: 5, knownTenantsRefresh: 15s }       # JWKS_* (§5.4)
rotator:   { schedule: "*/5 * * * *", activeDeadlineSeconds: 240, runTimeout: 180s, metricsScrapeGrace: 15s,
             batchLimit: 500, pruneBatchLimit: 10000, outboxPruneOlderThan: 168h, terminationGracePeriodSeconds: 45 }
scheduler: { schedule: "*/5 * * * *", activeDeadlineSeconds: 240, runTimeout: 180s, metricsScrapeGrace: 15s,
             batchLimit: 500, terminationGracePeriodSeconds: 45 }   # schedule is a Helm cron, not an env var
server:    { replicaCount: 2, terminationGracePeriodSeconds: 80 }
consumer:  { replicaCount: 2, terminationGracePeriodSeconds: 60 }
migrations: { enabled: true, backoffLimit: 1, activeDeadlineSeconds: 300 }   # pre-install/pre-upgrade hook Job (§13.4)
shutdown:  { drainDelay: 5s }                     # SHUTDOWN_DRAIN_DELAY
cronjobs:  { istioInject: false }
realmProvisioner: { baseUrl: "http://iam-realm-provisioner.iam.svc.cluster.local:8080" }  # REALM_PROVISIONER_BASE_URL (RP-17)
sqs: { queueUrl: <SQS_QUEUE_URL>, concurrency: 2, maxMessages: 10, waitSeconds: 20,
       visibilityTimeoutSeconds: 60, handlerTimeout: 45s, drainTimeout: 15s, queueDepthInterval: 60s,
       retryBackoff: 60s, maxRetryBackoff: 15m }
events: { glueRegistryName: iam-serviceaccount-events, topicArn: <SNS_TOPIC_ARN>, awsRegion: ap-south-1 }
outbox: { pollInterval: 500ms, batchSize: 50, maxAttempts: 5, drainTimeout: 30s, publishConcurrency: 4, publishTimeout: 10s, startupJitter: 2s, claimLeaseDuration: 10m }
exporters: { rotationOverlapInterval: 30s, rlsViolationInterval: 1m, keysRefreshInterval: 30s }
processedEvents: { ttlDays: 8 }
rlsViolationLog: { ttlDays: 30 }
otel: { exporterEndpoint: "otel-collector.observability.svc.cluster.local:4317", insecure: false, samplerRatio: "0.1", propagatorBaggage: false, ignoreRemoteParentSampled: false }
docs: { enabled: false, authEnabled: false }      # DOCS_ENABLED; DOCS_AUTH_TOKEN from the Secret
serviceAccount: { create: true, perWorkload: true, workloadAnnotations: {}, annotations: {} }   # IRSA per workload (§10.5)
networkPolicy: { enabled: true, ingressNamespaceSelector: {}, keycloakNamespaceSelector: {}, monitoringNamespaceSelector: {},
                 postgresPort: 5432, postgresDirectPort: 5432, egress: { postgresCIDRs: [], openbaoCIDRs: [], ... } }  # §10.2
authorizationPolicy: { enabled: false, meshNamespaces: [], keycloakNamespaces: [], workflowNamespaces: [],
                       strictMTLS: true, nativeSidecar: true }   # required outside local/dev/test (§10.2)
image: { digest: "" }                             # deploy by digest (§13.5)
```

Key env vars (non-secret in Helm `env`; OpenBao access via Kubernetes auth, not a static secret). Names are the shared libraries' canonical ones; the former service aliases (`SNS_TOPIC_SERVICEACCOUNT_ARN`, `SQS_OFFBOARDING_QUEUE_URL`, `SQS_OFFBOARDING_CONCURRENCY`) and `OTEL_SERVICE_NAME` are no longer read (TS-D20):

| Var | Read by | Meaning |
|---|---|---|
| `APP_ENV`, `APP_NAME`, `OBSERVABILITY_DOMAIN`, `BUILD_VERSION` | every binary + platform-gincommon | environment and observability identity; `APP_NAME` is also the trace service name |
| `APP_PORT`, `METRICS_PORT` | server, consumer (API/health); every binary (`/metrics`) | ports |
| `LOG_LEVEL`, `LOG_SAMPLING` | platform-gincommon logger | log level and zap sampling |
| `OTEL_EXPORTER_OTLP_ENDPOINT` (`none` disables export), `OTEL_EXPORTER_OTLP_INSECURE`, `OTEL_TRACES_SAMPLER_RATIO`, `OTEL_PROPAGATOR_BAGGAGE`, `OTEL_TRACES_IGNORE_REMOTE_PARENT_SAMPLED` | platform-gincommon tracing | OTLP export and sampling |
| `DATABASE_URL` (or `PG_HOST/PORT/USER/PASSWORD/DBNAME/SSLMODE`), `PG_BOUNCER_MODE`, `PG_MAX_CONNS`, `PG_MIN_CONNS`, `PG_SLOW_QUERY_THRESHOLD` | platform-pgcommon | app pool |
| `PG_STATEMENT_TIMEOUT`, `PG_LOCK_TIMEOUT` | platform-pgcommon | per-transaction `SET LOCAL` timeouts on the app and reconciler pools; never applied to the migration DSN. 5s for server/consumer, 60s for the CronJobs |
| `MIGRATION_DATABASE_URL`, `RECONCILER_DATABASE_URL` | every binary | direct DSN for migrations (bypasses PgBouncer); `serviceaccount_reconciler` (BYPASSRLS, SELECT-only) pool |
| `OPENBAO_ADDR`, `OPENBAO_ROLE`, `OPENBAO_KV_MOUNT` | every binary | OpenBao KV v2 custody (§6.3, §10.5); `OPENBAO_ADDR` required outside dev |
| `ROTATION_DEFAULT_OVERLAP_SECONDS` | server (TS-1 default when `overlap_seconds` is omitted), scheduler | startup fails outside `[0, 900]` (TS-CONFIG-4) |
| `ROTATION_DEFAULT_CADENCE_DAYS` | server, scheduler | default rotation cadence (TSQ-2) |
| `REALM_PROVISIONER_BASE_URL` | rotator, scheduler | RP-17 relay target; required outside dev |
| `ROTATOR_RUN_TIMEOUT`, `ROTATOR_METRICS_SCRAPE_GRACE`, `SCHEDULER_RUN_TIMEOUT`, `SCHEDULER_METRICS_SCRAPE_GRACE` | rotator / scheduler | run budget and `/metrics` window; their sum stays under `activeDeadlineSeconds` |
| `PROCESSED_EVENTS_TTL_DAYS`, `RLS_VIOLATION_LOG_TTL_DAYS`, `PRUNE_BATCH_LIMIT`, `OUTBOX_PRUNE_OLDER_THAN` | rotator | retention prunes |
| `ROTATION_OVERLAP_GAUGE_INTERVAL`, `RLS_VIOLATION_EXPORTER_INTERVAL` | server | DB-state exporters |
| `JWKS_RATE_LIMIT_RPS`, `JWKS_RATE_LIMIT_BURST` | server | JWKS route rate limit (§5.4, EXT-6, TS-D15); defaults 20/40 |
| `JWKS_RATE_LIMIT_PER_TENANT_RPS`/`_BURST`, `JWKS_RATE_LIMIT_UNKNOWN_TENANT_RPS`/`_BURST` | server | Per-tenant (5/10) and unknown-tenant (2/5) JWKS buckets (TS-D21, TS-D22) |
| `JWKS_KNOWN_TENANTS_REFRESH` | server | Refresh interval of the DB-sourced known-tenant set (default 15s, TS-D23) |
| `KEYS_REFRESH_EXPORTER_INTERVAL` | server | `keys_refresh_pending` gauge refresh (default 30s, TS-D23) |
| `ROTATION_REPLAY_WINDOW` | server | TS-1 replay window (default 15m, `0` = unlimited, TS-D22) |
| `SHUTDOWN_DRAIN_DELAY` | server, consumer | Readiness-off wait before listeners close (default 5s, TS-D21) |
| `ROTATOR_BATCH_LIMIT`, `SCHEDULER_BATCH_LIMIT` | rotator, scheduler | Max rows per run (default 500, TS-D21) |
| `RUN_MIGRATIONS` | every binary | `false` skips startup migrations (Helm sets it on every workload when `migrations.enabled`, TS-D21) |
| `MIGRATE_ONLY` | server | `true` applies migrations and exits — the Helm migrate hook Job (TS-D21) |
| `OPENBAO_K8S_TOKEN_PATH`, `BAO_CACERT` | server, consumer, rotator, scheduler | OpenBao login JWT path (Helm: `openbao`-audience projected token) and private CA (TS-D21) |
| `AWS_REGION`, `AWS_ENDPOINT_URL` (dev only) | server, consumer + platform-events | AWS clients |
| `GLUE_REGISTRY_NAME` (`iam-serviceaccount-events`), `SNS_TOPIC_ARN` | server + platform-events `config.LoadSNS` | produced events (§7); both required outside dev |
| `OUTBOX_*` (`POLL_INTERVAL`, `BATCH_SIZE`, `MAX_ATTEMPTS`, `DRAIN_TIMEOUT`, `PUBLISH_CONCURRENCY`, `PUBLISH_TIMEOUT`, `STARTUP_JITTER`, `CLAIM_LEASE_DURATION`) | server + platform-events `config.LoadOutbox` | outbox runner |
| `SQS_QUEUE_URL`, `SQS_CONCURRENCY`, `SQS_MAX_MESSAGES`, `SQS_WAIT_SECONDS`, `SQS_VISIBILITY_TIMEOUT`, `SQS_HANDLER_TIMEOUT`, `SQS_DRAIN_TIMEOUT`, `SQS_QUEUE_DEPTH_INTERVAL`, `SQS_RETRY_BACKOFF` (60s), `SQS_MAX_RETRY_BACKOFF` (15m) | consumer + platform-events `config.LoadSQS` | offboarding queue; `SQS_QUEUE_URL` required outside dev. The retry backoff (platform-events v2.1.0) hides a failed message 60s·2^(n-1) after its n-th delivery, so `maxReceiveCount=5` spans ~15 minutes of outage instead of ~5; `0s` turns it off |
| `DOCS_ENABLED`, `DOCS_AUTH_TOKEN` | server | AsyncAPI/Swagger viewer gating |

The system principal (`…00a1`) and the OpenBao path prefix (`iam/serviceaccount/…`, §6.3) are compiled constants (frozen, §25), not configuration.

**Configuration invariants:**

| # | Invariant |
|---|---|
| TS-CONFIG-1 | All infrastructure endpoints (OpenBao, SNS/SQS, Postgres) are environment-supplied; no environment value is compiled into the binary (only `BUILD_VERSION`). The same image runs everywhere. |
| TS-CONFIG-2 | Migrations use `MIGRATION_DATABASE_URL` and bypass PgBouncer (DDL incompatible with transaction pooling); the app role cannot `CREATE` on `public`. |
| TS-CONFIG-3 | OpenBao access is via the Kubernetes auth method (pod ServiceAccount → role → policy); there is no static OpenBao token and no AWS Secrets Manager permission (HLD §11.2, §15.5.3). |
| TS-CONFIG-4 | `overlap_seconds` is clamped server-side to `[0, 900]` regardless of the request or default; a config value outside the range is rejected at startup. |

### 12.1 Docs gating and fail-fast

`DocsConfig` gates `/asyncapi`, `/asyncapi.yaml` and `/swagger/*`: always mounted in `local`/`dev`/`test`; in every other environment (staging included) mounted only with `DOCS_ENABLED=true`, and then bearer-gated by `DOCS_AUTH_TOKEN` (constant-time compare). `cmd/server` refuses to start outside local/dev/test when docs are enabled without the token, and the chart refuses to render `docs.enabled` without `docs.authEnabled` (TS-D22/TS-D23). The generated Swagger spec declares both identity headers on every TS route. Each binary fail-fast-validates on startup outside dev: an empty `OPENBAO_ADDR` (every binary), `GLUE_REGISTRY_NAME` or `SNS_TOPIC_ARN` (server), `SQS_QUEUE_URL` (consumer) or `REALM_PROVISIONER_BASE_URL` (rotator, scheduler), or a `ROTATION_DEFAULT_OVERLAP_SECONDS` outside `[0, 900]`, panics rather than starting a mis-wired service. `cmd/server` also rejects a non-positive `JWKS_KNOWN_TENANTS_REFRESH` and a negative `ROTATION_REPLAY_WINDOW` at startup in every environment.

---

## 13. Deployment and Scaling

### 13.1 Topology

EKS, one Helm chart (`deploy/helm/`), one distroless image with four binaries:

| Workload | Binary | Kind | Database role(s) |
|---|---|---|---|
| server | `cmd/server` — TS-1..TS-6, JWKS, outbox runner, DB-state exporters, JWKS known-tenant refresher | Deployment, 2 replicas | `serviceaccount_app`; `serviceaccount_reconciler` read-only for exporters and the known-tenant list |
| consumer | `cmd/consumer` — `TenantMembershipsPurged` offboarding (§7.1) | Deployment, 2 replicas | `serviceaccount_app` |
| rotator | `cmd/rotator` — retry pass, sweep, orphan reconciler, prunes (§8.3/§8.6/§8.8/§8.9) | CronJob, every 5 min | `serviceaccount_reconciler` to enumerate, `serviceaccount_app` to write |
| scheduler | `cmd/scheduler` — retry pass and cadence rotation (§8.2) | CronJob, every 5 min | same as rotator; its write is `CredentialService.IssueOrRotate` |
| migrate | `cmd/server` with `MIGRATE_ONLY=true` | pre-install/pre-upgrade hook Job | `serviceaccount_migrator` (direct DSN) |

RDS PostgreSQL `serviceaccount` (Multi-AZ, PgBouncer transaction pooling); migrations and the reconciler pool use the direct port (§4.4). OpenBao (in-cluster HA) holds the key material. Each workload runs under its own ServiceAccount (§10.5). The rotator and scheduler both egress to the Realm Provisioner for RP-17 (§8.8).

### 13.2 Scaling

The service is trivially small (§21). Two server and two consumer replicas cover HA, not load. The HPA is off by default (`autoscaling.enabled: false`); turned on, it scales the server on CPU/memory. Server and consumer replicas are spread with a required pod anti-affinity, which the server relaxes to preferred when the HPA is on (it may scale past the node count); a PodDisruptionBudget keeps `minAvailable: 1`. The consumer runs at low concurrency (`SQS_CONCURRENCY` 2). The CronJobs are singletons (`concurrencyPolicy: Forbid`) and process at most 500 rows per run (`*_BATCH_LIMIT`). Scaling triggers, if ever needed, would be tenant count and rotation cadence — both far below any single-replica limit.

### 13.3 Migration safety and disaster recovery

Dev-stage migrations are outright (no expand/contract, §19). **Disaster recovery:** metadata is protected by RDS Multi-AZ + automated backups; secret material by OpenBao's own HA/backup posture. Because plaintext lives only in OpenBao, a Postgres restore never resurrects a secret; a metadata/OpenBao divergence is reconciled by **rotation** (regenerate, then RP-17, §6.4/CUST-3), never by reading a stale secret. RTO/RPO follow the platform defaults (HLD §13.1).

### 13.4 Helm chart

`deploy/helm/` renders, from one image:

- **Workloads.** `deployment-server.yaml`, `deployment-consumer.yaml`, `cronjob-rotator.yaml`, `cronjob-scheduler.yaml`, each with its own ServiceAccount (`serviceAccount.perWorkload`, §10.5). The image is `repository@digest` when `image.digest` is set (what the release signs), else `repository:tag`. Every pod restarts on a change to the chart-rendered Secret (`checksum/secret`), or on a bumped `rotationEpoch` when the Secret is external.
- **Migrate hook Job** (`job-migrate.yaml`, `migrations.enabled`). A `pre-install,pre-upgrade` hook (weight 0) that runs `/iam-token-service-server` with `MIGRATE_ONLY=true`: `outbox.ApplySchema`, then the domain migrations, then exit. Its ServiceAccount, the chart-rendered Secret and its NetworkPolicy are hooks at weight -10 so they exist first on a first install; the Secret carries release-ownership annotations so later upgrades keep managing it. The Job is never mesh-injected, mounts no ServiceAccount token, and has `backoffLimit: 1`, `activeDeadlineSeconds: 300`. All workloads run with `RUN_MIGRATIONS=false`, so only this Job holds the migrator DSN, and a rollback never makes an older binary migrate a newer schema (TS-D21/TS-D22).
- **Render guards** (`validate.yaml`, plus per-template guards). Outside `local`/`dev`/`test`: `authorizationPolicy.enabled` with `meshNamespaces` and `keycloakNamespaces`, `events.topicArn`, `sqs.queueUrl`, and `docs.authEnabled` whenever `docs.enabled`. Always: `workloadAnnotations` (other than `migrate`) requires `perWorkload: true`; with NetworkPolicy on, the four selectors/CIDRs listed in §10.2.
- **Mesh.** `authorizationpolicy.yaml` (the `AuthorizationPolicy` and, with `strictMTLS`, a STRICT `PeerAuthentication`, §10.2). With the policy on, server and consumer pods are force-injected with **native sidecars** (`sidecar.istio.io/nativeSidecar`), so Envoy starts before and stops after the app container and the drain below runs with the mesh still up. CronJobs are not injected unless `cronjobs.istioInject`.
- **Shutdown and grace periods.** On SIGTERM the server and consumer fail `/readyz` for `SHUTDOWN_DRAIN_DELAY` (5 s) so the endpoint is removed before the listener closes, then stop the HTTP server (30 s budget), the outbox runner (`OUTBOX_DRAIN_TIMEOUT` 30 s) or the SQS loop (`SQS_DRAIN_TIMEOUT` 15 s), and drain the pools. `terminationGracePeriodSeconds`: server 80, consumer 60, CronJobs 45 (enough for the in-flight tenant's 20 s row reserve plus its 10 s RP-17, §8.3).
- **Probes** (server and consumer): startup `/healthz` (period 5 s, 12 failures), liveness `/healthz` (period 15 s), readiness `/readyz` (period 10 s, `timeoutSeconds: 3`, above the handler's 2 s per-check deadline so the handler, not the kubelet, answers 503).
- **CronJobs.** `concurrencyPolicy: Forbid`, `activeDeadlineSeconds: 240` (shorter than the 5-minute schedule), `runTimeout` 180 s + `metricsScrapeGrace` 15 s under it so the graceful exit always wins (TS-D13/TS-D15). `podFailurePolicy` fails the Job at once on exit code 1 (a failed run or a page-worthy divergence) instead of retrying the batch, and ignores node disruptions. `PG_STATEMENT_TIMEOUT` is 60 s for the CronJobs and 5 s for server/consumer; `PG_LOCK_TIMEOUT` is 2 s everywhere.
- **Monitoring.** `servicemonitor.yaml` scrapes server and consumer; `podmonitor.yaml` scrapes the rotator and scheduler pods during their post-run grace window (5 s interval) and rewrites `job` to `<fullname>-<component>`. Both use `honorLabels: true`, so the series' own `service` label (which the alerts select on) wins over the target's. `prometheusrule.yaml` mirrors `deploy/monitoring/app-alerts.yml` and `prometheusrule-slo.yaml` mirrors `slo-rules.yml`.
- **Network.** `networkpolicy.yaml` (§10.2), `pdb.yaml`, `hpa.yaml`; optional `httproute.yaml` / `ingress.yaml` / `securitypolicy.yaml` for an internal gateway (off by default).

Every value the chart sets maps to an env var a binary or shared library reads (§12, TS-D20); `templates/_env.tpl` holds the common block.

### 13.5 CI/CD

CI mirrors the siblings (HLD §15.5): `go-arch-lint`, the `make gates` checks (RLS-6 `SET LOCAL`, no-`gocloak`, secret-logging with its self-test, gincommon-observability, metrics-taxonomy, §3.2), `make metrics-lint`, `schema-gov validate` for the AsyncAPI/Glue contract, `govulncheck` over `./...` (`make vuln-check`, govulncheck v1.8.0), and **`gosec`** (Go-code SAST, TS-D13). All third-party and first-party actions are pinned by commit SHA, and Dependabot is enabled (TS-D21/TS-D22). AWS access is via OIDC (no long-lived keys); OpenBao access is via Kubernetes auth, not an AWS IAM action (HLD §15.5.3).

**Images are tagged only after they pass.** On a push to `main`, `ci.yml` builds and pushes a *candidate*, scans the pushed digest (Trivy), smoke-tests that same digest for all four binaries, signs it with Cosign, verifies the signature, and only then publishes the real tags (TS-D22/TS-D23).

**Release flow** (`release.yml`, on a `v*` tag): `preflight` (the run must be on a tag ref; a dispatched release must be started from the tag itself, since Cosign verification needs a `refs/tags/` identity) → `validate-test` + `validate-quality` → `build` (tag/CHANGELOG gate, binaries) → `docker` (candidate push, CVE scan, SBOM and SLSA provenance, smoke tests of the pushed digest for all four binaries, Cosign sign and verify, then the release tags) → `register-schemas` (`schema-registry.yml` via `workflow_call`, production Glue registration, before any deploy because the server refuses to start on an unregistered schema) → `deploy-gate` (only when `vars.DEPLOY_GATE_ENABLED`; `helm upgrade --atomic` with the environment's values from the `HELM_VALUES_B64` secret and `image.digest` set to the signed digest, then a check that the running image is that digest) → `publish` (GitHub Release). Pre-release tags (any `-` suffix) build, scan, sign and publish the image but skip `register-schemas` and `deploy-gate` (TS-D23).

### 13.6 Environments

dev → staging → prod promotion per HLD §15.7. Nothing is deployed to any environment yet (§19). Docs routes are open in local/dev/test and gated everywhere else, staging included (§12.1); the production safety guards in §13.4 apply to every non-dev `appEnv`.

---

## 14. Testing Strategy

### 14.1 Unit tests

- **Service** (`test/unit/service`, over fake ports): issue vs rotate branching, `overlap_seconds` clamping, optimistic-lock conflict, `rotation_id` idempotency (same version, no re-generate), `422` on a revoked principal, JWKS key selection and the `Unservable` rule. `credential_service_lock_test.go` (TS-D22/TS-D23): keygen runs before the locked transaction, the in-lock OpenBao write and delete are bounded, replays inside/outside `ROTATION_REPLAY_WINDOW` (and `0` = unlimited, revoked outranks expired), every replay is audited, and the lock-timeout `409` carries `active_rotation_id` only when an active credential exists.
- **Consumer** (`test/unit/consumer`) drives `OffboardingConsumer` over a `fakeInbox` (`port.Inbox`); `cmd/consumer/dlq_test.go` / `inbound_schema_test.go` cover DLQ routing for both reasons, `RedrivePolicy` parsing and Glue-framed decoding.
- **CronJobs.** `cmd/rotator/sweep_test.go`: per-tenant grouping, inline RP-17 once per tenant on an uncancelled context, the 30 s tenant reserve, SIGTERM before and during a tenant, budget deferral, skipped races, and the marker lifecycle; the retry pass (failure keeps the marker, an offboarded tenant's marker is dropped, list failure fails, signalled runs defer) and `refreshAndClear` keeping a newer request. `cmd/rotator/orphan_reconciler_test.go`: deletes under the lock, lock timeouts skipped, principals with no rows, rotating start offset, missing-material confirmed under the lock (an in-flight revoke is not missing), bounded in-lock calls, every stored prefix scanned. `cmd/rotator/prune_test.go`: each prune, deferral on budget or signal versus failure. `cmd/scheduler/scan_test.go`: intent marker before `IssueOrRotate`, `ExpectActiveVersion`, skipped races without RP-17, ambiguous failures keeping the marker, RP-17 failure is `failed`.
- **Server.** `cmd/server/exporters_test.go` covers the RLS-violation exporter's cursor and the keys-refresh gauge; `cmd/server/main_helpers_test.go` the drain-aware, deadline-bounded `/readyz` and the startup env validation (docs token, replay window, known-tenant refresh).
- **Metrics standard.** `test/unit/metricsstandard` (`TestStandard_LibrariesShareRegistryCollectors`) registers metrics in production order, asserts the identity labels on every `platform_*`/`iam_*` series, and writes the scrape `make metrics-lint` checks when `METRICS_SCRAPE_OUT` is set.

### 14.2 Integration tests

**Real Postgres + OpenBao** (`test/integration`, testcontainers Postgres and a dev-mode OpenBao KV v2): `credential_service_test.go` runs register → issue → rotate → revoke through `CredentialService` and checks that OpenBao and Postgres agree at each step: the key is at the frozen path, both versions' material exists during the overlap, and a revoked version's material is really gone (CUST-2); CONC-2 (one `active` per principal) is asserted in `rls_test.go`; `openbao_test.go` covers the client (write/read round trip, idempotent delete, `LIST` of versions, a real permission-denied error, token caching); `glue_codec_test.go` is described below. **Cross-tenant enumeration (RLS-7)** is covered in `test/postgres/reconciler_repository_test.go` (expired-rotating and due-for-rotation lists across tenants, oldest first, batch limit; principal material states across tenants) and `rls_test.go` (`TestReconcilerRoleIsEnumerationOnly`: the reconciler role cannot `UPDATE`/`DELETE`). The sweep, reconciler and scheduler decision logic (orphan delete under the lock, missing-material confirmation, markers) is covered by the CronJob unit tests in §14.1 over fakes of those repositories.

**Postgres (`test/postgres`, testcontainers, real migrations):**

| File | Covers |
|---|---|
| `rls_test.go` | RLS-1..RLS-7 |
| `repository_test.go` | repositories, incl. `TestInboxRepository_ProcessOnceAndPrune` / `TestInboxRepository_Prune` |
| `reconciler_repository_test.go` | cross-tenant enumeration |
| `consumer_test.go` | offboarding redelivery is a no-op, a failed cascade leaves the event unclaimed, concurrent deliveries run the cascade once (TS-D19) |
| `rls_violation_log_test.go` | both violation types logged, no false positive from a tenant-scoped repository path, grants and `prune_rls_violation_log` (TS-D18) |
| `lock_timeout_test.go` | a lock wait past `lock_timeout` maps to `409 rotation_in_flight`, and through the real service carries `active_rotation_id` (TS-D22/TS-D23) |
| `credential_concurrency_test.go` | two concurrent TS-1 calls with different `rotation_id`s get consecutive versions; issuing after revoking the active version does not reuse a version (TS-D21) |
| `migration_roundtrip_test.go` | every down migration reverses its up migration and the schema re-applies cleanly |
| `hardening_migration_test.go` | `000004`: the app role can dead-letter outbox events, `prune_rls_violation_log` refuses a TTL under 7 days, `app_tenant_id()` has a pinned `search_path` |
| `keys_refresh_test.go` | `000005` shape and grants, the marker written transactionally with the revoke, monotonic `requested_at`, intent vs committed upsert rules, `ListPending` skipping live intents and flagging offboarded tenants, `PendingStats`, and that the reconciler role cannot write markers |
| `jwks_tenants_test.go` | the known-tenant listing returns exactly the tenants with an `active`/`rotating` credential (TS-D23) |

`internal/adapter/outbound/postgres/repository_errors_test.go` forces every driver-error branch (including `55P03` from `QueryRow.Scan` and `rows.Err()`) through a fake `pgx.Tx`, with no Postgres.

**Event pipeline (rev 1.4):** `test/integration/glue_codec_test.go` (floci Glue) — a codec built from the v1 embedded schema still stamps v1's UUID after a BACKWARD-compatible v2 is registered, all five produced schemas resolve by definition, and an unregistered or different definition fails startup. Unit: `cmd/consumer/dlq_test.go` / `inbound_schema_test.go` (DLQ routing + `RedrivePolicy` parsing, validation against the real embedded schema, and the actual consumer decoding a Glue-framed `TenantMembershipsPurged` — that test fails without `WithConsumerCodec`); `eventbus/glue_codec_test.go` (mock Glue that rejects any action but `GetSchemaByDefinition`, non-`AVAILABLE`/missing-schema errors, Python parity for every produced schema).

### 14.3 Contract tests

AsyncAPI/Glue validation (`schema-gov validate`) for the five produced events and the one consumed event; the generated OpenAPI spec for TS-1..TS-6 and the EXT-6 JWKS route, with `make swag-check` failing on drift. A negative contract test (`test/contract/no_secret_payload_test.go`) asserts **no** event payload contains a `secret`/material field (EVT-2).

**Gate self-test.** `check-no-secret-log.sh` runs `test_check_no_secret_log.py` before the real scan, so a regression in the checker itself (a sink or name pattern it stops catching) fails CI instead of silently passing (§3.2).

### 14.4 End-to-end / smoke

Against a dev mesh: RP mints the client (client-jwt/jwks.url, EXT-6) → TS-4 register → TS-1 issue → a client-credentials token is obtained from Keycloak (fetched lazily off the JWKS, no RP action needed at mint time) → rotate → RP calls `ClearServiceAccountKeysCache` → both old and new keys valid within overlap → cache-clear again after expiry → old key invalid. In-repo e2e (`test/e2e`, the HTTP stack against Postgres and OpenBao) covers registration, the credential lifecycle, reads, error shapes and `tenant_roles_test.go` (an oversized `x-tenant-roles` header is ignored, TS-D23). Smoke (`.github/scripts/smoke-tests.sh`, once per binary, in CI and release against the pushed digest): the image stays under 200 MB, and each binary exits non-zero within 10 s when its required config is missing (proving the fail-fast validation of §12.1 runs). `/readyz` behaviour (2 s per check, `503` while draining or on a hung dependency) is covered by `cmd/server/main_helpers_test.go`.

### 14.5 RLS test cases (canonical)

The canonical sibling RLS matrix, applied to both tenant-scoped tables:

| # | Case | Expectation |
|---|---|---|
| RLS-T1 | Read with no `app.tenant_id` GUC | zero rows (fail-closed, RLS-2) |
| RLS-T2 | Read/write with tenant A's GUC against tenant B's row | zero rows / write rejected |
| RLS-T3 | Insert a credential whose `tenant_id` ≠ GUC | `WITH CHECK` violation |
| RLS-T4 | Session-scoped `SET app.tenant_id` (not `SET LOCAL`) | CI-forbidden pattern; gate fails (RLS-6) |
| RLS-T5 | Composite FK to a principal in another tenant | rejected (Layer 3, §10.1) |
| RLS-T6 | `outbox_events`/`processed_events`/`rls_violation_log`/`keys_refresh_pending` under RLS | exempt; operate without a tenant GUC |

---

## 15. GDPR, Data Lifecycle, and Compliance

### 15.1 What PII passes through this service

Almost none. The service stores no human PII — only a service-account principal's Keycloak `sub`, `client_id`, and credential metadata. The secret material is not personal data. `granted_by` holds the actor `x-user-id` (the `iam-system` principal on cron/RP paths, an operator id otherwise) for audit attribution.

### 15.2 Role in tenant offboarding and GDPR erasure

On `TenantMembershipsPurged` the service deletes everything under the tenant's OpenBao prefix (including material from an uncommitted TS-1) and hard-deletes the tenant's `service_account_*` rows (§8.4). A purge message whose envelope id is invalid is never acked: it goes to the DLQ and pages, so an erasure cannot be silently skipped (§7.1). Because the plaintext lived only in OpenBao and is deleted, and the metadata is hard-deleted, no credential survives erasure. The emitted `ServiceAccountRevoked` audit event is retained by the Audit Log Service (security records override erasure).

### 15.3 Data residency

Metadata resides in the regional RDS `serviceaccount` instance; secret material in the regional OpenBao. Neither crosses region. Data residency follows the platform default (HLD §13.3).

### 15.4 Retention schedule summary

| Data | Retention |
|---|---|
| `service_account_*` rows | Life of the tenant; hard-deleted on offboarding (§8.4) |
| OpenBao secret material | Life of the credential version; deleted on revoke/offboarding (CUST-2) |
| `processed_events` | 8 days (`PROCESSED_EVENTS_TTL_DAYS`, > SQS message lifetime), then pruned via `inbox.Store.Prune` |
| `outbox_events` | Published rows older than `OUTBOX_PRUNE_OLDER_THAN` (168h), then pruned |
| `rls_violation_log` | 30 days (`RLS_VIOLATION_LOG_TTL_DAYS`, never less than 7), then pruned via `prune_rls_violation_log()` |
| `keys_refresh_pending` | Until the owed RP-17 succeeds; for a tenant offboarded since, the first failed retry drops it (§8.8) |
| Credential-lifecycle audit events | Retained by the Audit Log Service (compliance) |

---

## 16. Open Questions and Sign-off Register

| # | Item | Status |
|---|---|---|
| TSQ-1 | Confirm Keycloak client-secret rotation (rotated-secret grace TTL) is enabled on the `platform-automation` client template — the overlap in §6.2/§8.2 depends on it. Owner: Realm Provisioner + Keycloak ops. | **Re-resolved 2026-09-18 (rev 1.3, EXT-6) — the original resolution's premise was false; corrected to a verified mechanism, client-jwt/JWKS, not a dual-secret grace.** The rev-0.6/0.9 resolution below assumed Keycloak's plain client-secret authenticator supports a second, still-valid secret during a rotation window. Investigated against a real Keycloak 26.0 instance (EXT-6): **it does not** — one secret per client, `UpdateClient` invalidates the old one immediately, no realm-template setting for a grace period exists. `platform-automation` is corrected to authenticate via Keycloak's **`client-jwt`** (private_key_jwt) authenticator with `use.jwks.url=true`, pointing at a JWKS this service now serves (`JWKSService`, `GET .../tenants/{id}/service-accounts/platform-automation/jwks.json`). Verified two RSA keys served simultaneously **do** both authenticate — genuine overlap is real — with two corrections to the original optimistic assumption: Keycloak does not self-refresh a `jwks.url` client's keys (no auto-recognition of a new key, no auto-forgetting of a removed one) until the Realm Provisioner calls `ClearServiceAccountKeysCache` (RP-17, `POST /admin/realms/{realm}/clear-keys-cache`) — so RP-17 is now triggered on **every** rotate/revoke, not applied once at mint time. §6.2/§8.2/TS-D4 updated accordingly; the original rev-0.6/0.9 text is preserved below struck through context for history. *(Original, superseded 2026-09-18): Resolved 2026-09-11 — accepted as a design assumption; confirmation is a development-time coordination item, not a deployment check. The rotation design assumed Keycloak client-secret rotation with a dual-secret grace period on the `platform-automation` client template; nothing was deployed yet so there was no running Keycloak to validate against; the confirmation was treated as a design-time agreement with the Realm Provisioner team, verified in RP dev/integration tests (§14.4). Superseded in full by the EXT-6 finding above.* |
| TSQ-2 | Default rotation cadence (proposed 90 days) and whether O&M/operator UI exposes on-demand rotation at MVP. | **Resolved 2026-09-11 — 90-day cadence; on-demand at MVP, scheduled-cadence driver deferred.** Default credential-rotation cadence is **90 days** for platform automation principals. MVP ships operator-initiated **on-demand** rotation via O&M tooling / admin APIs (§8.2, for break-glass and suspected-compromise scenarios), which calls TS-1 then relays the plaintext to RP-17. **Amended 2026-09-17 (implementation-phase correction, IB-5/§16 TSQ-6):** this row previously stated MVP also supports "scheduled rotation (the `cmd/rotator`/cron path)" — that is inaccurate. `cmd/rotator` only runs the overlap-expiry sweep (§8.3), orphaned-material reconciliation (§8.6), and outbox prune; it never calls TS-1 to initiate a rotation, and no cadence-tracking data model (e.g. a `next_rotation_at` column) exists. The component that tracks rotation-due per principal and drives TS-1+RP-17 on the 90-day cadence is unbuilt and, by design, deferred to O&M/operator tooling outside these repos — see TSQ-6. **Tenant self-service rotation is out of scope for MVP** (post-launch, §2.4). The rotation-overlap mechanism (§6.2) is unchanged — one `active` version plus at most one prior `rotating` version during the configured grace window (TS-INV-3). **A break-glass revoke of a compromised secret is a two-halves operation** (TS-INV-7): TS-2 removes this service's material/metadata, but cutting the secret off at Keycloak requires the paired Realm Provisioner action (§8.7). The MVP operator tooling therefore drives *both* halves; TS-2 alone must not be presented to operators as "the secret is now dead." |
| TSQ-3 | Whether TS-1's plaintext-once response or an OpenBao-handle-only response (RP reads OpenBao directly) is preferred; both keep RP-INV-1 and TS-INV-2. | **Resolved 2026-09-11 — plaintext-once.** TS-1 returns the generated secret exactly once to the Realm Provisioner for the Keycloak update while retaining the authoritative copy in OpenBao. This keeps RP the sole Keycloak writer (RP-INV-1), avoids granting RP any OpenBao read access, and keeps a single issuance flow (one round-trip, no second RP→OpenBao read path to authorize or monitor). TS-D3 updated accordingly. |
| TSQ-4 | Name-freeze (§25) at sign-off. | **Resolved 2026-09-15 — frozen at design sign-off v1.0.** The §25 inventory (tables, enums, the `platform-automation` client name, the 5 published events + queue, the endpoint paths, the OpenBao path shape, the metric prefix, the Glue registry / SNS topic) is frozen as of this sign-off; any change to a frozen name after this date is a breaking change under the §7 schema-evolution discipline. **Amended rev 1.1 (2026-09-17):** the `keycloak_client_id` *value shape* (not the frozen base name itself) was widened to also accept a tenant-scoped variant, needed for RP-1's shared trial realm; see the rev 1.1 revision-history row for the full rationale. No table/enum/event/path/metric name changed. |
| TSQ-5 | Reconcile the base-HLD version reference: this LLD cites `HLD v1.45` while the repository snapshot is `iam-hld-tender-saas-v1.41.md`. Confirm the intended base version (the automation-principal design is stable across v1.41–v1.46). Owner: HLD maintainer. | **Resolved 2026-09-11 — proceed with the current design.** The version mismatch (v1.45 cited vs the v1.41 repository snapshot) is acknowledged as an editorial inconsistency, not a design gap: the automation-principal design (§5.8/§11.7/§16/§17.1) is materially identical across v1.41–v1.46, so no design change is required. The HLD maintainer will align the version reference in a follow-up documentation update. |
| TSQ-6 | **(Added 2026-09-17, IB-5) Who builds the scheduled-cadence rotation orchestrator, and where does its rotation-due state live?** TS-D10/TSQ-2 set a 90-day default cadence, but nothing in this service (or Realm Provisioner) tracks *when a given principal's credential is due* or *drives* TS-1+RP-17 automatically when it is. Today the only two rotation triggers are an operator and O&M tooling calling TS-1 then RP-17 by hand (§8.2) — verified against the actual `cmd/rotator` source, which runs only cleanup jobs (§8.3/§8.6/outbox-prune), never TS-1. Owner: proposed as O&M/operator tooling, out of scope for `iam-token-service`/`iam-realm-provisioner` themselves — but this has not been formally decided anywhere in either LLD; TS-D10/TSQ-2's "MVP supports scheduled rotation" language was aspirational, not a record of an actual scope decision to build it elsewhere. | **Resolved 2026-09-17 (rev 1.2) — cadence state lives in this service; the scheduler calls TS-1 directly via a dedicated service identity.** Both TSQ-6 sub-decisions are settled: (1) `rotation_cadence_days`/`next_rotation_at` are stored on the `active` row in this service's own `service_account_credentials` table (§4.2), queryable by O&M through TS-3 (§5.4) — not solely in an external O&M tool's store; (2) the cadence scheduler calls TS-1 **directly** (it does not merely surface a due-list for a human) — a new composition root, **`cmd/scheduler`**, scans `idx_sac_next_rotation` and drives TS-1 (then relays to RP-17) once a principal's `next_rotation_at` has passed, exactly the same two-call sequence §8.2 already describes for an operator, just automated. `cmd/scheduler` authenticates as a **dedicated service identity**: it presents the same reserved `x-user-id: iam-system` header as every other internal caller (§5.2/§10.4) and is added to the NetworkPolicy in-mesh allow-list alongside the Realm Provisioner and operator/O&M callers — this is a new *caller*, not a new *authorization mechanism* (TS-INV-4 is unaffected; the scheduler carries no roles and makes no authorization decision). Operator-triggered on-demand rotation (§8.2) is unchanged and remains available for break-glass/suspected-compromise. **`cmd/rotator` continues to perform retirement and cleanup only** (the overlap-expiry sweep §8.3, orphan-material reconciliation §8.6, outbox/processed-events prune) — it does not and will not initiate rotations; that responsibility belongs solely to `cmd/scheduler`. See TS-D10 (updated) and new **TS-D14** (§22). **Implemented 2026-09-18** — `cmd/scheduler`, the cadence stamping, the due-list scan, and the RP-17 relay client all landed; see TS-D14 for the shipped shape. |

### 16.1 Sign-off record

**Design sign-off: v1.0, 2026-09-15.** All open questions (TSQ-1..TSQ-5) are resolved and the name inventory (§25) is frozen. This is a **pre-deployment design sign-off** — it approves the design for implementation; it is not a production-readiness or security-operations sign-off (nothing is deployed, §15/§19).

| Sign-off dimension | State at v1.0 |
|---|---|
| Design owner (IAM platform engineering) | **Signed off** — scope, boundaries, data model, credential lifecycle, RLS, event contract, invariants (TS-INV-1..7, RLS-1..7, CUST, EVT, CONC, MIG). |
| Name inventory (§25) | **Frozen** (TSQ-4). |
| Open questions | **All resolved** (TSQ-1..5). |
| Security / SRE review | **Code-level pass performed at implementation (TS-D13)** — adversarial security/correctness/operational-readiness review against running code, findings fixed (§9.2/§9.3, §10.2, §11.2/§11.5, §13.4/§13.5, §17, §19). **Still outstanding:** a review against a real deployed footprint (mesh, live OpenBao HA, live cluster) — nothing is deployed yet (§15/§19), so that pass remains appropriate at first deployment, not before. |
| Audit Log LLD reconciliation | **Carried forward** — the `iam.serviceaccount.events` entry-type vocabulary (§7.5) is this service's produce contract, to be reconciled when the Audit Log LLD exists. |

**Cross-service follow-ups carried past sign-off** (tracked, not blocking this document — each lands in the named sibling's own work):

1. **TSQ-1 — resolved in full 2026-09-18 (rev 1.3, EXT-6), not merely tracked as a follow-up.** The `platform-automation` client template now uses Keycloak's `client-jwt`/`jwks.url` authenticator (§6.2, TS-D4) rather than a client-secret dual-grace TTL — verified empirically against a real Keycloak 26.0 instance, and specified in the Realm Provisioner's own LLD §2.5, rev 1.13 (its own EXT-6 revision row). Real-Keycloak integration test coverage proving the two-key-overlap/cache-clear behavior lives in the Realm Provisioner repo (`test/integration/keycloak_service_account_test.go`, its own EXT-6 test, superseding the original TSQ-1 e2e case placeholder).
2. **O&M non-member guard — this service's half delivered (TS-D16), the O&M-side half still tracked.** O&M's role/membership-grant endpoints must reject a `service_account`-typed subject (the mechanism behind TS-INV-4 / the non-member guarantee). This service now exposes the one primitive that check needs — **TS-5** (`GET …/service-accounts?principal_sub=<uuid>`, §5.4) — so org-membership can resolve "is this subject a service account" from the only identifier it has (a Keycloak `sub`). The O&M/org-membership-side integration of that check against TS-5 remains tracked as a companion guard, mirroring RP-9/RP-16.
3. **Audit Log entry-type contract** (as above).

Post-sign-off, a change to any frozen name (§25) or to a resolved TSQ is a **revision with a new rev number and a breaking-change note**, not an in-place edit.

---

## 17. Appendix — Error Taxonomy

`platform-gincommon.ErrorResponse` flat wire shape:

```json
{ "error": "principal_not_found", "status": 404, "trace_id": "abc123", "request_id": "req-xyz", "details": {} }
```

Internal API (`/api/v1/internal/*`) codes:

| `error` | HTTP | Meaning | `details` |
|---|---|---|---|
| `missing_identity_headers` | 401 | `x-user-id` or `x-tenant-id` absent, repeated or not a UUID, or `x-user-id` not the iam-system principal (RLS-5; TS-D22) | — |
| `principal_not_found` | 404 | No principal for `(tenant_id, principal_id)` / `principal_sub` / the tenant (TS-6), or no credential at the TS-2 version | — |
| `rotation_in_flight` | 409 | The principal's row lock (another TS-1/TS-2, the scheduler, the reconciler or the offboarding cascade) was still held after `PG_LOCK_TIMEOUT` — SQLSTATE `55P03` on any repository path (TS-D21/TS-D22). Retryable | `details.active_rotation_id` (best effort, when an active credential exists, TS-D23) |
| `optimistic_lock_conflict` | 409 | `record_version` mismatch on UPDATE (CONC-1) | `details.expected_version` |
| `principal_revoked` | 422 | Issue/rotate against a `revoked` principal | — |
| `secret_store_unavailable` | 502 | OpenBao read/write/delete failed | — |
| `invalid_request` | 400 | Malformed `rotation_id`/`overlap_seconds`/path id (§10.3) | `details.field` |
| `db_unavailable` | 503 | Postgres connectivity/resource failure, classified **only** by positive SQLSTATE identification (class `08`/`53`/`57`/`58`) or a closed pool (`puddle.ErrClosedPool`) — **corrected 2026-09-20, TS-D16**: no longer a broad "network I/O" catch-all (see TS-D16 for why). A `*domain.Error` from `HandleError`'s independent PgError classification is the same code, as defense-in-depth. Additive to this taxonomy, implementation-phase (TS-D13) | — |
| `rate_limited` | 429 | The JWKS route's per-tenant, global or unknown-tenant bucket was empty (`JWKS_RATE_LIMIT_*`, §5.4). Additive (TS-D21) | — |
| `tenant_path_mismatch` | 403 | The `{tenant_id}` path segment differs from the `x-tenant-id` header. Additive, implementation-phase | — |
| `unsupported_media_type` | 415 | A request body was sent without `Content-Type: application/json`. Additive, implementation-phase | — |
| `internal_error` | 500 | Any unclassified failure; logged with `trace_id`, `tenant_id` and route. Additive, implementation-phase | — |
| `jwks_keys_unavailable` | 503 | JWKS route only: the active credential's key, or every live key, could not be read from OpenBao. Keycloak keeps its previously cached keys on a failed fetch, which is safer than caching a set without the active key. Replaces that route's earlier use of `secret_store_unavailable` (whose status stays 502). Additive (TS-D23) | — |
| `credential_replay_expired` | 409 | TS-1 `rotation_id` replay later than `ROTATION_REPLAY_WINDOW` (default 15m) after the credential's `issued_at`; a replay of a revoked credential still reports `credential_replay_revoked`. Additive (TS-D22) | `details.version` |
| `credential_replay_revoked` | 409 | TS-1 `rotation_id` replay against a version that has since been revoked (overlap-expiry, TS-2, or offboarding) — additive to this taxonomy, implementation-phase (TS-D13, §9.2) | `details.version` |

Consumer processing dispositions (not HTTP responses — surfaced via metrics/DLQ):

| Disposition | Trigger | Effect |
|---|---|---|
| `duplicate` | inbox claim finds the id in `processed_events` | ack, no side effect |
| `retry` | OpenBao/DB transient failure | inbox transaction (claim included) rolls back; no delete → SQS redelivery |
| `dlq` | consumed-schema violation (`DLQReason=schema_violation`) or a `TenantMembershipsPurged` with a missing/invalid envelope id (`DLQReason=invalid_envelope_id`), both sent straight to the DLQ (§7.1); or decode failure / `maxReceiveCount=5` exhausted | DLQ + `consumer_dlq_rejects_total{reason}` + alert (§11.5) |
| `ack_unknown` | unrecognized event type | acked and recorded, `unknown_event_acknowledged_total` |

---

## 18. Integration Details

### 18.1 Realm Provisioner (`iam-realm-provisioner`) — RP ↔ this service

| Call | Endpoint | When | Notes |
|---|---|---|---|
| TS-4 | `POST …/tenants/:id/service-accounts` | after RP mints the `platform-automation` client (RP-1/RP-2) | RP supplies `principal_sub`, `keycloak_client_id`; idempotent on `(tenant_id, principal_type)` |
| TS-1 | `POST …/service-accounts/:principal_id/credentials` | provisioning (issue) and rotation (RP-17) | RP discards the returned private key immediately — Keycloak fetches the public half from this service's JWKS (§6.1, EXT-6); RP calls `ClearServiceAccountKeysCache` so Keycloak notices; idempotent per `rotation_id` |
| TS-3 | `GET …/service-accounts/:principal_id` | RP-18 reconcile/read | metadata only; RP never reads the secret from here |
| RP-17 (this → RP) | `POST {REALM_PROVISIONER_BASE_URL}/api/v1/internal/tenants/{id}/service-account/keys/refresh` | `cmd/rotator` after each tenant's sweep revokes, `cmd/scheduler` after each tenant's rotations, and both in their start-of-run retry pass (§8.8) | `iam-system` headers; 3 s per attempt, 2 attempts, retries unreachable/`429`/`502`/`503`/`504`; any `2xx` is success; a failure leaves the `keys_refresh_pending` marker for the next run |

RP is the sole Keycloak writer (RP-INV-1); this service never writes Keycloak (TS-INV-1). The split is the two-halves handshake (§6.1).

### 18.2 OpenBao — this service → OpenBao

| Call | Operation | When |
|---|---|---|
| KV v2 write | `PUT iam/serviceaccount/<tenant>/<client>/v<n>` | TS-1 issue/rotate (§6.3), under the principal lock, 5 s bound |
| KV v2 read | `GET …/v<n>` | JWKS route (public half derived in-process), TS-1 replay |
| KV v2 delete | `DELETE …/v<n>` | TS-2 revoke, overlap sweeps, orphan reconciler, offboarding (CUST-2) |
| KV v2 list | `LIST …/` | orphan reconciler (§8.6), offboarding prefix walk (§8.4) |

Authorized by the Kubernetes auth method (each workload's ServiceAccount, `openbao`-audience projected token → role → path-scoped policy), never AWS IAM (§10.5, HLD §11.2). No static token. Every call is recorded in `platform_dependency_request_seconds{dependency="openbao"}`.

### 18.3 Keycloak — indirect only

This service has **no** Keycloak write coupling (TS-INV-1) — no gocloak import, no Admin credential, no outbound call to Keycloak. The only traffic is inbound: Keycloak's `client-jwt` authenticator fetches the JWKS route (§5.4) and validates the presented `client_assertion` against it at the client-credentials token endpoint. Keycloak caches those keys until RP-17 clears the cache.

### 18.4 Audit Log — this service → Audit (via the bus)

**No direct HTTP coupling to Audit.** Every credential-lifecycle record is published on `iam.serviceaccount.events` through the outbox (§7) and consumed by the Audit Log Service. Audit-side dedup is on the stable envelope `id` (EVT-3). The entry-type vocabulary is this service's produce contract, to be reconciled with the Audit Log LLD when it exists.

### 18.5 Org & Membership / operator — O&M ↔ this service

O&M and operators may call TS-3 to read credential metadata (for a tenant-admin view of the automation principal's credential status). O&M enforces the non-member guarantee on its own side (AUTH-9) — its role/membership-grant endpoints reject a `service_account`-typed subject; this service depends on that guarantee but makes no call to O&M.

### 18.5a Workflow Service — Workflow → this service (TS-D17)

Workflow's connector workers name the tenant's `platform-automation` principal as the acting principal on step-completion callbacks. The principal is an actor and never a permission (a non-member by construction, TS-INV-4 / AUTH-9). Workflow reads the subject through TS-6. It keeps `principal_id` as the durable reference, because `principal_sub` changes on an RP-3/RP-4 re-mint (§5.4 TS-6). It may subscribe to `ServiceAccountRegistered` to refresh a cached sub. This service makes no call to Workflow.

### 18.6 Cross-service dependency table

| Direction | Interface | Purpose | Failure posture |
|---|---|---|---|
| Realm Provisioner → this | TS-4 (register), TS-1 (issue/rotate) | Provision + rotate the automation credential; RP refreshes Keycloak's key cache (RP-17, EXT-6) | RP retries; TS-1 idempotent per `rotation_id` |
| this → OpenBao | KV v2 read/write/delete/list | Custody of key material | `502 secret_store_unavailable`; nothing committed; JWKS `503 jwks_keys_unavailable` |
| this → Realm Provisioner | RP-17 (`cmd/rotator`, `cmd/scheduler`) | Make Keycloak re-fetch the JWKS after an automatic revoke/rotate | 2 attempts; durable `keys_refresh_pending` marker, retried every run; `failed` pages, backlog alert after 15 min (§8.8) |
| Core (events) → this | `TenantMembershipsPurged` (`tenant-lifecycle-tokensvc-q`) | Offboarding cascade | at-least-once; exactly-once processing via the inbox over `processed_events` |
| this → Audit Log (events) | `iam.serviceaccount.events` | Credential-lifecycle audit trail | outbox at-least-once; deterministic-`id` dedup |
| O&M / operator → this | TS-3 | Read credential metadata | plain read; no side effect |
| Workflow Service → this | TS-6 | Resolve the tenant's automation subject for connector callbacks (TS-D17) | plain read; `404` when not minted; cache and invalidate on `ServiceAccountRegistered` |
| Keycloak → this | JWKS route | Fetch the public keys for `client-jwt` validation | rate-limited per tenant/global/unknown bucket; on `503` Keycloak keeps its cached keys |

---

## 19. Migration Strategy

Nothing is deployed to any environment. Initial deployment runs five migrations (§4.4): `000001_schema` creates the enums, the two tenant-scoped tables (with RLS + triggers), and the operational tables (outbox via `outbox.ApplySchema`, `processed_events`, `schema_migrations`); `000002_rotation_cadence` adds the cadence columns; `000003_rls_violation_log` adds `rls_violation_log` and the logging RLS predicate; `000004_hardening` fixes grants and hardens functions (TS-D21); `000005_keys_refresh_pending` adds the RP-17-owed marker table (TS-D22). Because no data exists anywhere, migrations are **outright** (no expand/contract dance). In Kubernetes they run once per release in the Helm pre-install/pre-upgrade hook Job (`MIGRATE_ONLY=true`, §13.4), never from the long-running workloads; the binaries tolerate a schema newer than their own migrations, so a `helm rollback` does not fail on it. Every down migration is exercised by `test/postgres/migration_roundtrip_test.go`. Post-launch, the `principal_type` enum is extended additively (`ALTER TYPE … ADD VALUE 'tenant_bot' | 'user_pat'`, §2.4) — forward-compatible, no table rewrite. Schema governance for events follows `platform-schemagov` (§7.6). Rollback in dev is a `DROP`/re-migrate; there is no production data to preserve.

**Implementation-phase fix (TS-D13).** The down migration's `REVOKE ... FROM admin_readonly` was unconditional, but `admin_readonly` is infra-provisioned ahead of the migration in prod and may legitimately not exist in dev/CI/a fresh environment (the up migration already guards its `GRANT` to that role the same way) — `REVOKE ... FROM <nonexistent role>` raises `role does not exist` and aborts the whole rollback. The down migration now guards the `REVOKE` identically, verified against a real Postgres instance in both branches (role present / role absent).

---

## 20. Operational Considerations

### 20.1 Outbox health

`platform_outbox_pending_events` (§11.2) is the primary bus-health signal — sustained growth means the SNS relay is stalled; credential operations still succeed (state is committed), only audit emission is delayed (EVT-4). Drain resumes automatically when SNS recovers. A warning-level `platform_outbox_pending_events` growth alert (TS-D13, §11.5) fires before the DLQ-depth alert, at an earlier and still fully-recoverable stage.

### 20.2 Stuck `rotating` versions

`rotation_overlap_active` should return to zero shortly after each rotation's `expires_at`. A non-zero gauge for an hour (`IAMTokenServiceRotationOverlapStuck`) means the rotation CronJob is not running, is deferring rows (batch limit or budget), or is failing OpenBao deletes — inspect `cmd/rotator` logs, `IAMTokenServiceRotatorNotRunning`/`…CronJobRunFailed` and OpenBao reachability (§8.3). The JWKS route already stops serving an expired overlap key, so Keycloak is not affected by the delay once RP-17 has run.

### 20.3 OpenBao dependency health

OpenBao errors raise `IAMTokenServiceOpenBaoErrors` (> 5% of calls over 10 min, all binaries) and latency `…OpenBaoCallLatencyHigh`. TS-1 cannot issue/rotate while OpenBao is down (`502`), and revokes fail before committing (material-first), so rows stay live until OpenBao returns. The JWKS route answers `503 jwks_keys_unavailable` if it cannot read the active key, and Keycloak keeps its cached keys meanwhile. Provisioning of new tenants stalls until OpenBao returns. The reconciler's own `missing_material` result (a committed row whose material is gone) is a separate page (§11.5), resolved by rotation not by the reconciler (§8.6).

### 20.4 Offboarding-cascade health

DLQ depth on `tenant-lifecycle-tokensvc-q-dlq` is an alert (`IAMTokenServiceOffboardingDLQBacklog`, plus `IAMTokenServiceOffboardingQueueStalled` on the main queue, §11.5); a stuck offboarding is inert-if-delayed (nothing reads a departed tenant's credential, §8.4) but must be drained for GDPR completeness (§15.2). Replay is idempotent (§9.2). Two rejects page at once instead of waiting for depth: `IAMTokenServiceConsumedSchemaViolation` and `IAMTokenServiceOffboardingInvalidEnvelopeRejected` (both mean a tenant's erasure has not run, §7.1).

### 20.5 Degradation matrix

| Dependency down | Effect | Posture |
|---|---|---|
| OpenBao | No issue/rotate/revoke; TS-3/TS-5/TS-6 unaffected; JWKS `503` (Keycloak keeps cached keys) | fail-closed on writes; retry on recovery |
| Realm Provisioner | Automatic revokes/rotations commit, but Keycloak keeps old key-cache state | markers kept, retried every run; `failed` pages; backlog alert (§20.6) |
| SNS bus | Audit emission delayed; operations succeed | outbox drains on recovery |
| SQS (offboarding) | Offboarding delayed | inert-if-delayed; DLQ + alert |
| RDS | Full outage (no metadata) | standard Multi-AZ failover |

### 20.6 RP-17 refresh backlog

`iam_token_service_keys_refresh_pending` counts tenants owed an RP-17 call and `…_oldest_age_seconds` the oldest one (§8.8). Each CronJob run retries every eligible marker first, so a marker older than 15 minutes (`IAMTokenServiceKeysRefreshBacklog`) has survived several runs: RP-17 is still failing, and Keycloak may still accept a revoked key or not yet know a new one. Check the Realm Provisioner and Keycloak, and the `RP-17 key-cache refresh still failing` log lines (tenant id included). The backlog drains by itself once RP-17 succeeds; nothing in this service needs repair.

---

## 21. Performance Considerations

The service is trivially small: a few principals per tenant (one at MVP), credential writes only at provisioning and on rotation (default cadence 90 days, plus on-demand). Sizing (HLD §14): 2 replicas for HA, not throughput. The dominant steady-state cost is TS-3 metadata reads (a single RLS-scoped join) and the periodic rotation cron — neither latency-sensitive. The write path (TS-1) is bounded by the OpenBao round-trip, not by Postgres. Generating an RSA-2048 keypair takes tens to hundreds of milliseconds; it runs before the principal lock is taken, so it never lengthens a lock hold. The JWKS route reads one OpenBao entry per live key (at most two) per fetch, bounded by the rate limits in §5.4; the CronJobs process at most 500 rows per run. There is no read cache because there is no hot read path to cache (§6 has no cache; §3.1).

---

## 22. Decision Register

Prefix `TS-D#`.

| # | Decision | Rationale |
|---|---|---|
| TS-D1 | **Credential system-of-record here; Keycloak applier is RP.** This service generates/versions/custodies material and serves its public half via JWKS; RP triggers Keycloak's key-cache refresh (corrected 2026-09-18/EXT-6, was "RP applies it to Keycloak" — see rev 1.3). | Preserves RP-INV-1 (sole Keycloak writer) and TS-INV-1 (this service never writes Keycloak); mirrors the RP-9/RP-16 two-halves pattern (§6.1). |
| TS-D2 | **No secret in Postgres — OpenBao only, path-referenced.** Postgres holds `openbao_path`, never material. | TS-INV-2; a Postgres restore can never resurrect a secret (§13.3). |
| TS-D3 | **Plaintext-once handoff (TS-1), never re-readable.** TS-1 returns the generated secret once to RP for the Keycloak update and retains the authoritative copy in OpenBao; TS-3 is metadata-only. Chosen over an OpenBao-handle-only response (RP reads OpenBao directly). | Minimizes plaintext exposure surface (CUST-1); keeps RP the sole Keycloak writer (RP-INV-1) without granting RP OpenBao read access; a single issuance flow, one round-trip, no second RP→OpenBao read path to authorize/monitor (TSQ-3 Resolved 2026-09-11). |
| TS-D4 | **Bounded rotation overlap via `expires_at` + Keycloak client-jwt/JWKS multi-key overlap (corrected 2026-09-18, rev 1.3, EXT-6).** Both a principal's `active` and `rotating` credentials' public keys are served simultaneously by this service's JWKS endpoint; both validate at Keycloak within `[0,900]s`. | In-flight callers on the old key are not cut off mid-request; leaked keys are bounded (§6.2). **Superseded the original "Keycloak dual-secret grace" assumption** — verified empirically (EXT-6) that Keycloak's plain client-secret authenticator has no such mechanism. The real mechanism (`client-jwt`/`jwks.url`) requires an explicit Realm Provisioner action (`ClearServiceAccountKeysCache`, RP-17) after every rotate/revoke for the change to take effect at Keycloak — it is not automatic the way a TTL-based grace would have been (TSQ-1 Re-resolved 2026-09-18). |
| TS-D5 | **Transactional outbox for lifecycle events.** Event bound to the committed state write. | Audit visibility never depends on a live bus (EVT-4); no event for a rolled-back rotation (§7.3). |
| TS-D6 | **Offboarding hard-deletes rows + OpenBao material; inert-if-delayed.** Mirrors the Tender-ACL/Delegation cascades. | GDPR erasure completeness (§15.2); nothing reads a departed tenant's credential (§8.4). |
| TS-D7 | **No `iam-keycloakclient`/gocloak dependency, CI-enforced.** A grep gate forbids the import (inverse of RP's requirement). | Makes TS-INV-1 structural, not conventional (§3.2). |
| TS-D8 | **Tables/API shaped for post-launch `tenant_bot`/`user_pat` without redesign.** Additive enum values + `principal_type` discriminator. | Phase-2 tenant bots and user PATs extend the same tables (§2.4); MVP ships only `platform_automation`. |
| TS-D9 | **Renamed inbound signal `TenantMembershipsPurged` (from `TenantOffboarded`) under ADR-0008.** | Consistency with the platform-wide rename; the semantics (tenant hard-delete/GDPR-wipe) are unchanged (§7.1). |
| TS-D10 | **90-day default rotation cadence; MVP ships operator/O&M on-demand rotation, plus automatic cadence-driven rotation via `cmd/scheduler`; no tenant self-service.** All paths call TS-1 (then RP-17); overlap mechanism unchanged. **Corrected 2026-09-17 (IB-5/§16 TSQ-6):** this decision previously claimed MVP supports *scheduled* rotation via a `cmd/rotator`/cron path — that was inaccurate at the time (`cmd/rotator` only sweeps/reconciles, §8.3/§8.6) and no rotation-due data model existed. **Resolved 2026-09-17 (rev 1.2, §16 TSQ-6 Resolved):** the scheduled-rotation gap is closed by a *new, separate* component, `cmd/scheduler` (TS-D14) — `cmd/rotator` itself remains cleanup-only and never calls TS-1. | Bounds credential lifetime by default while giving operators a break-glass / suspected-compromise rotation via O&M tooling/admin APIs; tenant self-service deferred with the rest of the post-launch surface (§2.4); one active + one prior version during the grace window (TS-INV-3, TSQ-2 Resolved 2026-09-11). |
| TS-D11 | **Material-first write ordering + a dedicated orphan-material reconciler** (`cmd/rotator` `ReconcileOrphanedMaterial`, §8.6). OpenBao write precedes the Postgres commit; an interrupted issue/rotate leaves an orphaned KV path that the reconciler reclaims. | Material must exist before a row references it (a row pointing at absent material is the worse failure); monotonic non-reused versions mean an orphan never self-heals, so a reclaiming sweep is required, not optional — it is the recovery mechanism §9.3/§13.3 depend on. The inverse (row without material) is alerted and fixed by rotation, never fabrication (CUST-3). |
| TS-D12 | **Revocation is two-halves, like rotation (TS-INV-7).** TS-2 removes this service's material/metadata; the paired RP action removes/rotates the Keycloak secret. TS-2 alone is complete only where a Keycloak change is already implied (overlap grace / realm deletion). | Keycloak is the validator and only RP writes it (§6.1, RP-INV-1); a metadata-only revoke cannot invalidate a live secret. Making this explicit closes a break-glass gap where a compromise revoke via TS-2 alone would leave the leaked secret authenticating (§8.7, TSQ-2). |
| TS-D13 | **Implementation-phase production-readiness hardening + Enterprise Platform Observability Standard adoption** (post-sign-off, no frozen name or resolved TSQ touched — normal living-document maintenance, not a revision): (1) NetworkPolicy `ingressNamespaceSelector` has no safe default, failing the Helm render if unset (§10.2/§13.4); (2) the down migration's `admin_readonly` REVOKE is guarded to match the up migration's grant guard (§19); (3) TS-2/overlap-sweep concurrent-revoke races re-check after an optimistic-lock conflict and return the already-achieved idempotent outcome instead of a false `409`/sweep failure (§9.2/§9.3); (4) a stale `rotation_id` replay against a since-revoked version is classified as `409 credential_replay_revoked` instead of a misleading `502` (§9.2, §17); (5) the OpenBao client clones its SDK client per call instead of mutating a shared client's token field, removing a logical (not Go-race-detector-flagged) token race under concurrent requests (§10.5); (6) `gosec` added as a Go-code SAST CI gate (§13.5); (7) the secret-logging CI gate rewritten to catch a secret-named field split across a multi-line log call or a capitalized Go identifier, which the original single-line regex could miss (§3.2/§11.4); (8) metrics adopt the three-tier `platform_*`/`iam_*`/`iam_token_service_*` taxonomy with three registry-proposed dual-emitted metrics and a CI naming gate (§11.2); (9) `pgcommon_pool_*` connection-pool metrics wired for every pool this service opens, with new pool-exhaustion/outbox-backlog/queue-age alerts (§11.5/§20.1); (10) the rotator CronJob's `activeDeadlineSeconds` shortened to leave slack before the next schedule tick (§13.4). | Found via an adversarial security/correctness/operational-readiness review conducted before any deployment (§16.1 notes a security/SRE review was "not yet performed" at design sign-off — this is that review, at implementation time, as anticipated). None of these change a frozen name (§25) or reopen a resolved TSQ (§16) — they are bug fixes and additive standard adoption within the signed-off design. |
| TS-D14 | **A new composition root, `cmd/scheduler`, drives automatic cadence-based rotation; `cmd/rotator` stays cleanup-only (§16 TSQ-6 Resolved).** `cmd/scheduler` scans `service_account_credentials` for `active` rows past `next_rotation_at` (`idx_sac_next_rotation`, §4.2) and calls TS-1 directly, then calls RP-17 (`ClearServiceAccountKeysCache`) so Keycloak re-fetches the JWKS — no key material is sent to RP (EXT-6/rev 1.3) — the same two-call sequence §8.2 describes for an operator, invoked automatically instead of by hand. It authenticates as a **dedicated service identity**: the existing reserved `x-user-id: iam-system` header (§5.2/§10.4), with `cmd/scheduler`'s pod added to the in-mesh NetworkPolicy allow-list as a new caller — no new authorization mechanism, no roles granted (TS-INV-4 unaffected). `cmd/rotator` is unchanged and explicitly out of scope for this responsibility. **Implemented 2026-09-18** — `cmd/scheduler` (`main.go`/`scan.go`/`helpers.go`), the `ReconcilerRepository.ListDueForRotation` due-list scan, `CredentialService`'s cadence stamping (`WithCadenceDays`), and the `internal/adapter/outbound/realmprovisioner` RP-17 client. | Keeps the fourth binary's responsibility singular and mirrors the existing `cmd/rotator` pattern (a dedicated composition root per scheduled concern) rather than overloading `cmd/rotator` with a privilege (calling TS-1, a `service`-layer write path) that RLS-7's rotator/BYPASSRLS design deliberately keeps separate from cross-tenant enumeration; reusing the existing system-principal auth model avoids inventing a second authorization path for one new caller. |
| TS-D15 | **EXT-6/rev 1.3 completeness fixes (production-readiness review, 2026-09-19; post-sign-off, no frozen name or resolved TSQ touched — normal living-document maintenance, not a revision).** Rev 1.3 (TS-D4/TS-INV-7) established that overlap-expiry revocation requires an explicit RP-17 `ClearServiceAccountKeysCache` call — a cached Keycloak key has no self-expiring TTL — but three places were never updated to match: (1) **`cmd/rotator`'s sweep never actually called RP-17** — `revokeExpiredRotating` now does, after its DB commit, via the same `port.RealmProvisionerClient` `cmd/scheduler` already uses (`reconciler_jobs`'s existing `adapters_outbound` allowance, `.go-arch-lint.yml`); a resulting RP-17 failure is classified `Failed`, not silently swallowed, since the row is already `revoked` and will never be re-enumerated (the same two-halves gap `cmd/scheduler`'s own rotate path already documents). (2) **§8.2's own prose still described the pre-EXT-6 plaintext-relay flow**, contradicting TS-D4/TS-INV-7/TSQ-1 elsewhere in this same document — corrected to the RSA-keypair/JWKS/`ClearServiceAccountKeysCache` flow. §8.3 corrected to match. (3) **§10.2/the NetworkPolicy chart never admitted Keycloak as an ingress source** for the JWKS route rev 1.3 introduced — a real cluster with `networkPolicy.enabled: true` would silently drop Keycloak's own JWKS fetch, making the client-jwt authenticator non-functional; new required, no-safe-default `networkPolicy.keycloakNamespaceSelector` (mirrors `ingressNamespaceSelector`'s existing fail-closed pattern, §13.4) plus a new ingress rule on the `-server` NetworkPolicy. **Second pass, same review, same day:** (4) the JWKS route had no defense against being hit at an arbitrary rate — every hit reads a private key out of OpenBao per live credential (`JWKSService.PublicKeys`) with no caller-identity gate to lean on (it can't have one, §10.2); `JWKSHandler.WithRateLimit` now installs a process-wide token-bucket limiter (`JWKS_RATE_LIMIT_RPS`/`JWKS_RATE_LIMIT_BURST`, default 20/40, §12) and the response carries `Cache-Control: public, max-age=60` (content is public and Keycloak's own fetch is lazy/uncached at its end regardless, so this adds no real staleness). (5) `RealmProvisionerClient.RefreshKeys` never retried — it now makes 2 attempts total with a 300ms backoff on a plausibly-transient failure (unreachable, or RP-17's own documented 502 `keycloak_unavailable`), never on a permanent one (422, or anything else) — RP-17 is documented idempotent (RP LLD §2.5), so this absorbs a blip that would otherwise page on-call every 5-minute CronJob tick for nothing. (6) new alert `IAMTokenServiceCadenceRotationFailures` on `iam_token_service_cadence_rotation_total{result="failed"}` (§11.5/§20) — the metric existed but nothing paged on it. **Third pass, same review, next day (2026-09-20):** (7) **`CadenceRotationTotal` was constructed but never passed to `MustRegister`** — it never actually reached `/metrics`, so alert (6) above could never have fired despite existing; fixed, and a regression test added (`TestRegister_IsIdempotentAndRegistersAllCollectors` now re-`MustRegister`s every collector and asserts each panics — a silent non-panic means "never registered," which is exactly how this bug hid from two prior review passes). (8) **the retry budget in (5) still didn't close against the CronJob deadline** — the original 3-attempt/5s-timeout policy costs ~16s worst case per row, and both `ROTATOR_RUN_TIMEOUT`/`SCHEDULER_RUN_TIMEOUT` in-process timeouts defaulted to 5 minutes, *longer* than `activeDeadlineSeconds` (240s default) — meaning a run that legitimately used its own budget could never reach its own graceful-exit logging/metrics/pool-drain before Kubernetes SIGKILLs the Pod first; `RealmProvisionerClient`'s request timeout is now 3s (down from 5s, justified since RP is in-mesh) making (5)'s worst case ~6.3s/row, and `ROTATOR_RUN_TIMEOUT`/`SCHEDULER_RUN_TIMEOUT` now default to 180s, safely under the k8s deadline. (9) **the JWKS route's silent-partial-200 had no observability** — a live credential whose OpenBao material is unreadable only reached a log line (`jwks_service.go`'s existing `Warn`, also now nil-guarded); `JWKSService.PublicKeys` now also returns a `skipped` count, and the handler (which — unlike `core/service` — may depend on `observability`) turns it into a new page-worthy counter, `iam_token_service_jwks_key_errors_total`, with a matching alert `IAMTokenServiceJWKSKeyErrors`. (10) added `X-Content-Type-Options: nosniff` to the JWKS response (its one fully-public, unauthenticated route) and `networkPolicy.keycloakNamespaceSelector`/`REALM_PROVISIONER_BASE_URL`/`JWKS_RATE_LIMIT_*` documentation gaps across `.env-example`, `docker-compose.yml`, and `.claude/CLAUDE.md` (which still described three binaries and never mentioned EXT-6 at all). **Known, accepted limitation, not fixed:** the JWKS mechanism has no algorithm agility — key size (RSA-2048) and algorithm (`RS256`) are hardcoded in both the keygen (`DefaultKeyGenerator`) and the JWKS reader (`x509.ParsePKCS1PrivateKey`, unconditional), so a future migration to a different algorithm or key size cannot coexist with already-issued keys without a reader-side type-dispatch change this review did not attempt, since nothing at this service's current scale motivates it yet. | (1) is a live security regression — without it, every credential the sweep revokes keeps authenticating at Keycloak indefinitely, silently, with no alert. (2) violates this document's own "tie-breaker on any discrepancy" claim (front matter) by disagreeing with itself. (3) means the shipped mechanism cannot work at all once NetworkPolicy is actually enforced, which is precisely the condition TS-D13 already hardened this chart to require (`ingressNamespaceSelector`'s own no-safe-default gate). (4)–(6) close the remaining gaps the same review flagged as non-blocking but real: an unmitigated OpenBao read-amplification surface, unnecessary paging on transient RP-17 blips, and a page-worthy metric nobody was paged on. (7)–(10) are the product of applying this review's own standard to itself: (7) is the same "constructed but never wired" failure mode as (1) and (3), just one layer down (a metric instead of a call); (8) is the same "budget math doesn't close" failure mode as the original RP-17 retry design; both were caught by a subsequent audit pass rather than the review that introduced them — worth noting as a limits-of-self-review lesson, not just a changelog entry. |
| TS-D16 | **TS-5 — find a principal by Keycloak sub, for AUTH-9's non-member defense-in-depth (2026-09-20; post-sign-off, no frozen name or resolved TSQ touched — normal living-document maintenance, not a revision).** New route `GET …/service-accounts?principal_sub=<uuid>` (§5.3/§5.4) — a query-param lookup on the existing TS-4 collection path (a static path segment at the same tree position as TS-3's `:principal_id` would conflict in gin's router). `PrincipalRepository.FindByPrincipalSub` (RLS-scoped, same pattern as `FindByID`) + `PrincipalService.FindPrincipalBySub` + the `FindBySub` handler. Deliberately returns identity/status only (`principalResponseBody`) — never `readPrincipalResponseBody`'s credential list, which a membership-grantability check has no legitimate use for and TS-3 already serves. Closes §16.1 follow-up #2's Token-Service-side half: org-membership's own service-account-not-grantable check needed a way to answer "is this subject a service account" from the only identifier it has (a Keycloak `sub`, seen elsewhere as `user_id`) — this service never generates `principal_sub` itself (TS-INV-1), so a sub-keyed lookup here is the only way to answer that without this service also generating identities. No new error code (reuses `principal_not_found`/`invalid_request`/`missing_identity_headers`); no schema change (reads the existing `principal_sub` column). **Same pass:** `wrapConnErr` (`internal/adapter/outbound/postgres/db.go`) no longer defaults an unrecognized error to `db_unavailable` via a broad `isNetworkError` heuristic (string-matching "connection refused"/EOF/etc.) — it now classifies **only** by positive SQLSTATE identification (class `08`/`53`, `pgcommon` helpers; class `57`/`58`, matched on the `pgconn` error text since `pgcommon` v1.3.0 has no dedicated helper yet) or a closed pool (`puddle.ErrClosedPool`); everything else, including a transport-level failure that never reached Postgres, now passes through unchanged instead of being silently relabeled "database unavailable" under the caller's real error. `HandleError` (`internal/adapter/inbound/http/errors.go`) gained an independent classification of a leaked `*pgconn.PgError` into the same `503 db_unavailable`, as defense-in-depth against a case `wrapConnErr` misses — §17's `db_unavailable` row corrected to match (was: "…, network I/O", a broad catch-all this fix removed). | TS-5 makes the non-member guarantee (TS-INV-4) actually enforceable outside this service, not just true inside it — a role-grant check that can never resolve "is this a service account" is a check that can never fire. The `wrapConnErr` correction fixes a real correctness bug: the old broad remap could discard a caller's genuine business error (a unique-constraint violation surfaced through an unexpected code path, say) under a misleading `503`, hiding the actual failure from whoever debugs it — narrowing to positive SQLSTATE identification, with an HTTP-layer fallback for the connectivity/resource classes specifically, keeps the `503` classification precise without losing the safety net. |
| TS-D17 | **TS-6: read the tenant's automation principal, for the Workflow Service's connector callbacks. Also fixes the `ServiceAccountRegistered` schema, which rejected every shared-realm registration (2026-09-24).** This was post-sign-off living-document maintenance and needs no revision: no frozen name or resolved TSQ is changed, only a path and a decision are added. New route `GET …/service-accounts/platform-automation` (§5.3/§5.4) → `PrincipalRepository.FindByType` → `PrincipalService.ReadPlatformAutomation` → `ReadPlatformAutomation`. It returns `principal_id`, `principal_sub`, `keycloak_client_id`, type, status and version, and never credential metadata. Answers Workflow's *Automation principal sync* Question 1 ("which subject is tenant T's automation principal"). TS-5 answers the reverse question, and RP-18 is the wrong runtime dependency for a daily-running worker. The path is addressed by the frozen principal name (the JWKS route already uses that static segment beside TS-3's `:principal_id`; gin ≥1.8 routes static-over-param siblings), not by making TS-5's `principal_sub` optional, which would have given one path two response shapes. Question 2 is answered in §5.4 TS-6: the sub is stable across rotation and changes on an RP-3/RP-4 re-mint; `principal_id` is the stable handle. Helm gains an optional `networkPolicy.workflowNamespaceSelector` ingress rule. **Same pass, a real bug:** `ServiceAccountRegistered`'s `keycloak_client_id` was still `const: "platform-automation"` in `api/asyncapi.yaml` and the embedded schema after rev 1.1 widened TS-4 to the tenant-scoped `platform-automation-<tenant_id>`. The enqueue-time `ValidatingCodec` rejected the event inside TS-4's transaction, so every RP-1 trial mint and every RP-4 revert re-mint failed with `500`. Unit tests used only the base name and nothing exercised the tenant-scoped shape end to end. It is now `pattern: ^platform-automation(-<uuid>)?$`, the same two shapes as `domain.ValidPlatformAutomationClientID`, and it is a BACKWARD-compatible loosening. It is covered by a codec table test and by the TS-6 e2e (register tenant-scoped → re-register base name → TS-6). | A connector worker has only a tenant id, so an "is this the automation sub" check is useless to it. A tenant→subject read belongs to the service that projects the identity (TS-INV-1), not the one that minted it. The schema fix unblocks the trial-signup path outright. |
| TS-D18 | **RLS violations become observable (2026-10-05).** Post-sign-off living-document maintenance: no frozen name or resolved TSQ is changed. `IAMTokenServiceRLSCrossTenantAccess` / `IAMTokenServiceRLSMissingGUC` queried a metric nothing emitted. Migration `000003_rls_violation_log` adds the sampled `rls_violation_log` and a logging `rls_check_tenant()` (§4.3), matching iam-org-membership's design; the service emits the registry's Tier 2 `iam_rls_violations_total` instead of inventing an `iam_token_service_*` name, and both alerts now query it. Two differences from iam-org-membership: the exporter uses an id cursor (each row counted once per pod, and a restart re-counts at most one interval), and retention is pruned through a `SECURITY DEFINER` function so the app role holds no `DELETE` on the audit table. Known limit (shared with iam-org-membership): rejected cross-tenant writes are not logged, because Postgres has no autonomous transactions. `test/postgres/rls_violation_log_test.go` asserts that every tenant-scoped repository call logs nothing when other tenants' rows are present. | Cross-tenant access attempts were invisible: RLS fails closed silently, so a regression could probe other tenants with nothing to alert on. |
| TS-D19 | **Offboarding dedup moves onto platform-events' `pkg/inbox`, as in iam-org-membership (2026-10-05).** Post-sign-off living-document maintenance: the frozen consumer name `tenant_offboarding` (§25), the `processed_events` table and the material-first ordering are unchanged. Before, the consumer checked `processed_events` first and recorded the id in a separate transaction after the OpenBao deletes, so two copies of one message handled at once could both run the cascade. Now `InboxRepository.ProcessOnce` (`inbox.Store.Process`) claims the id as the first statement of ONE transaction on the RLS-scoped app pool (`SET LOCAL app.tenant_id` from the context, RLS-6); inside it the cascade lists the tenant's credentials, deletes their OpenBao material (still before the commit, §9.3/§15.2), deletes the rows and enqueues `ServiceAccountRevoked`. Any failure rolls back the claim too, so the redelivery repeats the cascade. A concurrent copy waits on the claim's row lock and is a duplicate. Trade-off: the OpenBao HTTP calls now run while the transaction is open; offboarding is rare and per-tenant, so the held connection is acceptable. `platform_duplicate_messages_total` is now counted by the library; the service counts only its Tier 3 `iam_token_service_processed_events_duplicates_total`. `cmd/rotator` prunes through `inbox.Store.Prune` (batched until done). `test/postgres/consumer_test.go` covers redelivery, rollback on failure and concurrent delivery. | The old two-transaction dedup let concurrent copies of one message both run the cascade; the shared inbox closes that race and matches iam-org-membership. |
| TS-D20 | **Env audit against the shared libraries, aligned with iam-org-membership (2026-10-05).** Post-sign-off living-document maintenance: no frozen name or resolved TSQ changes. Every env var the chart sets is now read by a binary or a shared library it calls, and every library setting iam-org-membership passes is passed here (§12). Removed: `OTEL_SERVICE_NAME` (the trace name comes from `APP_NAME` since `InitTracingWithConfig`), the aliases `SNS_TOPIC_SERVICEACCOUNT_ARN` / `SQS_OFFBOARDING_QUEUE_URL` / `SQS_OFFBOARDING_CONCURRENCY` in favour of the libraries' canonical names, `AWS_REGION` on the CronJobs (no AWS client), and the unused values `database.logicalName`, `events.topic`, `events.source`. Fixed: `ROTATION_DEFAULT_OVERLAP_SECONDS` was documented and set but never read (the 300s constant was hardcoded); it now feeds the TS-1 default and the scheduler, and an out-of-range value fails startup (TS-CONFIG-4). Fixed: `PG_STATEMENT_TIMEOUT` was appended to the migration and reconciler DSNs by a hand-written helper that produced a malformed URL for a DSN without a query string and would have bounded migrations by the API's timeout; platform-pgcommon already applies it per transaction, so the helper is removed. Added: `PG_LOCK_TIMEOUT`, `PG_SLOW_QUERY_THRESHOLD`, `PG_MIN_CONNS`, `SQS_HANDLER_TIMEOUT` (45s, below the 60s visibility timeout), `SQS_DRAIN_TIMEOUT`, `LOG_SAMPLING`, the `OTEL_*` sampler/exporter settings, and the CronJob run/prune and server exporter intervals. | Settings that are set but never read (or read but never set) hide misconfiguration; aligning with the shared libraries' canonical names makes every value effective. |
| TS-D21 | **Production-readiness hardening pass (2026-10-06).** Post-sign-off living-document maintenance. No frozen name (§25) or resolved TSQ (§16) changes: the new env vars, migration, metric, alerts and error codes are all additive. **Concurrency:** issue/rotate, revoke and offboarding serialize on a `SELECT … FOR UPDATE` of the principal row (§9.1). Before, two TS-1 calls with different `rotation_id`s could compute the same next version and overwrite each other's OpenBao material. The next version also came from the highest *non-revoked* row, so issuing after a revoke reused a version number and broke `uq_sac_version`; it is now `MaxVersion + 1` over every row. Older open overlaps are closed when a new rotation commits (TS-INV-3). The scheduler passes `ExpectActiveVersion`, so a cadence rotation racing an operator rotation is a no-op, not a double rotation. **CronJobs:** the sweep and the scheduler process at most `ROTATOR_BATCH_LIMIT`/`SCHEDULER_BATCH_LIMIT` rows (default 500) and stop 20s before their run deadline, deferring the rest. RP-17 is called once per tenant, after that tenant's commits, on a context detached from the run deadline. Every failure, including a list or prune failure, fails the Job. The orphan reconciler lists prefixes taken from stored paths, deletes material that survived a revoke, and reports `missing_material` only for live rows, after a re-check. **Runtime:** OpenBao re-logs in once on a 403. RP-17 treats any 2xx as success and retries 429/502/503/504. The JWKS route gains a per-tenant limiter (`JWKS_RATE_LIMIT_PER_TENANT_RPS`/`_BURST`, new `iam_token_service_jwks_rate_limited_total`) and skips expired overlap keys. The server and consumer flip `/readyz` to 503 and wait `SHUTDOWN_DRAIN_DELAY` before closing listeners. A consumer whose loop exits early now exits non-zero. `57014` (query canceled) is `db_unavailable` only while the request is still live. **Database:** migration `000004_hardening` (§4.4). **Deployment:** per-workload ServiceAccounts, each with its own IRSA role (`deploy/iam/policy-{server,consumer}.json`; rotator, scheduler and migrate get none), and OpenBao login with an `openbao`-audience projected token. Migrations run in a pre-install/pre-upgrade hook Job (`MIGRATE_ONLY`, `RUN_MIGRATIONS`). NetworkPolicy egress uses CIDRs for off-cluster Postgres and OpenBao and limits the metrics port to the monitoring namespace. Also new: an optional Istio `AuthorizationPolicy`, image by digest, a PodMonitor for the CronJobs, and the SLO rules as a PrometheusRule. **Alerting:** per-run CronJob counters are read with `max_over_time`; new `IAMTokenServiceSchedulerNotRunning`, `IAMTokenServiceCronJobRunFailed` and `IAMTokenServiceJWKSRateLimited`. **Supply chain:** third-party actions are pinned by SHA, with Dependabot enabled. Releases are scanned, signed and smoke-tested by digest before the release tags are applied, and deployed by digest. `govulncheck` covers `./...`. GO-2026-6443 (gRPC server panic) stays open on `grpc v1.84.0` by choice: there is no gRPC server here (gRPC is only the OTLP exporter's client), and the fix exists only in an unreleased `v1.85.0-dev` build, so the bump waits for the v1.85.0 tag. | Found by a production-readiness review before any deployment; each item was a correctness, availability or supply-chain gap, fixed without touching frozen names. |
| TS-D22 | **Second production-readiness pass (2026-10-06).** Post-sign-off living-document maintenance. No frozen name (§25) or resolved TSQ (§16) changes; new items are additive: a table, a migration, an error code, env vars, Helm values and render-time checks. **RP-17 durability:** the rotator sweep and the scheduler now process their batch tenant by tenant, calling RP-17 inline after each tenant's commits and starting a tenant only if `rowReserve` + `rp17Timeout` (30s) of budget is left. A durable marker in the new `keys_refresh_pending` table (migration `000005`) is written in the revoke transaction (sweep) or before `IssueOrRotate` (scheduler, intent-first). It is cleared only after RP-17 succeeds, and only when no newer request arrived. Every run first retries owed refreshes, using at most half its budget (TS-D23 refines which markers are eligible). Both binaries handle SIGTERM: they stop starting tenants and finish the current one, including its RP-17. The TS-D15 two-halves gap therefore self-heals once the Realm Provisioner is healthy; a failed RP-17 still counts as failed and pages. The orphan reconciler and the prunes have their own budget checks; out-of-time work is deferred, not failed, and the reconciler starts at a per-run rotating offset. Orphan material is reclaimed under the principal row lock instead of the `MaxVersion` heuristic, so a principal with no rows is handled. **Locking:** SQLSTATE `55P03` maps to `409 rotation_in_flight` on every repository path (TS-4 `Register` and `LockByTenant` used to surface 500 / an unclassified error). TS-1 generates the keypair before the transaction and bounds the in-lock OpenBao write at 5s. **API:** a missing, repeated or non-UUID identity header is `401 missing_identity_headers` (§17), no longer the gateway library's generic body. TS-1 responses carry `Cache-Control: no-store`. A `rotation_id` replay is bounded by `ROTATION_REPLAY_WINDOW` (new `409 credential_replay_expired`, §9.2). **JWKS:** tenants with no live credential share a small bucket (`JWKS_RATE_LIMIT_UNKNOWN_TENANT_RPS`/`_BURST`, 2/5) and never drain the global one. Per-tenant buckets use LRU eviction. A fully unreadable key set is a 503 (superseded by TS-D23: `jwks_keys_unavailable`), and a cancelled request is not counted as a key error. **OpenBao:** login is single-flight, detached from the caller's context, with a token margin of `min(30s, lease/2)`. **Consumer:** a `TenantMembershipsPurged` with an invalid envelope id goes to the DLQ (`invalid_envelope_id`); unknown event-type labels fold to `other`. **Server:** startup fails in prod when docs are enabled without `DOCS_AUTH_TOKEN`. **Deployment:** outside local/dev/test the chart refuses to render without the Istio `AuthorizationPolicy` (the API's identity is a header any pod reaching the port can set; Keycloak must be in the mesh), or without `events.topicArn`/`sqs.queueUrl`. The migrate ServiceAccount, the chart-rendered Secret and a migrate NetworkPolicy are pre-install hooks (weight -10), so the first install no longer fails. CronJob grace is 45s, and Jobs opt out of sidecar injection (`cronjobs.istioInject`). `honorLabels` keeps the `service` label the alerts select on. SLO-3 counts runs with `max_over_time`. Alerts keep `environment`. Production schemas are registered from `release.yml` before the deploy gate. CI and release images get their real tags only after scan, sign and smoke. First-party actions are SHA-pinned. **Gate:** TS-INV-2's check also covers error constructors, span attributes, PEM/private-key names and `-----BEGIN` literals. | Second review pass over TS-D21's own changes and the deployment path; the RP-17 durability gap in particular left revoked keys trusted at Keycloak with no record. |
| TS-D23 | **Third production-readiness pass (2026-10-07).** Post-sign-off living-document maintenance. No frozen name (§25) or resolved TSQ (§16) changes: one additive error code, five metrics, one column on the unreleased `keys_refresh_pending` table, env vars and Helm values. `details.active_rotation_id` (§17, frozen) is kept and now also attached on the lock-timeout path (best effort, the last committed active credential). **JWKS:** the TS-D22 unknown-tenant bucket regressed real tenants, because "known" meant served-recently-on-this-replica, so after a restart or 10 idle minutes Keycloak's fetches shared 2 rps with any attacker. Known tenants are now every tenant with an active or rotating credential, listed from the database over the reconciler pool every `JWKS_KNOWN_TENANTS_REFRESH` (15s), plus tenants served since. The route returns the new `503 jwks_keys_unavailable` when the active key (or every key) is unreadable, instead of a 200 set without the active key or `secret_store_unavailable` (frozen at 502). 429s are labelled by bucket. **Refresh markers:** a concurrent rotator sweep could clear the scheduler's intent marker before its rotation committed. The scheduler now re-marks (committed) after `IssueOrRotate` and after a failed RP-17. Intent markers carry `intent_until` and are ignored by the retry pass until they expire; committed ones are retried on the next run (the global 4-minute minimum age is gone). Backlog is exported as `keys_refresh_pending` / `keys_refresh_oldest_age_seconds` and alerts above 15 minutes. **Locking:** the TS-2 and orphan-reclaim OpenBao calls under the principal lock are bounded at 5s, like TS-1. `missing_material` is confirmed under the lock, so an in-flight revoke no longer pages. The scheduler treats `principal_not_found` (offboarding race) as skipped. **Audit:** every `rotation_id` replay is logged and counted (`credential_replays_total`). **API:** `x-tenant-roles` (unused here) is dropped before the gateway library can 401 on it. Docs in every non-dev environment (staging included) are mounted only when enabled and require the token. Swagger requires both identity headers. `/readyz` checks have a 2s server-side deadline. **Consumer:** straight-to-DLQ sends are counted (`consumer_dlq_rejects_total{reason}`) and logged; `invalid_envelope_id` pages. **Deployment:** with the AuthorizationPolicy enabled, server and consumer pods are force-injected (native sidecars by default, so Envoy outlives the app's drain), a STRICT `PeerAuthentication` covers the server, and egress to istiod is allowed. NetworkPolicy egress also opens `postgresDirectPort` for the direct reconciler DSN. The hook Secret carries release-ownership annotations, and pods restart on a secret checksum change. Readiness timeout is 3s. **Alerting:** outage alerts carry static `environment` labels. `CronJobRunFailed` fires on the latest failed run. New `IAMTokenServiceOpenBaoErrors`, `IAMTokenServiceOffboardingInvalidEnvelopeRejected` and `IAMTokenServiceKeysRefreshBacklog`. `JWKSRateLimited` ignores the unknown bucket. **Release/CI:** pre-release tags skip production schema registration and the deploy gate. A dispatched release must run from its tag. CI smoke-tests the pushed digest before signing. The CI role gains `glue:DeleteSchema` and `cloudwatch:PutMetricAlarm`. | Third review pass; TS-D22's JWKS limiter was a regression for real tenants, and several new paths (markers, locks, sidecars) needed the same rigour as the code they protect. |

---

## 23. Appendix — Glossary

| Term | Meaning |
|---|---|
| Automation principal | The per-tenant `service_account`-typed `platform-automation` identity the platform acts as on a tenant's behalf (HLD §5.8). |
| `platform-automation` | The frozen base Keycloak client name for the automation principal, one per tenant, minted by the Realm Provisioner (§25). A dedicated-realm tenant's client is named exactly this; a shared-realm/trial tenant's client is this name suffixed with the tenant's UUID (rev 1.1). |
| Credential version | A monotonically-increasing `service_account_credentials.version` per principal; exactly one is `active` (§6.2). |
| Rotation overlap | The bounded window in which a superseded (`rotating`) key is still served in the JWKS and so still validates, `[0,900]s` (§6.2). |
| OpenBao | The self-hosted, Vault-API-compatible secret store; the only home for plaintext material (HLD §11.2). |
| OpenBao path | `iam/serviceaccount/<tenant_id>/<keycloak_client_id>/v<version>` — the deterministic KV v2 location (§6.3). |
| `iam-system` | The reserved system principal sub `…00a1` accepted only on `/api/v1/internal/*` (RLS-5). |
| RP-INV-1 | The Realm Provisioner invariant that RP is the sole Keycloak Admin API writer. |
| TS-INV-1 | This service's invariant that it never writes Keycloak (the mirror of RP-INV-1). |
| Two-halves handshake | The TS-generates-and-serves / RP-refreshes-Keycloak split in which neither service completes a credential transition alone (§6.1). |
| JWKS | The JSON Web Key Set this service serves per tenant (`…/platform-automation/jwks.json`); Keycloak's `client-jwt` authenticator fetches and caches it (EXT-6, §5.4). |
| RP-17 | The Realm Provisioner route that clears Keycloak's cached keys for a tenant's realm (`ClearServiceAccountKeysCache`), so Keycloak re-fetches the JWKS. |
| Refresh marker | A `keys_refresh_pending` row: a durable record that a tenant is owed an RP-17 call. *Committed* (`intent_until` NULL) or *intent* (written by `cmd/scheduler` before a rotation) (§8.8). |
| `sub` | The Keycloak subject UUID; here, the automation client's `principal_sub` supplied by RP. |

---

## 24. Appendix — Operational Runbooks

Per-alert runbooks live in `docs/observability/runbooks.md`; the entries below are the procedures behind them.

- **Rotate a tenant's credential on demand.** Call TS-1 with a fresh `rotation_id`; confirm RP called `ClearServiceAccountKeysCache` (RP-17) so Keycloak picks up the new key; watch `rotation_overlap_active` drop to zero after `expires_at` **and** confirm RP cleared the cache again at that point (the gauge dropping is this service's own bookkeeping, not proof Keycloak forgot the old key — EXT-6). A lost response is safe to retry under the same `rotation_id` (§8.2).
- **Break-glass: revoke a compromised key (two-halves, §8.7).** Do **not** stop at TS-2 — a TS-2 revoke does not stop the key authenticating at Keycloak until RP's cache-clear lands (TS-INV-7). Recommended order: (1) TS-1 rotate to mint a clean replacement; (2) TS-2 revoke the compromised version (deletes its OpenBao material, so it drops out of the next JWKS fetch); (3) have RP call `ClearServiceAccountKeysCache` so Keycloak actually forgets it. If the compromise demands the old key dead immediately, a hard cutover (`overlap_seconds=0`) still requires the same RP cache-clear call to take effect — there is no Keycloak-side TTL that does it automatically. Confirm at Keycloak that the old key no longer authenticates before closing the incident.
- **Stuck `rotating` version.** Check `cmd/rotator` is scheduled and healthy (`IAMTokenServiceRotatorNotRunning`, `…CronJobRunFailed`) and OpenBao is reachable; the next TS-1 for the principal also sweeps it (§8.3). If OpenBao deletes are failing, fix OpenBao first: the sweep deletes material before it commits, so the row stays `rotating` (already absent from the JWKS) until a delete succeeds.
- **OpenBao outage.** Issue/rotate returns `502`; do not force a Postgres-only credential row (it would have no material). Wait for OpenBao recovery, then retry the provisioning/rotation; the §8.6 reconciler reclaims any orphaned material on its next run. If the reconciler reports `missing_material` (a live row whose material is gone, confirmed under the principal lock), rotate that principal (TS-1, then RP-17) rather than trying to recover the lost key.
- **Offboarding DLQ drain.** Inspect `tenant-lifecycle-tokensvc-q-dlq`; the `DLQReason` attribute says why a message was sent there directly (`schema_violation`, `invalid_envelope_id`; none means `maxReceiveCount` was exhausted). Confirm the tenant is genuinely purged, fix the cause, then re-drive DLQ → main queue (idempotent, §9.2). An `invalid_envelope_id` message cannot be redriven as-is: it has no usable dedup key, so it must be re-published by the producer with a valid id. For a malformed event, capture it and file a Core tenant-lifecycle bug. Until then that tenant's credentials have not been erased (GDPR, §15.2).
- **Outbox backlog.** If `platform_outbox_pending_events` grows, check SNS/`iam-serviceaccount-events` health; the runner drains automatically on recovery. Credential operations are unaffected (EVT-4).
- **Secret-logging gate failure in CI.** A credential field name reached a log or `fmt` sink; remove it — no credential field may ever be a log attribute (§11.4).
- **`409 credential_replay_revoked` from TS-1.** The caller replayed a `rotation_id` whose credential has since been revoked (overlap-expiry, a TS-2 revoke, or offboarding) — the material no longer exists in OpenBao. This is expected for a stale idempotency key; the caller must issue a fresh `rotation_id` to rotate again, not retry the old one (§9.2, §17).
- **`networkPolicy.ingressNamespaceSelector is required` Helm render failure** (or `keycloakNamespaceSelector`, `monitoringNamespaceSelector`, `egress.postgresCIDRs`). Set real values in your values file — an empty selector or CIDR list is refused deliberately, it is not a bug in the chart (§10.2/§13.4).
- **`authorizationPolicy.enabled must be true` Helm render failure** (any `appEnv` other than local/dev/test). Enable `authorizationPolicy` and set `meshNamespaces` and `keycloakNamespaces`; Keycloak must be in the mesh. Without it any pod in Keycloak's namespace could call TS-1/TS-2/TS-4 as the system principal (§10.2). The same file also refuses a missing `events.topicArn`/`sqs.queueUrl` and enabled docs without `docs.authEnabled`.
- **RP-17 backlog (`IAMTokenServiceKeysRefreshBacklog`, `…CadenceRotationFailures`, `…RotationSweepFailures`).** The Realm Provisioner's RP-17 is failing for the tenants in the `RP-17 key-cache refresh still failing` logs. Fix RP or Keycloak; the next CronJob run retries every owed refresh and the gauge drains by itself (§8.8, §20.6). Do not re-rotate by hand: the rotation already committed. For an urgent break-glass case, ask RP to clear that realm's key cache directly.
- **`503 jwks_keys_unavailable` / `IAMTokenServiceJWKSKeyErrors`.** The tenant's active key (or every key) could not be read from OpenBao. Keycloak keeps its cached keys, so existing logins keep working until the cache is cleared. Check OpenBao health and the `jwks: skipping unreadable credential` log (tenant, version). If OpenBao is healthy and the material is genuinely gone, rotate the principal (TS-1, then RP-17).
- **`IAMTokenServiceJWKSRateLimited`.** A real tenant's or the global bucket is refusing Keycloak's fetches. Check `jwks_rate_limited_by_bucket_total{bucket}`: `tenant` means one tenant is fetching unusually often; `global` means overall load. Raise `jwks.*` only after ruling out a misbehaving client (§5.4).
- **`409 credential_replay_expired` from TS-1.** The `rotation_id` replay came later than `ROTATION_REPLAY_WINDOW` after the credential was issued; the key is not handed out again. The caller must issue a fresh `rotation_id` (a new rotation). Frequent served or expired replays (`credential_replays_total`) deserve an audit look (§9.2).
- **`IAMTokenServiceCronJobRunFailed`.** The latest rotator or scheduler run exited 1. Read that Job's logs for the failing step (sweep, retry pass, reconciler, a prune, or a cadence rotation); the per-run counters say which. Deferred work alone never fails a run.
- **`409 rotation_in_flight` with no concurrent caller you know of.** Another TS-1/TS-2, the scheduler or the offboarding cascade held the principal's row lock longer than `PG_LOCK_TIMEOUT` (§9.1). Retry. If it persists, look for a long-held transaction (slow OpenBao calls inside it).
- **CronJob deferred rows.** `run budget nearly spent — deferring` in rotator or scheduler logs means a run hit its batch limit or its deadline reserve; the next run continues. A backlog that never drains needs a higher `ROTATOR_BATCH_LIMIT`/`SCHEDULER_BATCH_LIMIT` or a longer `runTimeout` (TS-D21).
- **`platform_db_pool_empty_acquires_total` growth alert.** A pool (`pool="default"` app pool or `pool="reconciler"`) is exhausted — requests are waiting for a connection. Check `platform_db_pool_connections{state="acquired"}` vs `platform_db_pool_max_connections` and slow-query/long-held-transaction logs before raising `PG_MAX_CONNS` (§11.5).

---

## 25. Appendix — Name Inventory (proposed freeze)

*Frozen at design sign-off v1.0 (2026-09-15, TSQ-4 Resolved / §16.1); a change to any name below after this date is a breaking change under the §7 schema-evolution discipline.*

- **Postgres tables (2 tenant-scoped + 3 operational):** `service_account_principals`, `service_account_credentials`; `outbox_events`, `processed_events`, `schema_migrations`; added post-freeze (additive): operational `rls_violation_log` (TS-D18) and `keys_refresh_pending` (TS-D22, column `intent_until` TS-D23).
- **Enum types + values (3):** `principal_type` (`platform_automation`; post-launch adds `tenant_bot`, `user_pat`), `principal_status` (`active`, `revoked`), `credential_status` (`active`, `rotating`, `revoked`).
- **Keycloak client name (frozen base name, shared with RP §25):** `platform-automation` — the `service_account`-typed automation client, one per tenant; dedicated-realm tenants use it literally, shared-realm/trial tenants suffix it with the tenant's UUID (rev 1.1).
- **Published events (5) on `iam-serviceaccount-events`:** `ServiceAccountRegistered`, `ServiceAccountCredentialIssued`, `ServiceAccountCredentialRotated`, `ServiceAccountCredentialRevoked`, `ServiceAccountRevoked` — payload fields per §7.5; **no payload carries a secret**.
- **Consumed events / queues:** `tenant-lifecycle-tokensvc-q` (filter `[TenantMembershipsPurged]`), `-dlq`, `maxReceiveCount=5`; `processed_events.consumer` = `tenant_offboarding`.
- **Endpoint paths (frozen) — all under `/api/v1/internal`:** TS-1 `POST …/tenants/:id/service-accounts/:principal_id/credentials`, TS-2 `POST …/credentials/:version/revoke`, TS-3 `GET …/tenants/:id/service-accounts/:principal_id`, TS-4 `POST …/tenants/:id/service-accounts`; added post-freeze (additive): the EXT-6 JWKS route `GET …/tenants/:id/service-accounts/platform-automation/jwks.json` (rev 1.3), TS-5 `GET …/tenants/:id/service-accounts?principal_sub=` (TS-D16), TS-6 `GET …/tenants/:id/service-accounts/platform-automation` (TS-D17). Unversioned infra/docs routes stay unversioned.
- **OpenBao path shape (frozen):** `iam/serviceaccount/<tenant_id>/<keycloak_client_id>/v<version>`.
- **Metric prefix (frozen):** `iam_token_service_*`.
- **Glue registry / SNS topic (frozen):** `iam-serviceaccount-events`.
- **Added post-freeze — additive, not frozen** (recorded here for completeness; each can still change through normal living-document maintenance):
  - error codes beyond the frozen §17 set: `db_unavailable`, `credential_replay_revoked`, `credential_replay_expired`, `jwks_keys_unavailable`, `rate_limited`, `tenant_path_mismatch`, `unsupported_media_type`, `internal_error` (§17);
  - consumer `DLQReason` values `schema_violation` and `invalid_envelope_id` (§7.1);
  - the `iam_token_service_*` metrics added under the frozen prefix and the Tier 2 `iam_rls_violations_total` (§11.2, generated list in `docs/observability/metric-registry.md`);
  - migrations `000002`–`000005` and the database functions `log_rls_violation()` / `prune_rls_violation_log()` (§4.4).
