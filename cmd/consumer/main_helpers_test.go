package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
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
	t.Setenv("SQS_HANDLER_TIMEOUT", "30s")
	t.Setenv("SQS_DRAIN_TIMEOUT", "10s")
	env := loadSQSEnv("dev")
	assert.Equal(t, "http://localhost/canonical", env.QueueURL)
	assert.Equal(t, 7, env.Concurrency)
	assert.Equal(t, 45*time.Second, env.VisibilityTimeout)
	assert.Equal(t, 30*time.Second, env.HandlerTimeout)
	assert.Equal(t, 10*time.Second, env.DrainTimeout)
}

func TestLoadSQSEnv_AppliesServiceDefaultsWhenUnset(t *testing.T) {
	for _, k := range []string{"SQS_QUEUE_URL", "SQS_CONCURRENCY", "SQS_VISIBILITY_TIMEOUT", "SQS_HANDLER_TIMEOUT", "SQS_DRAIN_TIMEOUT", "SQS_QUEUE_DEPTH_INTERVAL"} {
		t.Setenv(k, "")
	}
	env := loadSQSEnv("dev")
	assert.Equal(t, "http://localhost:4566/000000000000/tenant-lifecycle-tokensvc-q", env.QueueURL, "dev placeholder")
	assert.Equal(t, 2, env.Concurrency)
	assert.Equal(t, 60*time.Second, env.VisibilityTimeout)
	assert.Equal(t, 45*time.Second, env.HandlerTimeout, "below the visibility timeout")
	assert.Equal(t, 15*time.Second, env.DrainTimeout, "inside the 30s termination grace period")
	assert.Equal(t, 60*time.Second, env.QueueDepthInterval, "queue-depth sampling is on by default")
}

func TestLoadSQSEnv_QueueURLRequiredOutsideDev(t *testing.T) {
	t.Setenv("SQS_QUEUE_URL", "")
	assert.Panics(t, func() { loadSQSEnv("prod") })
}

func TestLoadSQSEnv_QueueDepthIntervalOverride(t *testing.T) {
	t.Setenv("SQS_QUEUE_URL", "http://localhost/q")
	t.Setenv("SQS_QUEUE_DEPTH_INTERVAL", "0s")
	assert.Zero(t, loadSQSEnv("dev").QueueDepthInterval, "an explicit 0s disables the sampler")
}

func TestLoadSQSEnv_MissingQueueURLMessageNamesHelmValue(t *testing.T) {
	t.Setenv("SQS_QUEUE_URL", "")
	defer func() {
		msg, _ := recover().(string)
		assert.Contains(t, msg, "SQS_QUEUE_URL")
		assert.Contains(t, msg, "sqs.queueUrl")
	}()
	loadSQSEnv("staging")
}

// ── awaitStop: drain + exit-code decision ─────────────────────────────────

func TestAwaitStop_SignalDrainsBeforeSleepAndExitsZero(t *testing.T) {
	var d atomic.Bool
	quit := make(chan os.Signal, 1)
	quit <- syscall.SIGTERM
	var slept time.Duration
	code := awaitStop(quit, make(chan error), &d, 3*time.Second, func(delay time.Duration) {
		assert.True(t, d.Load(), "readiness must already be 503 while the drain delay runs")
		slept = delay
	}, &fakeLogger{})
	assert.Equal(t, 0, code)
	assert.Equal(t, 3*time.Second, slept)
}

func TestAwaitStop_EarlyConsumerExitIsNonZero(t *testing.T) {
	for name, err := range map[string]error{"with error": errors.New("queue gone"), "clean return": nil} {
		t.Run(name, func(t *testing.T) {
			var d atomic.Bool
			done := make(chan error, 1)
			done <- err
			log := &fakeLogger{}
			code := awaitStop(make(chan os.Signal), done, &d, time.Hour, func(time.Duration) {
				t.Fatal("no drain delay when the receive loop died — there is no traffic to drain")
			}, log)
			assert.Equal(t, 1, code, "the pod must restart, not stay Ready consuming nothing")
			assert.True(t, d.Load())
			assert.Len(t, log.errorCalls, 1)
		})
	}
}

// ── readyzHandler ─────────────────────────────────────────────────────────

func TestReadyzHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	probe := func(d *atomic.Bool, db bool, bao error) (int, string) {
		r := gin.New()
		r.GET("/readyz", readyzHandler(d, func(context.Context) bool { return db }, func(context.Context) error { return bao }))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return rec.Code, rec.Body.String()
	}
	var d atomic.Bool
	code, _ := probe(&d, true, nil)
	assert.Equal(t, http.StatusOK, code)

	code, body := probe(&d, false, nil)
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Contains(t, body, `"database":"down"`)

	code, body = probe(&d, true, errors.New("sealed"))
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Contains(t, body, `"openbao":"down"`)

	d.Store(true)
	code, body = probe(&d, true, nil)
	assert.Equal(t, http.StatusServiceUnavailable, code, "503 while draining even with healthy deps")
	assert.Contains(t, body, "draining")
}

func TestReadyzHandler_ChecksRunUnderServerSideDeadline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var dbDeadline, baoDeadline time.Time
	var dbOK, baoOK bool
	r := gin.New()
	r.GET("/readyz", readyzHandler(&atomic.Bool{},
		func(ctx context.Context) bool { dbDeadline, dbOK = ctx.Deadline(); return true },
		func(ctx context.Context) error {
			baoDeadline, baoOK = ctx.Deadline()
			<-ctx.Done() // a hung OpenBao call that only returns when cancelled
			return ctx.Err()
		}))
	start := time.Now()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), `"openbao":"down"`)
	require.True(t, dbOK)
	require.True(t, baoOK)
	assert.WithinDuration(t, start.Add(readinessCheckTimeout), dbDeadline, time.Second)
	assert.Equal(t, dbDeadline, baoDeadline, "one deadline bounds the whole check")
	assert.Less(t, time.Since(start), readinessCheckTimeout+time.Second)
	assert.LessOrEqual(t, readinessCheckTimeout, 2*time.Second, "must stay under the probe's 3s timeoutSeconds")
}
