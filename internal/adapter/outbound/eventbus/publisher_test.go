package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// fakeTx implements pgx.Tx by embedding the nil interface (any method
// other than Exec panics if called) and recording every Exec call —
// sufficient because outbox.Enqueue's InsertRecord calls only tx.Exec
// (verified by reading platform-events' outboxstore.InsertRecord source).
type fakeTx struct {
	pgx.Tx
	execCalls [][]any
	execErr   error
}

func (f *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.execCalls = append(f.execCalls, append([]any{sql}, args...))
	if f.execErr != nil {
		return pgconn.CommandTag{}, f.execErr
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

type failingCodec struct{ err error }

func (f failingCodec) Encode(context.Context, string, json.RawMessage) ([]byte, string, error) {
	return nil, "", f.err
}

func (f failingCodec) Decode(context.Context, string, []byte) (json.RawMessage, error) {
	return nil, f.err
}

func newTestEvent(tenantID uuid.UUID) *domain.Event {
	return &domain.Event{
		Type:     domain.EventServiceAccountRegistered,
		TenantID: tenantID,
		Actor:    domain.SystemPrincipalID,
		Data: domain.ServiceAccountRegisteredPayload{
			TenantID: tenantID, PrincipalID: uuid.New(), PrincipalSub: uuid.New(),
			KeycloakClientID: "platform-automation", PrincipalType: "platform_automation",
			CreatedAt: "2026-01-01T00:00:00Z",
		},
	}
}

func TestPublisher_New_NilCodecBecomesNoop(t *testing.T) {
	p := New("iam-token-service", nil)
	require.NotNil(t, p)
	err := p.Enqueue(port.WithTx(context.Background(), &fakeTx{}), newTestEvent(uuid.New()))
	require.NoError(t, err)
}

func TestPublisher_Enqueue_HappyPath_InsertsIntoOutbox(t *testing.T) {
	p := New("iam-token-service", NoopCodec{})
	tx := &fakeTx{}

	err := p.Enqueue(port.WithTx(context.Background(), tx), newTestEvent(uuid.New()))
	require.NoError(t, err)
	require.Len(t, tx.execCalls, 1, "exactly one INSERT into outbox_events")

	call := tx.execCalls[0]
	sql, ok := call[0].(string)
	require.True(t, ok)
	assert.Contains(t, sql, "INSERT INTO outbox_events")
}

func TestPublisher_Enqueue_RequiresOpenTx(t *testing.T) {
	p := New("iam-token-service", NoopCodec{})
	err := p.Enqueue(context.Background(), newTestEvent(uuid.New()))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RunInTx")
}

// TestPublisher_Enqueue_CodecRejectionNeverTouchesOutbox confirms a
// validation failure returns before any Exec call — an invalid payload
// must never reach outbox_events.
func TestPublisher_Enqueue_CodecRejectionNeverTouchesOutbox(t *testing.T) {
	wantErr := errors.New("schema validation failed")
	p := New("iam-token-service", failingCodec{err: wantErr})
	tx := &fakeTx{}

	err := p.Enqueue(port.WithTx(context.Background(), tx), newTestEvent(uuid.New()))
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.Empty(t, tx.execCalls, "a codec-rejected payload must never reach the outbox insert")
}

func TestPublisher_Enqueue_ExecFailurePropagates(t *testing.T) {
	p := New("iam-token-service", NoopCodec{})
	execErr := errors.New("connection reset")
	tx := &fakeTx{execErr: execErr}

	err := p.Enqueue(port.WithTx(context.Background(), tx), newTestEvent(uuid.New()))
	require.Error(t, err)
	assert.ErrorIs(t, err, execErr)
}

func TestPublisher_WithLogger_ReturnsSamePublisherForChaining(t *testing.T) {
	p := New("iam-token-service", NoopCodec{})
	got := p.WithLogger(nil)
	assert.Same(t, p, got)
}

// TestPublisher_WithLogger_RoutesExecFailureLogThroughAttachedLogger
// confirms WithLogger isn't merely a no-op setter — the attached logger is
// what Enqueue's warn call on an outbox Exec failure actually writes
// through.
func TestPublisher_WithLogger_RoutesExecFailureLogThroughAttachedLogger(t *testing.T) {
	fl := &fakeWarnLogger{}
	p := New("iam-token-service", NoopCodec{}).WithLogger(fl)
	tx := &fakeTx{execErr: errors.New("connection reset")}

	require.Error(t, p.Enqueue(port.WithTx(context.Background(), tx), newTestEvent(uuid.New())))
	require.Equal(t, 1, fl.count(), "the attached logger, not a nil no-op, must receive the enqueue failure")
}

// TestPublisher_Enqueue_NilLoggerOnExecFailureDoesNotPanic covers warn's
// nil-logger guard directly through the public Enqueue failure path.
func TestPublisher_Enqueue_NilLoggerOnExecFailureDoesNotPanic(t *testing.T) {
	p := New("iam-token-service", NoopCodec{})
	tx := &fakeTx{execErr: errors.New("connection reset")}
	assert.NotPanics(t, func() {
		_ = p.Enqueue(port.WithTx(context.Background(), tx), newTestEvent(uuid.New()))
	})
}

// TestPublisher_Enqueue_MarshalErrorPropagates covers Enqueue's very first
// error branch — an event.Data value json.Marshal cannot encode (a
// channel, here) must surface as an error rather than reach the codec or
// the outbox insert.
func TestPublisher_Enqueue_MarshalErrorPropagates(t *testing.T) {
	p := New("iam-token-service", NoopCodec{})
	tx := &fakeTx{}

	event := &domain.Event{
		Type:     domain.EventServiceAccountRevoked,
		TenantID: uuid.New(),
		Data:     make(chan int),
	}

	err := p.Enqueue(port.WithTx(context.Background(), tx), event)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "marshal event payload")
	assert.Empty(t, tx.execCalls)
}

// TestPublisher_Enqueue_ValidSpanContextPropagatesTraceAndCorrelationID
// covers the trace-context branch — when the caller's ctx carries a valid
// OTel span, Enqueue must stamp both TraceID and CorrelationID (the span
// ID) onto the envelope so a produced event is traceable back to the
// request that caused it.
func TestPublisher_Enqueue_ValidSpanContextPropagatesTraceAndCorrelationID(t *testing.T) {
	p := New("iam-token-service", NoopCodec{})
	tx := &fakeTx{}

	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	require.NoError(t, err)
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	require.NoError(t, p.Enqueue(port.WithTx(ctx, tx), newTestEvent(uuid.New())))
	require.Len(t, tx.execCalls, 1)

	var envelope struct {
		TraceID       string `json:"trace_id"`
		CorrelationID string `json:"correlation_id"`
	}
	payloadArg, ok := tx.execCalls[0][3].(json.RawMessage)
	require.True(t, ok, "expected the marshaled envelope bytes as the payload arg")
	require.NoError(t, json.Unmarshal(payloadArg, &envelope))
	assert.Equal(t, traceID.String(), envelope.TraceID)
	assert.Equal(t, spanID.String(), envelope.CorrelationID)
}

// TestPublisher_Enqueue_InvalidSpanContextOmitsTraceFields covers the
// plain-context path (no valid span) — TraceID/CorrelationID must be left
// unset on the envelope, not zero-valued garbage.
func TestPublisher_Enqueue_InvalidSpanContextOmitsTraceFields(t *testing.T) {
	p := New("iam-token-service", NoopCodec{})
	tx := &fakeTx{}

	require.NoError(t, p.Enqueue(port.WithTx(context.Background(), tx), newTestEvent(uuid.New())))
	require.Len(t, tx.execCalls, 1)

	var envelope struct {
		TraceID       string `json:"trace_id"`
		CorrelationID string `json:"correlation_id"`
	}
	payloadArg, ok := tx.execCalls[0][3].(json.RawMessage)
	require.True(t, ok)
	require.NoError(t, json.Unmarshal(payloadArg, &envelope))
	assert.Empty(t, envelope.TraceID)
	assert.Empty(t, envelope.CorrelationID)
}

// TestPublisher_Enqueue_SuccessWithLoggerNoSpanLogsDebugWithoutTraceID covers
// debug's log!=nil, no-valid-span branch — never previously exercised since
// every prior test either attached no logger or hit the exec-failure (warn)
// path instead of the success (debug) path.
func TestPublisher_Enqueue_SuccessWithLoggerNoSpanLogsDebugWithoutTraceID(t *testing.T) {
	fl := &fakeFieldLogger{}
	p := New("iam-token-service", NoopCodec{}).WithLogger(fl)
	tx := &fakeTx{}

	require.NoError(t, p.Enqueue(port.WithTx(context.Background(), tx), newTestEvent(uuid.New())))
	require.Len(t, fl.debugCalls, 1)
	assert.NotContains(t, fl.debugCalls[0], "trace_id")
}

// TestPublisher_Enqueue_SuccessWithLoggerAndValidSpanLogsDebugWithTraceID
// covers debug's trace_id-stamping branch.
func TestPublisher_Enqueue_SuccessWithLoggerAndValidSpanLogsDebugWithTraceID(t *testing.T) {
	fl := &fakeFieldLogger{}
	p := New("iam-token-service", NoopCodec{}).WithLogger(fl)
	tx := &fakeTx{}

	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	require.NoError(t, err)
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	require.NoError(t, p.Enqueue(port.WithTx(ctx, tx), newTestEvent(uuid.New())))
	require.Len(t, fl.debugCalls, 1)
	assert.Equal(t, traceID.String(), fl.debugCalls[0]["trace_id"])
}

// TestPublisher_Enqueue_ExecFailureWithLoggerAndValidSpanLogsWarnWithTraceID
// covers warn's trace_id-stamping branch — the other three prior tests only
// exercised the nil-logger and no-span sub-paths.
func TestPublisher_Enqueue_ExecFailureWithLoggerAndValidSpanLogsWarnWithTraceID(t *testing.T) {
	fl := &fakeFieldLogger{}
	p := New("iam-token-service", NoopCodec{}).WithLogger(fl)
	tx := &fakeTx{execErr: errors.New("connection reset")}

	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	require.NoError(t, err)
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	require.Error(t, p.Enqueue(port.WithTx(ctx, tx), newTestEvent(uuid.New())))
	require.Len(t, fl.warnCalls, 1)
	assert.Equal(t, traceID.String(), fl.warnCalls[0]["trace_id"])
}

func TestPublisher_Enqueue_MarshalsDataAsPayloadJSON(t *testing.T) {
	p := New("iam-token-service", NoopCodec{})
	tx := &fakeTx{}

	tenantID := uuid.New()
	event := newTestEvent(tenantID)
	require.NoError(t, p.Enqueue(port.WithTx(context.Background(), tx), event))
	require.Len(t, tx.execCalls, 1)

	// The payload argument is the 3rd positional arg after sql ($1=id,
	// $2=event_type, $3=payload, ...) per outboxstore.InsertRecord.
	call := tx.execCalls[0]
	payloadArg, ok := call[3].(json.RawMessage)
	require.True(t, ok, "expected the marshaled envelope bytes as the payload arg")

	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(payloadArg, &envelope))
	assert.Equal(t, domain.EventServiceAccountRegistered, envelope.Type)
	assert.Contains(t, string(envelope.Data), "platform-automation")
}
