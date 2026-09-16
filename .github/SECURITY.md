# Security Policy

## Supported Versions

| Version | Supported          |
| ------- | ------------------ |
| 1.x     | :white_check_mark: |

## Reporting a Vulnerability

Email **vijay@bcbpsolutions.com** with the subject line:

```
[iam-token-service] Security vulnerability
```

Please include:
- A description of the vulnerability and its potential impact.
- Steps to reproduce (a minimal repro is ideal).
- Any relevant logs, request/response samples, or PoC code — **redact real
  credentials, OpenBao paths, or tenant identifiers** before sending.

Do not open a public GitHub issue for a suspected vulnerability.

### Response Timeline

| Milestone                     | Target             |
| ------------------------------ | ------------------- |
| Acknowledgment                 | Within 48 hours      |
| Initial severity assessment    | Within 5 business days |
| Patch for confirmed critical/high issues | Within 14 days |
| Public disclosure              | After a patch is released |

## Scope — Areas of Particular Sensitivity

This service is the sole custodian of the platform-automation service
account's rotating credential material. The following areas carry the
highest blast radius if broken and deserve extra scrutiny in any report or
review:

- **TS-INV-1 — no Keycloak Admin API dependency.** This service never talks
  to Keycloak; it manages its own bcrypt-hashed, versioned credential
  material independently. A dependency reintroducing a Keycloak Admin API
  call would be a significant architectural regression, not a feature.
- **TS-INV-2 — no secret ever reaches a log sink.** Credential plaintext,
  bcrypt hashes, and OpenBao tokens must never appear in a log line, error
  message, trace span, or metric label. See `.github/scripts/check-no-secret-log.sh`.
- **OpenBao custody (§6.3, §10.5, TS-CONFIG-3).** Credential material is
  written to OpenBao KV v2 at a deterministic, frozen path
  (`iam/serviceaccount/<tenant_id>/<keycloak_client_id>/v<version>`, §25)
  and never persisted in Postgres. Authentication to OpenBao is via the
  Kubernetes auth method (pod ServiceAccount JWT) — never a static token,
  never an AWS IAM action.
- **Row-Level Security / tenant isolation (RLS-6, §17.5).** Every
  tenant-scoped query must run under `SET LOCAL app.tenant_id` inside a
  transaction (`pgcommon.WithGUCSet`/`GUCSetFromContext`) — never a
  session-scoped `SET`, which would leak across tenants under pooled
  connections. See `.github/scripts/check-set-local-only.sh`.
- **Credential rotation overlap and idempotency (§6.2, §8.3, §9.2).** A bug
  in the overlap-window sweep or the `rotation_id`-replay path could revoke
  a still-valid credential early, or hand out inconsistent material on
  retry.
- **Offboarding cascade (`TenantMembershipsPurged` consumer, §7.1).** A bug
  here could fail to revoke credentials for an offboarded tenant, leaving
  live material behind in OpenBao.
- **Outbox event payloads.** Published events must never carry credential
  plaintext or bcrypt hashes — only metadata (principal id, tenant id,
  rotation id, version).
