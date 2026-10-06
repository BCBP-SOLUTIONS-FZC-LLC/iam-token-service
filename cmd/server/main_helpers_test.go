package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	httpadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/inbound/http"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
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
		t.Setenv("SNS_TOPIC_ARN", "")
		assert.NotPanics(t, func() { validateRequiredEnv("dev") })
	})
	t.Run("production with both vars set is a no-op", func(t *testing.T) {
		t.Setenv("GLUE_REGISTRY_NAME", "iam-serviceaccount-events")
		t.Setenv("SNS_TOPIC_ARN", "arn:aws:sns:ap-south-1:123456789012:iam-serviceaccount-events")
		assert.NotPanics(t, func() { validateRequiredEnv("production") })
	})
	t.Run("production with a missing var panics", func(t *testing.T) {
		t.Setenv("GLUE_REGISTRY_NAME", "")
		t.Setenv("SNS_TOPIC_ARN", "arn:aws:sns:ap-south-1:123456789012:iam-serviceaccount-events")
		assert.Panics(t, func() { validateRequiredEnv("production") })
	})
	t.Run("production with both vars missing panics listing both", func(t *testing.T) {
		t.Setenv("GLUE_REGISTRY_NAME", "")
		t.Setenv("SNS_TOPIC_ARN", "")
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

func TestDefaultOverlapSecondsFromEnv(t *testing.T) {
	t.Run("unset uses the built-in default", func(t *testing.T) {
		t.Setenv("ROTATION_DEFAULT_OVERLAP_SECONDS", "")
		assert.Equal(t, 300, defaultOverlapSecondsFromEnv())
	})
	t.Run("in range is used as is", func(t *testing.T) {
		t.Setenv("ROTATION_DEFAULT_OVERLAP_SECONDS", "900")
		assert.Equal(t, 900, defaultOverlapSecondsFromEnv())
	})
	t.Run("zero is a legitimate hard cutover", func(t *testing.T) {
		t.Setenv("ROTATION_DEFAULT_OVERLAP_SECONDS", "0")
		assert.Equal(t, 0, defaultOverlapSecondsFromEnv())
	})
	for _, bad := range []string{"901", "-1", "abc", "1.5"} {
		t.Run("invalid "+bad+" fails startup (TS-CONFIG-4)", func(t *testing.T) {
			t.Setenv("ROTATION_DEFAULT_OVERLAP_SECONDS", bad)
			assert.Panics(t, func() { defaultOverlapSecondsFromEnv() })
		})
	}
}

func TestValidateRequiredEnv_MessageNamesHelmValues(t *testing.T) {
	t.Setenv("GLUE_REGISTRY_NAME", "")
	t.Setenv("SNS_TOPIC_ARN", "")
	t.Setenv("DOCS_ENABLED", "")
	defer func() {
		msg, _ := recover().(string)
		assert.Contains(t, msg, "events.glueRegistryName")
		assert.Contains(t, msg, "events.topicArn")
	}()
	validateRequiredEnv("prod")
}

// ── Docs in prod (DOCS_ENABLED=true needs DOCS_AUTH_TOKEN) ─────────────────

func TestValidateRequiredEnv_DocsInProd(t *testing.T) {
	setAWS := func(t *testing.T) {
		t.Setenv("GLUE_REGISTRY_NAME", "iam-serviceaccount-events")
		t.Setenv("SNS_TOPIC_ARN", "arn:aws:sns:ap-south-1:123456789012:iam-serviceaccount-events")
	}
	for _, env := range []string{"prod", "production", " PROD ", "staging", "uat", "development"} {
		t.Run("enabled without token panics in "+env, func(t *testing.T) {
			setAWS(t)
			t.Setenv("DOCS_ENABLED", "true")
			t.Setenv("DOCS_AUTH_TOKEN", "")
			defer func() {
				msg, _ := recover().(string)
				require.NotEmpty(t, msg, "startup must fail")
				assert.Contains(t, msg, "DOCS_AUTH_TOKEN")
				assert.Contains(t, msg, "docs.authEnabled")
			}()
			validateRequiredEnv(env)
		})
	}
	t.Run("enabled with token is fine", func(t *testing.T) {
		setAWS(t)
		t.Setenv("DOCS_ENABLED", "true")
		t.Setenv("DOCS_AUTH_TOKEN", "s3cr3t-bearer")
		assert.NotPanics(t, func() { validateRequiredEnv("prod") })
	})
	t.Run("disabled without token is fine", func(t *testing.T) {
		setAWS(t)
		t.Setenv("DOCS_ENABLED", "false")
		t.Setenv("DOCS_AUTH_TOKEN", "")
		assert.NotPanics(t, func() { validateRequiredEnv("prod") })
	})
	t.Run("staging with token is fine", func(t *testing.T) {
		setAWS(t)
		t.Setenv("DOCS_ENABLED", "true")
		t.Setenv("DOCS_AUTH_TOKEN", "s3cr3t-bearer")
		assert.NotPanics(t, func() { validateRequiredEnv("staging") })
	})
}

func TestDocsUnprotected(t *testing.T) {
	t.Setenv("DOCS_ENABLED", "true")
	t.Setenv("DOCS_AUTH_TOKEN", "")
	for _, env := range []string{"local", "dev", "test", " DEV "} {
		assert.False(t, docsUnprotected(env), "%q is a developer environment", env)
	}
	for _, env := range []string{"staging", "prod", "production", "qa", ""} {
		assert.True(t, docsUnprotected(env), "%q must be treated like prod", env)
	}
	t.Setenv("DOCS_AUTH_TOKEN", "   ")
	assert.True(t, docsUnprotected("staging"), "a whitespace-only token is no token")
}

// ── JWKS_KNOWN_TENANTS_REFRESH ────────────────────────────────────────────

func TestJWKSKnownTenantsRefreshFromEnv(t *testing.T) {
	t.Run("unset is 15s", func(t *testing.T) {
		t.Setenv("JWKS_KNOWN_TENANTS_REFRESH", "")
		assert.Equal(t, 15*time.Second, jwksKnownTenantsRefreshFromEnv())
	})
	t.Run("valid duration", func(t *testing.T) {
		t.Setenv("JWKS_KNOWN_TENANTS_REFRESH", "1m")
		assert.Equal(t, time.Minute, jwksKnownTenantsRefreshFromEnv())
	})
	for _, bad := range []string{"0", "0s", "-5s", "15", "soon"} {
		t.Run("rejects "+bad, func(t *testing.T) {
			t.Setenv("JWKS_KNOWN_TENANTS_REFRESH", bad)
			assert.PanicsWithValue(t,
				`startup aborted — JWKS_KNOWN_TENANTS_REFRESH="`+bad+`" must be a positive Go duration (e.g. 15s)`,
				func() { jwksKnownTenantsRefreshFromEnv() })
		})
	}
}

// ── ROTATION_REPLAY_WINDOW ────────────────────────────────────────────────

func TestReplayWindowFromEnv(t *testing.T) {
	t.Run("unset uses the 15m default", func(t *testing.T) {
		t.Setenv("ROTATION_REPLAY_WINDOW", "")
		assert.Equal(t, 15*time.Minute, replayWindowFromEnv())
	})
	t.Run("explicit duration is used", func(t *testing.T) {
		t.Setenv("ROTATION_REPLAY_WINDOW", "1h")
		assert.Equal(t, time.Hour, replayWindowFromEnv())
	})
	for _, zero := range []string{"0", "0s"} {
		t.Run(zero+" lifts the limit", func(t *testing.T) {
			t.Setenv("ROTATION_REPLAY_WINDOW", zero)
			assert.Zero(t, replayWindowFromEnv())
		})
	}
	for _, bad := range []string{"-1m", "abc", "15"} {
		t.Run("invalid "+bad+" fails startup", func(t *testing.T) {
			t.Setenv("ROTATION_REPLAY_WINDOW", bad)
			assert.Panics(t, func() { replayWindowFromEnv() })
		})
	}
}

// ── JWKS unknown-tenant limiter env (envFloat/envInt) ─────────────────────

func TestEnvFloat(t *testing.T) {
	t.Setenv("JWKS_RATE_LIMIT_UNKNOWN_TENANT_RPS", "")
	assert.InDelta(t, 2.0, envFloat("JWKS_RATE_LIMIT_UNKNOWN_TENANT_RPS", 2), 0)
	t.Setenv("JWKS_RATE_LIMIT_UNKNOWN_TENANT_RPS", "0.5")
	assert.InDelta(t, 0.5, envFloat("JWKS_RATE_LIMIT_UNKNOWN_TENANT_RPS", 2), 0)
	for _, bad := range []string{"0", "-1", "x"} {
		t.Setenv("JWKS_RATE_LIMIT_UNKNOWN_TENANT_RPS", bad)
		assert.InDelta(t, 2.0, envFloat("JWKS_RATE_LIMIT_UNKNOWN_TENANT_RPS", 2), 0, bad)
	}
}

func TestEnvInt(t *testing.T) {
	t.Setenv("JWKS_RATE_LIMIT_UNKNOWN_TENANT_BURST", "")
	assert.Equal(t, 5, envInt("JWKS_RATE_LIMIT_UNKNOWN_TENANT_BURST", 5))
	t.Setenv("JWKS_RATE_LIMIT_UNKNOWN_TENANT_BURST", "9")
	assert.Equal(t, 9, envInt("JWKS_RATE_LIMIT_UNKNOWN_TENANT_BURST", 5))
	for _, bad := range []string{"0", "-3", "1.5"} {
		t.Setenv("JWKS_RATE_LIMIT_UNKNOWN_TENANT_BURST", bad)
		assert.Equal(t, 5, envInt("JWKS_RATE_LIMIT_UNKNOWN_TENANT_BURST", 5), bad)
	}
}

// ── Shutdown drain ────────────────────────────────────────────────────────

func TestDrainThenWait_FlagSetBeforeSleep(t *testing.T) {
	var d atomic.Bool
	var slept time.Duration
	drainThenWait(&d, 7*time.Second, func(delay time.Duration) {
		assert.True(t, d.Load(), "readiness must already be 503 while the drain delay runs")
		slept = delay
	})
	assert.Equal(t, 7*time.Second, slept)
	assert.True(t, d.Load())
}

func TestDrainAwarePinger(t *testing.T) {
	var d atomic.Bool
	calls := 0
	p := drainAwarePinger(&d, func(context.Context) error { calls++; return nil })
	require.NoError(t, p.Health(t.Context()))
	d.Store(true)
	require.ErrorIs(t, p.Health(t.Context()), errDraining)
	assert.Equal(t, 1, calls, "a draining pod does not even ask the database")
}

func TestReadyz_Returns503WhileDraining(t *testing.T) {
	var d atomic.Bool
	ok := pingerFunc(func(context.Context) error { return nil })
	r := httpadapter.NewRouter(httpadapter.RouterConfig{
		GinConfig: gincommon.Config{ServiceName: "iam-token-service-test", Domain: "iam", Environment: "test"},
		Handlers: httpadapter.Handlers{
			Principal:  httpadapter.NewPrincipalHandler(nil),
			Credential: httpadapter.NewCredentialHandler(nil),
		},
		Postgres: drainAwarePinger(&d, ok),
		OpenBao:  ok,
		Outbox:   ok,
	})
	probe := func() int {
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return rec.Code
	}
	assert.Equal(t, http.StatusOK, probe())
	drainThenWait(&d, 0, func(time.Duration) {
		assert.Equal(t, http.StatusServiceUnavailable, probe(), "503 during the drain delay")
	})
}

func TestWithDeadline_BoundsTheCheck(t *testing.T) {
	var deadline time.Time
	var hasDeadline bool
	p := withDeadline(readinessCheckTimeout, func(ctx context.Context) error {
		deadline, hasDeadline = ctx.Deadline()
		return nil
	})
	start := time.Now()
	require.NoError(t, p.Health(context.Background()))
	require.True(t, hasDeadline, "a check must never run without a server-side deadline")
	assert.WithinDuration(t, start.Add(readinessCheckTimeout), deadline, time.Second)
	assert.LessOrEqual(t, readinessCheckTimeout, 2*time.Second, "must stay under the probe's 3s timeoutSeconds")
}

func TestReadyz_HungDependencyAnswers503WithinDeadline(t *testing.T) {
	hung := withDeadline(50*time.Millisecond, func(ctx context.Context) error {
		<-ctx.Done() // a dependency call that only returns when cancelled
		return ctx.Err()
	})
	ok := pingerFunc(func(context.Context) error { return nil })
	r := httpadapter.NewRouter(httpadapter.RouterConfig{
		GinConfig: gincommon.Config{ServiceName: "iam-token-service-test", Domain: "iam", Environment: "test"},
		Handlers: httpadapter.Handlers{
			Principal:  httpadapter.NewPrincipalHandler(nil),
			Credential: httpadapter.NewCredentialHandler(nil),
		},
		Postgres: ok,
		OpenBao:  hung,
		Outbox:   ok,
	})
	rec := httptest.NewRecorder()
	start := time.Now()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Less(t, time.Since(start), 2*time.Second)
	assert.Contains(t, rec.Body.String(), `"openbao":"down"`)
}
