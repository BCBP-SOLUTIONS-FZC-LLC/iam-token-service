#!/usr/bin/env bash
# Floci ready-hook — provisions the full messaging topology this service
# needs for local dev per LLD §7.1/§7.3.1/§25 (frozen names). Runs
# automatically on container start via the volume mount to
# /etc/floci/init/ready.d/.
#
# Creates:
#   2 SNS topics    — iam-serviceaccount-events (this service's own, single-
#                     producer topic, §25), iam-tenant-events (stands in for
#                     Core's real topic, so this service's one inbound
#                     subscription has something to receive from locally)
#   1 inbound SQS   — tenant-lifecycle-tokensvc-q (+ DLQ, maxReceiveCount=5)
#                     subscribed to iam-tenant-events, filtered to
#                     EventType=TenantMembershipsPurged (§7.1)
#   1 fan-out SQS   — serviceaccount-audit-q (+ DLQ) subscribed to
#                     iam-serviceaccount-events, catch-all (no filter) —
#                     stands in for Audit Log, this service's one consumer
#                     (§7.4, audit-only)
#   1 Glue registry — iam-serviceaccount-events (§25)
#   5 Glue schemas  — this service's 5 produced events, registered from the
#                     same JSON Schema Draft-07 files eventbus.GlueCodec
#                     ships (internal/adapter/outbound/eventbus/schemas/*.json), so
#                     GLUE_REGISTRY_NAME/SNS_TOPIC_SERVICEACCOUNT_ARN can be
#                     set in the app service's environment and it runs with
#                     the real Glue wire-format codec locally — Floci
#                     includes Glue Schema Registry in its free tier (unlike
#                     LocalStack Community, which gates it behind Pro), so
#                     there's no NoopCodec fallback needed for local dev.
#
# The docker-compose `floci` service mounts this repo's internal/adapter/outbound/eventbus/schemas/
# directory read-only at /etc/floci/init/schemas so this script can read the
# schema definitions straight from source — one place to update when a
# schema changes.

set -euo pipefail

AWS_REGION=ap-south-1
AWS_ACCOUNT=000000000000
MAX_RECEIVES=5
SCHEMAS_DIR=/etc/floci/init/schemas

# The AWS CLI baked into the floci compat image defaults AWS_DEFAULT_REGION
# to us-east-1 regardless of the emulator's own FLOCI_DEFAULT_REGION — pin
# both region env vars so every `aws` call below lands in ap-south-1,
# matching FLOCI_DEFAULT_REGION/FLOCI_DEFAULT_ACCOUNT_ID on the container.
export AWS_DEFAULT_REGION="$AWS_REGION"
export AWS_REGION="$AWS_REGION"

# ────────────────────────────────────────────────────────────────────────
# Helpers
# ────────────────────────────────────────────────────────────────────────

topic_arn() { printf 'arn:aws:sns:%s:%s:%s' "$AWS_REGION" "$AWS_ACCOUNT" "$1"; }
queue_arn() { printf 'arn:aws:sqs:%s:%s:%s' "$AWS_REGION" "$AWS_ACCOUNT" "$1"; }

# create_queue_with_dlq <base-queue-name>
# Creates <base>-dlq then <base> with RedrivePolicy → <base>-dlq (max 5).
# Uses the JSON form of --attributes via a tempfile because the shorthand
# parser chokes on inline JSON values.
create_queue_with_dlq() {
  local queue="$1"
  local dlq="${queue}-dlq"

  aws sqs create-queue --queue-name "$dlq" >/dev/null
  local dlq_arn
  dlq_arn=$(queue_arn "$dlq")

  local attrs
  attrs=$(mktemp)
  cat > "$attrs" <<EOF
{
  "RedrivePolicy": "{\"deadLetterTargetArn\":\"${dlq_arn}\",\"maxReceiveCount\":\"${MAX_RECEIVES}\"}"
}
EOF
  aws sqs create-queue --queue-name "$queue" --attributes "file://$attrs" >/dev/null
  rm -f "$attrs"
}

# subscribe_queue <topic-name> <queue-name> [filter-policy-json]
# Creates an SNS→SQS subscription and applies a filter policy on the
# `EventType` MessageAttribute if one is provided.
subscribe_queue() {
  local topic="$1" queue="$2" filter="${3:-}"
  local t_arn q_arn
  t_arn=$(topic_arn "$topic")
  q_arn=$(queue_arn "$queue")

  local sub_arn
  sub_arn=$(aws sns subscribe \
    --topic-arn "$t_arn" \
    --protocol sqs \
    --notification-endpoint "$q_arn" \
    --attributes RawMessageDelivery=true \
    --query 'SubscriptionArn' --output text)

  if [ -n "$filter" ]; then
    local attrs
    attrs=$(mktemp)
    printf '%s' "$filter" > "$attrs"
    aws sns set-subscription-attributes \
      --subscription-arn "$sub_arn" \
      --attribute-name FilterPolicy \
      --attribute-value "file://$attrs" >/dev/null
    rm -f "$attrs"
  fi
}

# register_schema <registry> <schema-file> <schema-name>
# Idempotently registers schema-file's contents as a new schema in
# registry. DataFormat=JSON matches these files (JSON Schema Draft-07);
# Compatibility=BACKWARD mirrors the default schema-gov register uses
# against real AWS Glue.
register_schema() {
  local registry="$1" file="$2" name="$3"
  aws glue create-schema \
    --registry-id "RegistryName=${registry}" \
    --schema-name "$name" \
    --data-format JSON \
    --compatibility BACKWARD \
    --schema-definition "file://${SCHEMAS_DIR}/${file}" >/dev/null
}

# ────────────────────────────────────────────────────────────────────────
# SNS topics
# ────────────────────────────────────────────────────────────────────────

aws sns create-topic --name iam-serviceaccount-events >/dev/null
aws sns create-topic --name iam-tenant-events          >/dev/null

# ────────────────────────────────────────────────────────────────────────
# Inbound SQS — this service's one inbound subscription (§7.1)
# ────────────────────────────────────────────────────────────────────────

create_queue_with_dlq tenant-lifecycle-tokensvc-q

# tenant-lifecycle-tokensvc-q ← iam-tenant-events, filtered to
# TenantMembershipsPurged only (§7.1, §8.4 offboarding cascade).
subscribe_queue iam-tenant-events tenant-lifecycle-tokensvc-q \
  '{"EventType": ["TenantMembershipsPurged"]}'

# ────────────────────────────────────────────────────────────────────────
# Downstream fan-out — this service's own topic, one queue for its one
# consumer (Audit Log, audit-only per §7.4). Provisioning it here lets
# local dev / integration tests observe the produced-event fan-out even
# though Audit Log itself isn't part of this stack.
# ────────────────────────────────────────────────────────────────────────

create_queue_with_dlq serviceaccount-audit-q
subscribe_queue iam-serviceaccount-events serviceaccount-audit-q

# ────────────────────────────────────────────────────────────────────────
# Glue Schema Registry (§7.3.1, §25) — single-producer registry, populated
# from the exact JSON Schema Draft-07 files eventbus.GlueCodec reads in
# production, so local dev exercises the real Glue wire format instead of
# NoopCodec.
# ────────────────────────────────────────────────────────────────────────

aws glue create-registry --registry-name iam-serviceaccount-events >/dev/null

register_schema iam-serviceaccount-events service_account_registered.json          ServiceAccountRegistered
register_schema iam-serviceaccount-events service_account_credential_issued.json   ServiceAccountCredentialIssued
register_schema iam-serviceaccount-events service_account_credential_rotated.json  ServiceAccountCredentialRotated
register_schema iam-serviceaccount-events service_account_credential_revoked.json  ServiceAccountCredentialRevoked
register_schema iam-serviceaccount-events service_account_revoked.json             ServiceAccountRevoked

# ────────────────────────────────────────────────────────────────────────
# Summary
# ────────────────────────────────────────────────────────────────────────

topic_count=$(aws sns list-topics --query 'length(Topics)' --output text)
queue_count=$(aws sqs list-queues --query 'length(QueueUrls)' --output text)
sub_count=$(aws sns list-subscriptions --query 'length(Subscriptions)' --output text)
schema_count=$(aws glue list-schemas --registry-id RegistryName=iam-serviceaccount-events --query 'length(Schemas)' --output text)

echo "Floci init complete."
echo "  SNS topics:        ${topic_count} (expected 2)"
echo "  SQS queues+DLQs:   ${queue_count} (expected 4 = 2 pairs)"
echo "  SNS subscriptions: ${sub_count} (expected 2)"
echo "  Glue schemas:      ${schema_count} (expected 5)"
