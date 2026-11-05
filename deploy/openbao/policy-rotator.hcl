# OpenBao ACL policy for the iam-token-service rotator workload (cmd/rotator).
#
# The rotator performs the overlap-expiry sweep (reads and deletes credential
# material for expired rotating credentials), the orphan-material reconciler
# (enumerates OpenBao paths under each principal's prefix, reads and deletes
# orphans), and the retention prune. It never creates or updates secret data
# (cmd/rotator may not import service, per architecture.md RLS-7).
#
# Bind this policy to the OpenBao Kubernetes auth role that maps to the
# <fullname>-rotator ServiceAccount (serviceAccount.perWorkload: true,
# deploy/openbao/role.tf.example).

path "iam/data/serviceaccount/*" {
  capabilities = ["delete", "list"]
}

path "iam/metadata/serviceaccount/*" {
  capabilities = ["read", "delete", "list"]
}

# Token self-renewal (Kubernetes auth login tokens are short-lived and
# proactively refreshed by Client.token(), §10.5/TS-CONFIG-3).
path "auth/token/renew-self" {
  capabilities = ["update"]
}
