# OpenBao ACL policy for the iam-token-service consumer workload (cmd/consumer).
#
# The consumer handles the TenantMembershipsPurged offboarding cascade: it
# enumerates every credential row for a tenant (list on metadata), reads
# the openbao_path from Postgres (not from OpenBao directly), deletes each
# individual credential's KV v2 metadata (delete on metadata), and then
# deletes the whole tenant subtree (delete on metadata). It never creates
# or updates secret data.
#
# Bind this policy to the OpenBao Kubernetes auth role that maps to the
# <fullname>-consumer ServiceAccount (serviceAccount.perWorkload: true,
# deploy/openbao/role.tf.example).

path "iam/data/serviceaccount/*" {
  capabilities = ["deny"]
}

path "iam/metadata/serviceaccount/*" {
  capabilities = ["delete", "list"]
}

# Token self-renewal (Kubernetes auth login tokens are short-lived and
# proactively refreshed by Client.token(), §10.5/TS-CONFIG-3).
path "auth/token/renew-self" {
  capabilities = ["update"]
}
