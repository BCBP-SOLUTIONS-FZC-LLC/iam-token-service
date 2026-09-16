package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
)

// TestMain registers this service's business metrics once for the whole
// package's test binary (idempotent, sync.Once-guarded) — recordSweepMetric
// and recordMaterialReconcileMetric would otherwise panic against the nil
// collectors metrics.Register populates.
func TestMain(m *testing.M) {
	metrics.Register("test")
	m.Run()
}

// ── envOr / envInt / envDuration / isDevEnv / mustEnv ──────────────────

func TestEnvOr(t *testing.T) {
	t.Run("returns the set value", func(t *testing.T) {
		t.Setenv("TS_TEST_ENVOR", "value")
		assert.Equal(t, "value", envOr("TS_TEST_ENVOR", "default"))
	})
	t.Run("falls back to default when unset", func(t *testing.T) {
		t.Setenv("TS_TEST_ENVOR", "")
		assert.Equal(t, "default", envOr("TS_TEST_ENVOR", "default"))
	})
}

func TestEnvInt(t *testing.T) {
	t.Run("parses a valid positive int", func(t *testing.T) {
		t.Setenv("TS_TEST_ENVINT", "42")
		assert.Equal(t, 42, envInt("TS_TEST_ENVINT", 7))
	})
	t.Run("falls back on unset", func(t *testing.T) {
		t.Setenv("TS_TEST_ENVINT", "")
		assert.Equal(t, 7, envInt("TS_TEST_ENVINT", 7))
	})
	t.Run("falls back on unparseable value", func(t *testing.T) {
		t.Setenv("TS_TEST_ENVINT", "not-a-number")
		assert.Equal(t, 7, envInt("TS_TEST_ENVINT", 7))
	})
	t.Run("falls back on non-positive value", func(t *testing.T) {
		t.Setenv("TS_TEST_ENVINT", "0")
		assert.Equal(t, 7, envInt("TS_TEST_ENVINT", 7))
		t.Setenv("TS_TEST_ENVINT", "-3")
		assert.Equal(t, 7, envInt("TS_TEST_ENVINT", 7))
	})
}

func TestEnvDuration(t *testing.T) {
	t.Run("parses a valid positive duration", func(t *testing.T) {
		t.Setenv("TS_TEST_ENVDUR", "5s")
		assert.Equal(t, 5*time.Second, envDuration("TS_TEST_ENVDUR", time.Minute))
	})
	t.Run("falls back on unset", func(t *testing.T) {
		t.Setenv("TS_TEST_ENVDUR", "")
		assert.Equal(t, time.Minute, envDuration("TS_TEST_ENVDUR", time.Minute))
	})
	t.Run("falls back on unparseable value", func(t *testing.T) {
		t.Setenv("TS_TEST_ENVDUR", "not-a-duration")
		assert.Equal(t, time.Minute, envDuration("TS_TEST_ENVDUR", time.Minute))
	})
	t.Run("falls back on non-positive duration", func(t *testing.T) {
		t.Setenv("TS_TEST_ENVDUR", "0s")
		assert.Equal(t, time.Minute, envDuration("TS_TEST_ENVDUR", time.Minute))
		t.Setenv("TS_TEST_ENVDUR", "-5s")
		assert.Equal(t, time.Minute, envDuration("TS_TEST_ENVDUR", time.Minute))
	})
}

func TestIsDevEnv(t *testing.T) {
	for _, env := range []string{"dev", "development", "local", "test"} {
		t.Run(env, func(t *testing.T) { assert.True(t, isDevEnv(env)) })
	}
	for _, env := range []string{"production", "staging", ""} {
		t.Run(env, func(t *testing.T) { assert.False(t, isDevEnv(env)) })
	}
}

func TestMustEnv(t *testing.T) {
	t.Run("returns the set value regardless of env", func(t *testing.T) {
		t.Setenv("TS_TEST_MUSTENV", "value")
		assert.Equal(t, "value", mustEnv("TS_TEST_MUSTENV", "production", "dev-default"))
	})
	t.Run("falls back to devDefault in a dev-like env", func(t *testing.T) {
		t.Setenv("TS_TEST_MUSTENV", "")
		assert.Equal(t, "dev-default", mustEnv("TS_TEST_MUSTENV", "dev", "dev-default"))
	})
	t.Run("panics outside dev when unset", func(t *testing.T) {
		t.Setenv("TS_TEST_MUSTENV", "")
		assert.Panics(t, func() { mustEnv("TS_TEST_MUSTENV", "production", "dev-default") })
	})
}

// ── recordSweepMetric / recordMaterialReconcileMetric ──────────────────

func TestRecordSweepMetric(t *testing.T) {
	before := testutil.ToFloat64(metrics.RotationSweepTotal.WithLabelValues("ok"))
	recordSweepMetric(sweepResult{Revoked: 3, Skipped: 1, Failed: 0})
	assert.Equal(t, before+1, testutil.ToFloat64(metrics.RotationSweepTotal.WithLabelValues("ok")))

	beforeErr := testutil.ToFloat64(metrics.RotationSweepTotal.WithLabelValues("error"))
	recordSweepMetric(sweepResult{Revoked: 1, Failed: 2})
	assert.Equal(t, beforeErr+1, testutil.ToFloat64(metrics.RotationSweepTotal.WithLabelValues("error")))
}

func TestRecordMaterialReconcileMetric(t *testing.T) {
	beforeOrphan := testutil.ToFloat64(metrics.MaterialReconcileTotal.WithLabelValues("orphan_deleted"))
	beforeMissing := testutil.ToFloat64(metrics.MaterialReconcileTotal.WithLabelValues("missing_material"))
	beforeOK := testutil.ToFloat64(metrics.MaterialReconcileTotal.WithLabelValues("ok"))
	beforeErr := testutil.ToFloat64(metrics.MaterialReconcileTotal.WithLabelValues("error"))

	recordMaterialReconcileMetric(materialReconcileResult{OrphanDeleted: 2, MissingMaterial: 1, OK: 5, Failed: 3})

	assert.Equal(t, beforeOrphan+2, testutil.ToFloat64(metrics.MaterialReconcileTotal.WithLabelValues("orphan_deleted")))
	assert.Equal(t, beforeMissing+1, testutil.ToFloat64(metrics.MaterialReconcileTotal.WithLabelValues("missing_material")))
	assert.Equal(t, beforeOK+5, testutil.ToFloat64(metrics.MaterialReconcileTotal.WithLabelValues("ok")))
	assert.Equal(t, beforeErr+3, testutil.ToFloat64(metrics.MaterialReconcileTotal.WithLabelValues("error")))
}

func TestRecordMaterialReconcileMetric_AllZeroRecordsNothing(t *testing.T) {
	// Every branch in recordMaterialReconcileMetric is guarded by "> 0" —
	// an all-zero result should be a complete no-op (no label ever
	// touched), exercising the false side of all four guards in one call.
	before := testutil.ToFloat64(metrics.MaterialReconcileTotal.WithLabelValues("ok"))
	recordMaterialReconcileMetric(materialReconcileResult{})
	assert.Equal(t, before, testutil.ToFloat64(metrics.MaterialReconcileTotal.WithLabelValues("ok")))
}

// ── logError ─────────────────────────────────────────────────────────────

type recordingLogger struct {
	msg    string
	fields map[string]any
	calls  int
}

func (l *recordingLogger) Debug(string, map[string]any) {}
func (l *recordingLogger) Info(string, map[string]any)  {}
func (l *recordingLogger) Warn(string, map[string]any)  {}
func (l *recordingLogger) Error(msg string, fields map[string]any) {
	l.calls++
	l.msg = msg
	l.fields = fields
}

func TestLogError_NilLoggerIsNoOp(t *testing.T) {
	assert.NotPanics(t, func() {
		logError(context.Background(), nil, "should be dropped", map[string]any{"a": 1}, errors.New("boom"))
	})
}

func TestLogError_NilFieldsMapIsInitialized(t *testing.T) {
	log := &recordingLogger{}
	logError(context.Background(), log, "op failed", nil, errors.New("boom"))
	require.Equal(t, 1, log.calls)
	assert.Equal(t, "op failed", log.msg)
	assert.Equal(t, "boom", log.fields["error"])
}

func TestLogError_ExistingFieldsPreserved(t *testing.T) {
	log := &recordingLogger{}
	logError(context.Background(), log, "op failed", map[string]any{"tenant_id": "t1"}, errors.New("boom"))
	require.Equal(t, 1, log.calls)
	assert.Equal(t, "t1", log.fields["tenant_id"])
	assert.Equal(t, "boom", log.fields["error"])
}

func TestFieldsWithTrace_BackgroundContextOmitsTraceID(t *testing.T) {
	got := fieldsWithTrace(context.Background(), map[string]any{"k": "v"})
	assert.Equal(t, map[string]any{"k": "v"}, got)
	assert.Nil(t, fieldsWithTrace(context.Background(), nil))
}

func TestStartJobSpan_ReturnsEndFn(t *testing.T) {
	ctx, end := startJobSpan(context.Background(), "iam-token-service-test", "overlap_sweep")
	require.NotNil(t, end)
	assert.NotPanics(t, end)
	assert.NotNil(t, ctx)

	ctx2, end2 := startJobSpan(context.Background(), "", "prune")
	require.NotNil(t, end2)
	assert.NotPanics(t, end2)
	assert.NotNil(t, ctx2)
}
