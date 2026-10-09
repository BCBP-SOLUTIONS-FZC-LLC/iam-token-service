// Package metricsstandard checks this service's metrics against the
// Enterprise Platform Observability Standard in the production registration
// order, in a test binary of its own: gincommon's identity is process-global
// and first-wins, so it must be initialised here with the production service
// name, not the "-test" name internal/adapter/outbound/metrics' own tests use.
//
// With METRICS_SCRAPE_OUT set, the test also writes the production /metrics
// handler's output to that path — the scrape .github/scripts/metricslint.sh
// runs `metricslint check` on and renders docs/observability/metric-registry.md
// from (`make metrics-lint`, `make metrics-inventory`).
package metricsstandard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
)

func TestStandard_LibrariesShareRegistryCollectors(t *testing.T) {
	// Production order (cmd/*/main.go): ObservabilityMiddlewares fixes the
	// identity → InitLibraryMetrics (platform-events, platform-pgcommon) →
	// Register (adopts platform-events' shared collectors).
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{
		ServiceName:  "iam-token-service",
		BuildVersion: "test",
		Domain:       metrics.ObservabilityDomain,
		Environment:  "test",
	})
	require.NoError(t, metrics.InitLibraryMetrics(nil))
	metrics.Register("test")

	exerciseEveryCollector()

	gathered, err := gincommon.MetricsGatherer().Gather()
	require.NoError(t, err)
	names := map[string]bool{}
	for _, mf := range gathered {
		names[mf.GetName()] = true
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if !strings.HasPrefix(mf.GetName(), "platform_") && !strings.HasPrefix(mf.GetName(), "iam_") {
				continue
			}
			assert.Equal(t, "iam", labels["domain"], "%s: domain", mf.GetName())
			assert.Equal(t, "iam-token-service", labels["service"], "%s: service", mf.GetName())
			assert.Equal(t, "test", labels["environment"], "%s: environment", mf.GetName())
			if _, ok := labels["version"]; ok {
				assert.Equal(t, "platform_build_info", mf.GetName(), "version label only on platform_build_info")
			}
		}
	}
	for _, want := range []string{
		"platform_build_info",
		"platform_library_info",
		"platform_dependency_request_seconds",
		"platform_duplicate_messages_total",
		"iam_token_service_credentials_issued_total",
		"iam_token_service_jwks_key_errors_total",
		"iam_token_service_jwks_known_tenants_refresh_total",
		"iam_token_service_jwks_known_tenants_last_refresh_age_seconds",
		"iam_token_service_cadence_rotation_total",
		"iam_token_service_jwks_rate_limited_by_bucket_total",
		"iam_token_service_credential_replays_total",
		"iam_token_service_keys_refresh_pending",
		"iam_token_service_keys_refresh_oldest_age_seconds",
		"iam_token_service_consumer_dlq_rejects_total",
		"iam_rls_violations_total",
	} {
		assert.True(t, names[want], "%s must be exported", want)
	}

	if out := os.Getenv("METRICS_SCRAPE_OUT"); out != "" {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
		req.Header.Set("Accept", "text/plain")
		gincommon.MetricsHandler().ServeHTTP(rec, req) // the production /metrics handler
		require.Equal(t, http.StatusOK, rec.Code)
		require.NoError(t, os.WriteFile(out, rec.Body.Bytes(), 0o600))
	}
}

// exerciseEveryCollector records one sample with representative production
// label values on every collector this service owns, so the scrape written
// for `metricslint check` exposes every label set (an unsampled vec has no
// series, and its label values would go unchecked).
func exerciseEveryCollector() {
	for _, op := range []string{"issue", "rotate", "revoke"} {
		metrics.CredentialsIssuedTotal.WithLabelValues(op).Inc()
	}
	metrics.RotationOverlapActive.Set(0)
	for _, op := range []string{"write", "delete"} {
		metrics.OpenBaoCallDuration.WithLabelValues(op).Observe(0.01)
	}
	for _, op := range []string{"write", "delete", "read", "list"} {
		for _, outcome := range []string{metrics.OutcomeSuccess, metrics.OutcomeError} {
			metrics.DependencyRequestDuration.WithLabelValues(metrics.DependencyOpenBao, op, outcome).Observe(0.01)
		}
	}
	for _, outcome := range []string{metrics.OutcomeSuccess, metrics.OutcomeError} {
		metrics.DependencyRequestDuration.WithLabelValues(metrics.DependencyRealmProvisioner, metrics.OperationRefreshKeys, outcome).Observe(0.01)
	}
	for _, result := range []string{"ok", "error"} {
		metrics.OffboardingCascadeTotal.WithLabelValues(result).Inc()
		metrics.RotationSweepTotal.WithLabelValues(result).Inc()
	}
	for _, result := range []string{"orphan_deleted", "missing_material", "ok", "error"} {
		metrics.MaterialReconcileTotal.WithLabelValues(result).Inc()
	}
	for _, result := range []string{"rotated", "skipped", "failed"} {
		metrics.CadenceRotationTotal.WithLabelValues(result).Inc()
	}
	metrics.JWKSKeyErrorsTotal.Inc()
	for _, result := range []string{metrics.OutcomeSuccess, metrics.OutcomeError} {
		metrics.IncJWKSKnownTenantsRefresh(result)
	}
	metrics.SetJWKSKnownTenantsLastRefreshAge(0)
	metrics.JWKSRateLimitedTotal.Inc()
	for _, b := range []string{metrics.JWKSBucketTenant, metrics.JWKSBucketGlobal, metrics.JWKSBucketUnknown} {
		metrics.IncJWKSRateLimited(b)
	}
	for _, r := range []string{metrics.ReplayServed, metrics.ReplayExpired, metrics.ReplayRevoked} {
		metrics.IncCredentialReplay(r)
	}
	metrics.SetKeysRefreshPending(1, 30)
	for _, r := range []string{metrics.DLQReasonSchemaViolation, metrics.DLQReasonInvalidEnvelopeID, "other"} {
		metrics.IncConsumerDLQReject(r)
	}
	metrics.ProcessedEventsDuplicates.WithLabelValues("tenant_offboarding").Inc()
	metrics.DuplicateMessagesTotal.WithLabelValues("tenant-lifecycle-tokensvc-q", "TenantMembershipsPurged").Inc()
	metrics.UnknownEventAcknowledged.WithLabelValues("tenant_offboarding", "TenantCreated").Inc()
	metrics.ConsumedSchemaViolationsTotal.WithLabelValues("tenant_offboarding", "TenantMembershipsPurged").Inc()
	for _, vType := range []string{"cross_tenant_access", "missing_or_invalid_guc", "other"} {
		metrics.AddRLSViolations(vType, 1)
	}
}
