# OpenBao (Vault-API-compatible) ACL policy for iam-token-service.
#
# Grants access ONLY under this service's own deterministic, frozen
# per-tenant/per-version credential path (§6.3, §25): the KV v2 mount is
# "iam" (OPENBAO_KV_MOUNT) and the path shape is
# "iam/serviceaccount/<tenant_id>/<keycloak_client_id>/v<version>" — see
# internal/core/domain/credential.go's OpenBaoPathFor, which builds this
# path and never anything else.
#
# KV v2 exposes distinct logical sub-paths per secret operation:
#   data/<path>      — Write (create/update) and the rotation_id-replay
#                       Read (§9.2, port.SecretStore.Read — used ONLY by
#                       TS-1's idempotent-replay path, never TS-3).
#   metadata/<path>  — Delete (CUST-2: a full KV v2 metadata delete, not a
#                       soft/recoverable delete — TS-2 revoke, the §8.3
#                       overlap-expiry sweep, and the offboarding cascade
#                       all use this) and List (the §8.6 orphan-material
#                       reconciler enumerates OpenBao paths under a
#                       principal's tenant/client prefix).
#
# Do NOT widen this path pattern (e.g. to "iam/*") — this service has no
# other OpenBao-backed secret today. Any future path must be reviewed by
# the platform team (see .github/CODEOWNERS) alongside that change.

path "iam/data/serviceaccount/*" {
  capabilities = ["create", "read", "update"]
}

path "iam/metadata/serviceaccount/*" {
  capabilities = ["read", "delete", "list"]
}

# Token self-renewal (Kubernetes auth login tokens are short-lived and
# proactively refreshed by Client.token(), §10.5/TS-CONFIG-3).
path "auth/token/renew-self" {
  capabilities = ["update"]
}
