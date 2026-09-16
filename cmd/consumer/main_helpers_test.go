package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHealthLabel(t *testing.T) {
	assert.Equal(t, "ok", healthLabel(true))
	assert.Equal(t, "down", healthLabel(false))
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
		require.Panics(t, func() { mustEnv("TS_TEST_MUSTENV", "production", "dev-default") })
	})
}

func TestLoadSQSEnv_UsesCanonicalNames(t *testing.T) {
	t.Setenv("SQS_QUEUE_URL", "http://localhost/canonical")
	t.Setenv("SQS_CONCURRENCY", "7")
	t.Setenv("SQS_VISIBILITY_TIMEOUT", "45s")
	t.Setenv("SQS_OFFBOARDING_QUEUE_URL", "http://localhost/alias")
	t.Setenv("SQS_OFFBOARDING_CONCURRENCY", "2")
	env := loadSQSEnv("dev")
	assert.Equal(t, "http://localhost/canonical", env.QueueURL)
	assert.Equal(t, 7, env.Concurrency)
	assert.Equal(t, 45*time.Second, env.VisibilityTimeout)
}

func TestLoadSQSEnv_FallsBackToOffboardingAliases(t *testing.T) {
	t.Setenv("SQS_QUEUE_URL", "")
	t.Setenv("SQS_CONCURRENCY", "")
	t.Setenv("SQS_VISIBILITY_TIMEOUT", "")
	t.Setenv("SQS_OFFBOARDING_QUEUE_URL", "http://localhost/alias")
	t.Setenv("SQS_OFFBOARDING_CONCURRENCY", "3")
	env := loadSQSEnv("dev")
	assert.Equal(t, "http://localhost/alias", env.QueueURL)
	assert.Equal(t, 3, env.Concurrency)
	assert.Equal(t, 60*time.Second, env.VisibilityTimeout)
}
