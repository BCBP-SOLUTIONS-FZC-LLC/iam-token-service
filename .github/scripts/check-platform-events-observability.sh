#!/usr/bin/env bash
# Event publishing, consumption, and deduplication must pass only through
# platform-events, matching iam-org-membership / iam-user-profile (every
# binary: cmd/server, cmd/consumer, cmd/rotator, cmd/scheduler):
#
#   publishing   — events go into outbox_events via outbox.Enqueue (in
#                  publisher.go), then cmd/server's outbox.Runner relays
#                  them to SNS via events.NewSNSPublisher. No service code
#                  may call sns.Publish or sns.PublishBatch directly, nor
#                  INSERT INTO outbox_events directly.
#
#   consumption  — all inbound SQS messages are polled by platform-events'
#                  events.SQSConsumer (events.NewSQSConsumerWithClient).
#                  No service code may call sqs.ReceiveMessage,
#                  sqs.DeleteMessage, or sqs.ChangeMessageVisibility directly;
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

# ── SQS/SNS API bypasses ──────────────────────────────────────────────────────
offenders=$(grep -rnE \
  'sqs\.ReceiveMessage\(|sqs\.DeleteMessage\(|sqs\.ChangeMessageVisibility\(|sns\.Publish\(|sns\.PublishBatch\(' \
  --include='*.go' \
  --exclude='*_test.go' \
  cmd/ internal/ 2>/dev/null || true)

if [ -n "$offenders" ]; then
	echo "FAIL: direct SQS/SNS API calls bypass platform-events:"
	echo "  sqs.ReceiveMessage         → use events.NewSQSConsumerWithClient (polling, metrics, drain)"
	echo "  sqs.DeleteMessage          → platform-events consumer acks after handler returns nil"
	echo "  sqs.ChangeMessageVisibility → platform-events consumer extends visibility during processing"
	echo "  sns.Publish / PublishBatch → events go through outbox.Enqueue first;"
	echo "                               cmd/server's outbox.Runner relays via events.NewSNSPublisher"
	echo ""
	echo "$offenders"
	exit 1
fi

# ── SQL bypasses on platform-events-owned tables ──────────────────────────────
sql_offenders=$(grep -rnE \
  '"[^"]*INSERT[[:space:]]+INTO[[:space:]]+outbox_events|"[^"]*INSERT[[:space:]]+INTO[[:space:]]+processed_events' \
  --include='*.go' \
  --exclude='*_test.go' \
  cmd/ internal/ 2>/dev/null || true)

if [ -n "$sql_offenders" ]; then
	echo "FAIL: raw SQL INSERT on platform-events-owned tables bypasses platform-events:"
	echo "  outbox_events    → use outbox.Enqueue (via port.EventPublisher.Enqueue in a RunInTx transaction)"
	echo "  processed_events → use inbox.Store.Process (via port.Inbox.ProcessOnce)"
	echo ""
	echo "$sql_offenders"
	exit 1
fi

echo "OK: no direct sqs.ReceiveMessage/DeleteMessage/ChangeMessageVisibility/sns.Publish/raw-outbox-SQL in production code (cmd/, internal/)"
