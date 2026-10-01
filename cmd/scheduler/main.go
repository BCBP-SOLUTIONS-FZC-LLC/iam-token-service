// Package main is the composition root for the Token Service's automatic
// cadence-driven rotation job (§16 TSQ-6 Resolved, TS-D14): a fourth
// binary, separate from cmd/server/cmd/consumer/cmd/rotator, run as its
// own Kubernetes CronJob. Each invocation scans
// service_account_credentials for every `active` row past its
// next_rotation_at (idx_sac_next_rotation), calls TS-1's own
// CredentialService.IssueOrRotate in-process for each one — the same
// core/service code cmd/server's HTTP handler calls, just invoked
// directly rather than over the wire — and then relays the returned
// plaintext to the Realm Provisioner's RP-17 endpoint exactly as an
// operator/O&M tool does by hand today (§8.2). It authenticates to
// Postgres RLS as the reserved iam-system system principal, the same
// dedicated service identity every other internal caller of this service
// presents (§5.2/§10.4) — a new in-mesh caller, not a new authorization
// mechanism.
//
// Unlike cmd/rotator (reconciler_jobs, .go-arch-lint.yml), this
// composition root deliberately imports core/service: rotating a
// credential is a service-layer write path (material generation,
// idempotency, event emission), not the narrow revoke-only concern
// cmd/rotator's reconciler pool is walled off to. Cross-tenant
// enumeration still goes through the same BYPASSRLS
// `serviceaccount_reconciler` role cmd/rotator uses (RLS-7) — read-only,
// SELECT-only, enumeration only. Every resulting write (IssueOrRotate)
// runs through the ordinary RLS-scoped `serviceaccount_app` pool, bound to
// that row's own tenant, exactly like an HTTP-triggered TS-1 call.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/openbao"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/realmprovisioner"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/outbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/logger"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/migrate"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// buildVersion is injected by -ldflags at build time (see Dockerfile / Makefile).
var buildVersion = "dev"

func main() {
	appEnv := envOr("APP_ENV", "dev")

	log, err := logger.NewLogger(appEnv)
	if err != nil {
		panic("init logger: " + err.Error())
	}
	shutdownTracing := gincommon.InitTracingFromEnv()
	defer shutdownTracing()

	cfg := gincommon.Config{
		Logger:       log,
		ServiceName:  envOr("APP_NAME", "iam-token-service"),
		BuildVersion: envOr("BUILD_VERSION", buildVersion),
		// Observability identity (Enterprise Platform Observability
		// Standard): domain "iam" (OBSERVABILITY_DOMAIN overrides it) and the
		// environment label, which must be one of local/dev/test/staging/prod
		// — ObservabilityMiddlewares panics on anything else (e.g. "production").
		Domain:      envOr("OBSERVABILITY_DOMAIN", metrics.ObservabilityDomain),
		Environment: appEnv,
	}
	_ = gincommon.ObservabilityMiddlewares(cfg) // metrics-init side effect only — this process has no Gin router
	// platform-events' (outbox/publish/consume/SQS/SNS) and platform-pgcommon's
	// (platform_db_*) metrics share gincommon's registerer and identity so one
	// /metrics scrape serves HTTP + business + messaging + DB collectors. Must
	// run before NewPool (pool gauges are registered by NewPool) and before
	// metrics.Register (which records into platform-events' shared collectors).
	if err := metrics.InitLibraryMetrics(log); err != nil {
		panic(fmt.Sprintf("init library metrics: %v", err))
	}
	metrics.Register(appEnv)

	// ── Database: the normal RLS-scoped app pool for IssueOrRotate's
	// per-tenant writes, and the BYPASSRLS serviceaccount_reconciler pool
	// for the cross-tenant due-list scan ONLY (§4.3/RLS-7) — same
	// two-pool shape as cmd/rotator. ──────────────────────────────────────
	pgCfg, pgWarnings := pgcommon.ConfigFromEnv()
	for _, w := range pgWarnings {
		log.Warn("postgres config warning", map[string]interface{}{"key": w.Key, "reason": w.Reason})
	}
	migrationDSN := pgadapter.MigrationDSNFromEnv()

	pgCfg.DSN = pgadapter.DSNFromEnv()
	pgCfg.GUCProvider = pgcommon.GUCSetFromContext
	pgCfg.Logger = pgadapter.NewLoggerAdapter(log)
	pgCfg.Tracer = pgadapter.NewOTelTracer(cfg.ServiceName)
	appPool, err := pgcommon.NewPool(context.Background(), pgCfg)
	if err != nil {
		panic(fmt.Sprintf("connect app pool: %v", err))
	}
	defer appPool.Close()

	reconDSN := pgadapter.ReconcilerDSNFromEnv()
	reconCfg := pgadapter.SystemPoolConfig(reconDSN, log)
	reconCfg.Tracer = pgadapter.NewOTelTracer(cfg.ServiceName)
	reconcilerPool, err := pgcommon.NewPool(context.Background(), reconCfg)
	if err != nil {
		panic(fmt.Sprintf("connect reconciler pool: %v", err))
	}
	defer reconcilerPool.Close()
	if reconDSN == pgCfg.DSN {
		log.Warn("RECONCILER_DATABASE_URL not set — reconciler pool reuses app DSN; cross-tenant queries will be RLS-filtered", nil)
	}

	// Deliberately shorter than the CronJob's activeDeadlineSeconds (240s
	// default, deploy/helm/values.yaml) — production-readiness review,
	// TS-D15: the prior 5-minute default was LONGER than that k8s deadline,
	// so a run that legitimately used its own budget could never reach the
	// graceful exit path (log summary, serveMetricsBriefly, pool drain)
	// below before Kubernetes SIGKILLs the Pod first. 180s leaves a ~60s
	// margin for that cleanup to actually run.
	ctx, cancel := context.WithTimeout(context.Background(), envDuration("SCHEDULER_RUN_TIMEOUT", 180*time.Second))
	defer cancel()

	if err := outbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: migrationDSN}); err != nil {
		panic(fmt.Sprintf("outbox schema: %v", err))
	}
	if err := pgadapter.RunMigrations(ctx, migrationDSN, log); err != nil {
		panic(fmt.Sprintf("domain migrations: %v", err))
	}

	// ── OpenBao ────────────────────────────────────────────────────────
	openbaoClient, err := openbao.New(openbao.Config{
		Addr:     mustEnv("OPENBAO_ADDR", appEnv, "https://openbao.iam.svc.cluster.local:8200"),
		AuthRole: envOr("OPENBAO_ROLE", "iam-token-service"),
		KVMount:  envOr("OPENBAO_KV_MOUNT", "iam"),
	}, log)
	if err != nil {
		panic(fmt.Sprintf("init openbao client: %v", err))
	}
	instrumentedSecrets := metrics.NewInstrumentedSecretStore(openbaoClient)

	// ── Repositories, enqueue-only TxRunner (no SNS-publish loop — same
	// rationale as cmd/rotator: cmd/server's replicas already drain the
	// shared outbox_events table), and the core CredentialService itself
	// (the composition-root difference from cmd/rotator, §3). ────────────
	principalRepo := pgadapter.NewPrincipalRepository(appPool)
	credentialRepo := pgadapter.NewCredentialRepository(appPool)
	reconcilerRepo := pgadapter.NewReconcilerRepository(reconcilerPool)

	enqueueCodec, err := eventbusadapter.NewValidatingCodec(eventbusadapter.NoopCodec{})
	if err != nil {
		panic(fmt.Sprintf("init validating codec: %v", err))
	}
	outboxPublisher := eventbusadapter.New(cfg.ServiceName, enqueueCodec).WithLogger(log)
	txRunner := pgadapter.NewTxRunner(appPool, outboxPublisher)

	credentialSvc := service.NewCredentialService(principalRepo, credentialRepo, instrumentedSecrets, txRunner, log, nil).
		WithCadenceDays(envInt("ROTATION_DEFAULT_CADENCE_DAYS", domain.DefaultCadenceDays))
	instrumentedCredentialSvc := metrics.NewInstrumentedCredentialService(credentialSvc)

	rpClient := realmprovisioner.New(
		mustEnv("REALM_PROVISIONER_BASE_URL", appEnv, "http://iam-realm-provisioner.iam.svc.cluster.local:8080"),
		log,
	)

	// ── Run the due-list scan once ──────────────────────────────────────
	scanCtx, endScan := startJobSpan(ctx, cfg.ServiceName, "cadence_scan")
	result := runCadenceScan(scanCtx, reconcilerRepo, instrumentedCredentialSvc, rpClient, log)
	log.Info("cadence scan complete", fieldsWithTrace(scanCtx, map[string]interface{}{
		"rotated": result.Rotated, "skipped": result.Skipped, "failed": result.Failed,
	}))
	endScan()
	recordCadenceScanMetric(result)

	// Run-to-completion CronJob Pod (no Pushgateway in this deployment) —
	// same brief-scrape-window pattern as cmd/rotator.
	serveMetricsBriefly(log, envDuration("SCHEDULER_METRICS_SCRAPE_GRACE", 15*time.Second))

	if err := appPool.DrainAndClose(context.Background()); err != nil {
		log.Error("app pool drain error", map[string]interface{}{"error": err.Error()})
	}
	if err := reconcilerPool.DrainAndClose(context.Background()); err != nil {
		log.Error("reconciler pool drain error", map[string]interface{}{"error": err.Error()})
	}
	if err := gincommon.Shutdown(log); err != nil {
		log.Error("logger/tracer flush error", map[string]interface{}{"error": err.Error()})
	}

	// A page-worthy divergence (an IssueOrRotate that succeeded but whose
	// RP-17 relay failed, leaving Keycloak out of sync with this service's
	// own record — see scan.go's doc comment) or an outright scan failure
	// both fail the Kubernetes Job so it's visible via kubectl/alerting,
	// without blocking the next scheduled run.
	if result.Failed > 0 {
		os.Exit(1)
	}
}

// serveMetricsBriefly serves /metrics for grace, then shuts down — mirrors
// cmd/rotator's identical helper (the window a Pod-level Prometheus scrape
// annotation has to capture this run's samples before the CronJob
// container exits).
func serveMetricsBriefly(log port.Logger, grace time.Duration) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{Addr: ":" + envOr("METRICS_PORT", "9090"), Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Warn("scheduler metrics server error", map[string]interface{}{"error": err.Error()})
		}
	case <-time.After(grace):
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Warn("scheduler metrics server shutdown error", map[string]interface{}{"error": err.Error()})
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

func isDevEnv(appEnv string) bool {
	return appEnv == "dev" || appEnv == "development" || appEnv == "local" || appEnv == "test"
}

func mustEnv(key, appEnv, devDefault string) string {
	v := os.Getenv(key)
	if v != "" {
		return v
	}
	if isDevEnv(appEnv) {
		return devDefault
	}
	panic(fmt.Sprintf("startup aborted — required env var %s is not set", key))
}
