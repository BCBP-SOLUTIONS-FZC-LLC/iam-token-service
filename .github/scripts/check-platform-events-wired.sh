#!/usr/bin/env bash
# Positive assertion: every cmd binary and the key adapter layers wire
# events, outbox, and dedup through platform-events.
#
# This gate catches a binary or adapter added or refactored without the
# required platform-events wiring — absences the negative gate cannot detect.
#
#   CMD/SERVER
#     events.NewSNSPublisher(  — builds the SNS relay publisher through
#                               platform-events; SNS_TOPIC_ARN is the only
#                               direct-publish API this service touches, and
#                               only inside platform-events' own SNSPublisher
#     outbox.NewRunner(        — builds the outbox polling loop that drains
#                               outbox_events and calls SNSPublisher; must be
#                               present so produced events are eventually
#                               delivered (not stuck indefinitely in the table)
#
#   CMD/CONSUMER
#     events.NewSQSConsumerWithClient( — the SQS receive loop (platform-events
#                               handles polling, visibility extension, DLQ
#                               redrive, metrics, graceful drain)
#     eventcfg.LoadSQS(        — SQS configuration via platform-events'
#                               canonical config loader; ensures visibility
#                               timeout, handler timeout, concurrency, and
#                               queue-depth sampling all come from the same
#                               source (SQS_* env vars) as the library
#
#   CMD/ROTATOR
#     outbox.NewRunner(        — the rotator runs its own outbox.Runner
#                               (with a noop publisher) to drain any events
#                               the sweep or orphan reconciler enqueued;
#                               without it, events from batch revokes could
#                               sit in the outbox until the next server pod
#                               picks them up, widening the propagation gap
#     eventcfg.LoadOutbox(     — outbox configuration via platform-events'
#                               config loader (OUTBOX_* env vars)
#
#   CMD/SCHEDULER
#     eventbusadapter.New(     — initialises the enqueue-only publisher the
#                               scheduler injects into TxRunner, ensuring
#                               IssueOrRotate's ServiceAccountCredentialRotated
#                               events go into outbox_events (not direct SNS)
#
#   INTERNAL/ADAPTER/OUTBOUND/EVENTBUS
#     outbox.Enqueue(          — the single write path for all produced events;
#                               confirms that the Publisher calls platform-events'
#                               outbox.Enqueue (not hand-rolling the INSERT)
#
#   INTERNAL/ADAPTER/OUTBOUND/POSTGRES
#     inbox.NewStore(          — the dedup store; confirms that InboxRepository
#                               binds platform-events' inbox.Store to the
#                               processed_events table (not a hand-rolled INSERT)
#
# Grep searches the whole cmd/<binary>/ and internal/ subtrees so the check
# survives internal refactors that move wiring into helper files.
set -euo pipefail

FAIL=0

check_required() {
  local target="$1" pattern="$2" label="$3"
  if ! grep -rqE "$pattern" "$target" --include='*.go' --exclude='*_test.go' 2>/dev/null; then
    echo "FAIL: ${target}: missing ${label}"
    FAIL=1
  fi
}

echo "Checking that every binary and adapter wires events/outbox/dedup through platform-events (positive gate)..."

# ── cmd/server ─────────────────────────────────────────────────────────────────
check_required "cmd/server" \
  'events\.NewSNSPublisher\(' \
  "events.NewSNSPublisher (SNS relay publisher via platform-events — the only permitted direct-publish path)"
check_required "cmd/server" \
  'outbox\.NewRunner\(' \
  "outbox.NewRunner (outbox.Runner that drains outbox_events to SNS; without it events queue forever)"

# ── cmd/consumer ───────────────────────────────────────────────────────────────
check_required "cmd/consumer" \
  'events\.NewSQSConsumerWithClient\(' \
  "events.NewSQSConsumerWithClient (SQS receive loop via platform-events — polling, metrics, graceful drain)"
check_required "cmd/consumer" \
  'eventcfg\.LoadSQS\(' \
  "eventcfg.LoadSQS (SQS config via platform-events — visibility timeout, handler timeout, concurrency)"

# ── cmd/rotator ────────────────────────────────────────────────────────────────
check_required "cmd/rotator" \
  'outbox\.NewRunner\(' \
  "outbox.NewRunner (rotator's outbox.Runner drains sweep/reconciler events from outbox_events)"
check_required "cmd/rotator" \
  'eventcfg\.LoadOutbox\(' \
  "eventcfg.LoadOutbox (outbox config via platform-events — poll interval, concurrency, lease duration)"

# ── cmd/scheduler ──────────────────────────────────────────────────────────────
check_required "cmd/scheduler" \
  'eventbusadapter\.New\(' \
  "eventbusadapter.New (enqueue-only publisher wired into TxRunner — rotation events go to outbox_events, not direct SNS)"

# ── Adapter layers ─────────────────────────────────────────────────────────────
check_required "internal/adapter/outbound/eventbus" \
  'outbox\.Enqueue\(' \
  "outbox.Enqueue in eventbus adapter (all produced events go through platform-events' outbox.Enqueue, not a raw INSERT)"
check_required "internal/adapter/outbound/postgres" \
  'inbox\.NewStore\(' \
  "inbox.NewStore in postgres adapter (dedup via platform-events' inbox.Store bound to processed_events, not a raw INSERT)"

if [ "$FAIL" -ne 0 ]; then
  echo ""
  echo "FAIL: one or more cmd binaries or adapters are missing required platform-events wiring."
  echo "See CLAUDE.md §'API & events' and the §7/§8 event-flow descriptions."
  exit 1
fi
echo "OK: all cmd binaries (server consumer rotator scheduler) and event adapters wire through platform-events"
