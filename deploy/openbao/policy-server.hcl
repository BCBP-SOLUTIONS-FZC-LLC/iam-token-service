# OpenBao ACL policy for the iam-token-service server workload (cmd/server).
#
# The server issues credentials (TS-1: create/update on data), serves the
# JWKS route (EXT-6: read on data for key material), and manages credential
# metadata during rotation (delete on metadata for cleanup). It does NOT
# perform cross-tenant enumeration (no list on metadata across all paths).
#
# Bind this policy to the OpenBao Kubernetes auth role that maps to the
# <fullname>-server ServiceAccount (serviceAccount.perWorkload: true,
# deploy/openbao/role.tf.example).

path "iam/data/serviceaccount/*" {
  capabilities = ["create", "update", "read"]
}

path "iam/metadata/serviceaccount/*" {
  capabilities = ["read", "list", "delete"]
}

# Token self-renewal (Kubernetes auth login tokens are short-lived and
# proactively refreshed by Client.token(), §10.5/TS-CONFIG-3).
path "auth/token/renew-self" {
  capabilities = ["update"]
}
