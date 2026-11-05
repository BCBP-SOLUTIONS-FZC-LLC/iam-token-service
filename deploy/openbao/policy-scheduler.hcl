# OpenBao ACL policy for the iam-token-service scheduler workload (cmd/scheduler).
#
# The scheduler drives cadence-based credential rotation via
# CredentialService.IssueOrRotate (§16 TSQ-6 Resolved, TS-D14): it creates
# new credential material (create on data), updates existing material during
# a rotation (update on data), reads material for the rotation_id-replay
# idempotency check (read on data), and enumerates metadata for its
# per-principal rotation scan (list on metadata). It does not delete
# credential material directly (revokes go through the service layer which
# uses the app pool, not the reconciler pool).
#
# Bind this policy to the OpenBao Kubernetes auth role that maps to the
# <fullname>-scheduler ServiceAccount (serviceAccount.perWorkload: true,
# deploy/openbao/role.tf.example).

path "iam/data/serviceaccount/*" {
  capabilities = ["create", "update", "read"]
}

path "iam/metadata/serviceaccount/*" {
  capabilities = ["read", "list"]
}

# Token self-renewal (Kubernetes auth login tokens are short-lived and
# proactively refreshed by Client.token(), §10.5/TS-CONFIG-3).
path "auth/token/renew-self" {
  capabilities = ["update"]
}
