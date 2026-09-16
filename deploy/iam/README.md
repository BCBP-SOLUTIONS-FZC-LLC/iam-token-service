# AWS IAM policy for iam-token-service

This directory holds the reference IAM policy that must be attached to the
service's IRSA role. The application does not create the role itself — that is
managed by platform Terraform. Copy the JSON in `policy.json` into the
Terraform module or apply it directly via `aws iam create-policy` /
`aws iam put-role-policy`.

The role is assumed by the service's Kubernetes ServiceAccount via IRSA
(OIDC — no long-lived keys, §13.5). Wire the ARN into Helm via
`serviceAccount.annotations`:

```yaml
serviceAccount:
  create: true
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::ACCOUNT_ID:role/iam-token-service
```

The SAME role is assumed by all three workloads this chart creates: the
`cmd/server` Deployment (publishes to the `iam-serviceaccount-events` SNS
topic; reads the Glue Schema Registry), the `cmd/consumer` Deployment
(consumes the offboarding queue — enqueue-only, no AWS credentials strictly
required beyond SQS receive/delete), and the `cmd/rotator` CronJob (no AWS
calls at all today, but shares the ServiceAccount/role with the other two
per §13.1's "one ServiceAccount, shared by every pod this chart creates").

Note: this role has **no** OpenBao permissions — OpenBao access is a
separate authentication path (Kubernetes auth method against the same
ServiceAccount's projected JWT, not IRSA/STS, §10.5, TS-CONFIG-3). See
`deploy/openbao/`.

Unlike a sibling IAM service with a realm-export step, this service has
**no S3 or KMS grant** — it has no artifact to write to S3 (TS-1..TS-4 issue/
rotate/revoke credential material that lives entirely in OpenBao and
Postgres, never S3).

## Grants breakdown

| Action | Purpose | LLD ref |
|---|---|---|
| `sns:Publish` | Outbox → SNS `iam-serviceaccount-events` topic (this service's 5 published events) | §7.3.1 |
| `sqs:ReceiveMessage` / `DeleteMessage` / `GetQueueAttributes` / `ChangeMessageVisibility` | The single inbound consumer on the offboarding queue (`TenantMembershipsPurged`, §7.1) | §7.1 |
| `sqs:GetQueueAttributes` / `ReceiveMessage` on the offboarding DLQ | Ops visibility into DLQ depth — read-only for triage, not a consume-and-delete grant | §11.5 |
| `glue:GetSchemaVersion` (+ read-only siblings) | `GlueCodec` pre-fetches schema version IDs at startup, refreshed periodically | §13.4 |
| `logs:CreateLogStream` / `PutLogEvents` | Container stdout to CloudWatch (if not using an OTel collector for logs) | — |

## Least-privilege scoping

The SNS topic policy and the SQS queue policy should independently
restrict `Publish`/`ReceiveMessage` to this role and to the actual
publishing service (Org & Membership, for `TenantMembershipsPurged`)
respectively — that is managed by platform infrastructure and is not
represented here.

This service's `iam-serviceaccount-events` Glue registry is
**single-producer** (unlike a sibling service's shared registry) — every
schema name in it belongs to this service, so there is no cross-service
exclusion concern for the read-only Glue grants above.
