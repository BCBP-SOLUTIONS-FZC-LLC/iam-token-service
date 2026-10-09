#!/usr/bin/env bash
# Event publishing, consumption, and deduplication must pass only through
# platform-events, matching iam-org-membership / iam-user-profile (every
# binary: cmd/server, cmd/consumer, cmd/rotator, cmd/scheduler):
#
#   publishing   — events go into outbox_events via outbox.Enqueue (in
#                  publisher.go), then cmd/server's outbox.Runner relays
#                  them to SNS via events.NewSNSPublisher. No service code
#                  may call sns.Publish or sns.PublishBatch directly.
#
#   consumption  — all inbound SQS messages are polled by platform-events'
#                  events.SQSConsumer (events.NewSQSConsumerWithClient).
#                  No service code may call sqs.ReceiveMessage directly;
#                  that would bypass the consumer loop's DLQ routing,
#                  metrics, span injection, visibility-extension, and
#                  graceful-drain logic.
#
#   dedup/inbox  — processed_events claims go through platform-events'
#                  inbox.Store (inbox.NewStore, inlet via
#                  port.Inbox.ProcessOnce → inbox.Store.Process).
#                  No service code may INSERT INTO processed_events directly.
#
# Explicitly ALLOWED direct AWS SDK calls (not bypasses):
#   sqs.SendMessage        — DLQ routing (cmd/consumer/dlq.go): platform-events
#                            has no "permanent reject to DLQ" API; the router
#                            intercepts handler errors before the consumer acks,
#                            sends to the RedrivePolicy DLQ, and acks the source
#                            message — it is NOT a receive-loop replacement.
#   sqs.GetQueueAttributes — DLQ URL resolution (dlq.go): reads RedrivePolicy
#                            to locate the DLQ endpoint at startup.
#   glue.GetSchemaByDefinition — Glue codec startup (glue_codec.go): resolves
#                            schema version UUIDs once at boot via the Glue SDK;
#                            platform-events defines events.Codec but does not
#                            implement the Glue wire format.
#
# This is the NEGATIVE gate. The companion check-platform-events-wired.sh is
# the POSITIVE gate that asserts every binary wires the required platform-events
# functions.
set -euo pipefail

echo "Checking events/outbox/dedup go through platform-events (negative gate)..."

offenders=$(grep -rnE \
  'sqs\.ReceiveMessage\(|sns\.Publish\(|sns\.PublishBatch\(' \
  --include='*.go' \
  --exclude='*_test.go' \
  cmd/ internal/ 2>/dev/null || true)

if [ -n "$offenders" ]; then
	echo "FAIL: direct SQS/SNS API calls bypass platform-events:"
	echo "  sqs.ReceiveMessage → use events.NewSQSConsumerWithClient (handles polling,"
	echo "                        DLQ routing, visibility extension, metrics, drain)"
	echo "  sns.Publish / sns.PublishBatch → events go through outbox.Enqueue first;"
	echo "                        cmd/server's outbox.Runner relays them via events.NewSNSPublisher"
	echo ""
	echo "$offenders"
	exit 1
fi
echo "OK: no direct sqs.ReceiveMessage/sns.Publish in production code (cmd/, internal/)"
