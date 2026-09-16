package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPingerFunc_Health(t *testing.T) {
	t.Run("delegates to the wrapped func on success", func(t *testing.T) {
		var p pingerFunc = func(context.Context) error { return nil }
		assert.NoError(t, p.Health(t.Context()))
	})
	t.Run("delegates to the wrapped func on error", func(t *testing.T) {
		want := errors.New("down")
		var p pingerFunc = func(context.Context) error { return want }
		assert.ErrorIs(t, p.Health(t.Context()), want)
	})
}

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

// ── validateRequiredEnv ──────────────────────────────────────────────────

func TestValidateRequiredEnv(t *testing.T) {
	t.Run("dev env is always a no-op", func(t *testing.T) {
		t.Setenv("GLUE_REGISTRY_NAME", "")
		t.Setenv("SNS_TOPIC_SERVICEACCOUNT_ARN", "")
		assert.NotPanics(t, func() { validateRequiredEnv("dev") })
	})
	t.Run("production with both vars set is a no-op", func(t *testing.T) {
		t.Setenv("GLUE_REGISTRY_NAME", "iam-serviceaccount-events")
		t.Setenv("SNS_TOPIC_SERVICEACCOUNT_ARN", "arn:aws:sns:ap-south-1:123456789012:iam-serviceaccount-events")
		assert.NotPanics(t, func() { validateRequiredEnv("production") })
	})
	t.Run("production accepts SNS_TOPIC_ARN in place of the alias", func(t *testing.T) {
		t.Setenv("GLUE_REGISTRY_NAME", "iam-serviceaccount-events")
		t.Setenv("SNS_TOPIC_ARN", "arn:aws:sns:ap-south-1:123456789012:iam-serviceaccount-events")
		t.Setenv("SNS_TOPIC_SERVICEACCOUNT_ARN", "")
		assert.NotPanics(t, func() { validateRequiredEnv("production") })
	})
	t.Run("production with a missing var panics", func(t *testing.T) {
		t.Setenv("GLUE_REGISTRY_NAME", "")
		t.Setenv("SNS_TOPIC_ARN", "arn:aws:sns:ap-south-1:123456789012:iam-serviceaccount-events")
		t.Setenv("SNS_TOPIC_SERVICEACCOUNT_ARN", "arn:aws:sns:ap-south-1:123456789012:iam-serviceaccount-events")
		assert.Panics(t, func() { validateRequiredEnv("production") })
	})
	t.Run("production with both vars missing panics listing both", func(t *testing.T) {
		t.Setenv("GLUE_REGISTRY_NAME", "")
		t.Setenv("SNS_TOPIC_ARN", "")
		t.Setenv("SNS_TOPIC_SERVICEACCOUNT_ARN", "")
		defer func() {
			r := recover()
			require.NotNil(t, r)
			msg, ok := r.(string)
			require.True(t, ok)
			assert.Contains(t, msg, "GLUE_REGISTRY_NAME")
			assert.Contains(t, msg, "SNS_TOPIC_ARN")
		}()
		validateRequiredEnv("production")
	})
}
