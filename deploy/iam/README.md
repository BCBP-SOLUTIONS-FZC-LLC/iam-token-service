# AWS IAM policies for iam-token-service

This directory holds the reference IAM policies for the service's IRSA
roles. The application does not create the roles itself; platform Terraform
manages them. `policy.tf.example` shows the wiring.

Each binary runs under its own Kubernetes ServiceAccount
(`serviceAccount.perWorkload: true`, the chart default:
`<fullname>-server`, `-consumer`, `-rotator`, `-scheduler`, `-migrate`).
Only two of them call AWS, so only two roles exist:

| Workload | Role | Policy |
|---|---|---|
| `cmd/server` | `iam-token-service-server` | `policy-server.json`: SNS publish (outbox relay) + Glue read (`GlueCodec` startup resolution) |
| `cmd/consumer` | `iam-token-service-consumer` | `policy-consumer.json`: offboarding queue consume + DLQ send/depth |
| `cmd/rotator`, `cmd/scheduler`, migrate Job | none | No AWS calls: they only enqueue outbox rows, which `cmd/server` publishes |

Wire each ARN into Helm through `serviceAccount.workloadAnnotations`:

```yaml
serviceAccount:
  create: true
  perWorkload: true
  workloadAnnotations:
    server:   {eks.amazonaws.com/role-arn: arn:aws:iam::ACCOUNT_ID:role/iam-token-service-server}
    consumer: {eks.amazonaws.com/role-arn: arn:aws:iam::ACCOUNT_ID:role/iam-token-service-consumer}
```

With `serviceAccount.perWorkload: false`, the server, consumer, rotator and
scheduler share one ServiceAccount (`<fullname>`), so they can only have one
role: attach both policies (the union) to it, trust
`system:serviceaccount:<namespace>:<fullname>`, and set it in
`serviceAccount.annotations`. The chart fails the render if
`workloadAnnotations` is set in that mode. The OpenBao role must bind the
shared name too (`deploy/openbao/role.tf.example`). The migrate hook Job
always keeps its own `<fullname>-migrate` ServiceAccount, with no role.

Each role's trust policy is bound to exactly one ServiceAccount in exactly
one namespace (`system:serviceaccount:<namespace>:<fullname>-<component>`).
Use one role per environment, so a staging pod can never assume the
production role.

These roles have **no** OpenBao permissions. OpenBao access uses a separate
authentication path: OpenBao's Kubernetes auth method, with a projected
ServiceAccount token whose audience is `openbao` (§10.5, TS-CONFIG-3). See
`deploy/openbao/`.

There is **no S3, KMS or CloudWatch Logs grant**. Credential material lives
only in OpenBao and Postgres. Logs go to stdout and are collected by the
cluster's log pipeline, not written to CloudWatch by the application.

## Grants breakdown

| Action | Role | Purpose | LLD ref |
|---|---|---|---|
| `sns:Publish` | server | Outbox → SNS `iam-serviceaccount-events` (this service's 5 published events) | §7.3.1 |
| `glue:GetSchemaByDefinition` (+ read-only siblings) on the registry and its `schema/<registry>/*` ARNs | server | `GlueCodec` resolves each schema's version ID once at startup by definition. Schema ARNs are `arn:aws:glue:<region>:<account>:schema/<registry>/<schema>`, not children of the registry ARN, so both resources are needed | §13.4 |
| `sqs:ReceiveMessage` / `DeleteMessage` / `GetQueueAttributes` / `ChangeMessageVisibility` | consumer | The single inbound consumer on the offboarding queue (`TenantMembershipsPurged`, §7.1). `GetQueueAttributes` also feeds `platform_queue_depth` and reads the `RedrivePolicy` that names the DLQ | §7.1 |
| `sqs:SendMessage` / `GetQueueAttributes` on the DLQ | consumer | `cmd/consumer`'s DLQ router sends a permanently rejected `TenantMembershipsPurged` straight to the DLQ (`DLQReason=schema_violation` or `invalid_envelope_id`); `GetQueueAttributes` feeds `platform_dlq_depth`. The consumer never receives from the DLQ: redrive and triage are operator actions under their own role | §7.1, §11.5 |

## CI role (schema registry workflows)

`policy.tf.example` section 3 also defines a GitHub-OIDC role per environment
(`iam-token-service-ci-schema-registry-<env>`), assumable only from that
GitHub environment. It is not used by any pod.

| Action | Used by | Scope |
|---|---|---|
| Glue read + `CreateSchema` / `RegisterSchemaVersion` (and siblings) | `schema-registry.yml` register, diff, usage-check | the `iam-serviceaccount-events` registry and its `schema/<registry>/*` ARNs |
| `glue:DeleteSchema` | `schema-prune.yml` execute mode (`dry_run=false`, manual dispatch only) | same registry/schema ARNs, in its own `GlueSchemaPrune` statement |
| `cloudwatch:PutMetricData` | `schema-gov metrics` | `*` (the action has no resource-level permissions) |
| `cloudwatch:PutMetricAlarm` | `schema-gov metrics --alarm-sns-arn` (creates/updates its alarms) | alarms in this account and region (`alarm:*`); narrow to schema-gov's alarm-name prefix once confirmed |

Without `DeleteSchema`, a prune run in execute mode fails at the delete;
without `PutMetricAlarm`, the metrics step fails to
create its alarms.

## Least-privilege scoping

The SNS topic policy and the SQS queue policy should independently restrict
`Publish`/`ReceiveMessage` to these roles and to the actual publishing
service (Org & Membership, for `TenantMembershipsPurged`). Platform
infrastructure manages those policies; they are not represented here.

This service's `iam-serviceaccount-events` Glue registry is
**single-producer**: every schema name in it belongs to this service, so the
read-only Glue grants raise no cross-service exclusion concern.
