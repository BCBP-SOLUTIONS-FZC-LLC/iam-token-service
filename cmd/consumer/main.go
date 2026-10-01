// Package main is the composition root for the Token Service's inbound SQS
// consumer process: the one inbound subscription this service has,
// TenantMembershipsPurged on tenant-lifecycle-tokensvc-q, driving the
// offboarding cascade (§7.1, §8.4). A separate process/entrypoint from
// cmd/server (§build-order milestone 6) — it shares the same Postgres
// schema and outbox_events table, but does NOT run its own outbox.Runner:
// cmd/server's replica(s) already drain outbox_events (claim-lease safe
// across multiple pollers), so this process only needs to enqueue, never
// publish, keeping its AWS footprint to SQS alone (no Glue/SNS
// credentials required — inbound Glue headers are stripped by the
// registry-free eventbus.GlueDecoder).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	consumeradapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/inbound/consumer"
	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/openbao"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"

	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
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
	if appEnv != "dev" {
		gin.SetMode(gin.ReleaseMode)
	}

	// ── 1. Logger — Zap via platform-gincommon (same sink as iam-org-membership)
	log, err := logger.NewLogger(appEnv)
	if err != nil {
		panic("init logger: " + err.Error())
	}
	shutdownTracing := gincommon.InitTracingFromEnv()

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
	// ObservabilityMiddlewares is gincommon's public metrics-init API. Call
	// it here (before any collector registration) so business metrics land
	// on gincommon.MetricsRegisterer with matching {domain, service, environment} const
	// labels. The health engine below applies the same middleware slice so
	// probe traffic gets Zap access logs / HTTP metrics / HTTP traces.
	_ = gincommon.ObservabilityMiddlewares(cfg)
	// platform-events' (outbox/publish/consume/SQS/SNS) and platform-pgcommon's
	// (platform_db_*) metrics share gincommon's registerer and identity so one
	// /metrics scrape serves HTTP + business + messaging + DB collectors. Must
	// run before NewPool (pool gauges are registered by NewPool) and before
	// metrics.Register (which records into platform-events' shared collectors).
	if err := metrics.InitLibraryMetrics(log); err != nil {
		panic(fmt.Sprintf("init library metrics: %v", err))
	}
	metrics.Register(appEnv)

	// ── 2. Database ────────────────────────────────────────────────────────
	pgCfg, pgWarnings := pgcommon.ConfigFromEnv()
	for _, w := range pgWarnings {
		log.Warn("postgres config warning", map[string]interface{}{"key": w.Key, "reason": w.Reason})
	}
	dsn := pgadapter.DSNFromEnv()
	migrationDSN := pgadapter.MigrationDSNFromEnv()

	pgCfg.DSN = dsn
	pgCfg.GUCProvider = pgcommon.GUCSetFromContext
	pgCfg.Logger = pgadapter.NewLoggerAdapter(log)
	pgCfg.Tracer = pgadapter.NewOTelTracer(cfg.ServiceName)
	pool, err := pgcommon.NewPool(context.Background(), pgCfg)
	if err != nil {
		panic(fmt.Sprintf("connect to postgres: %v", err))
	}
	defer pool.Close()

	ctx, cancelBackground := context.WithCancel(context.Background())
	defer cancelBackground()

	if err := outbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: migrationDSN}); err != nil {
		panic(fmt.Sprintf("outbox schema: %v", err))
	}
	if err := pgadapter.RunMigrations(ctx, migrationDSN, log); err != nil {
		panic(fmt.Sprintf("domain migrations: %v", err))
	}

	// ── 3. OpenBao KV v2 secret store (§6.3, §10.5) ──────────────────────
	openbaoClient, err := openbao.New(openbao.Config{
		Addr:     mustEnv("OPENBAO_ADDR", appEnv, "https://openbao.iam.svc.cluster.local:8200"),
		AuthRole: envOr("OPENBAO_ROLE", "iam-token-service"),
		KVMount:  envOr("OPENBAO_KV_MOUNT", "iam"),
	}, log)
	if err != nil {
		panic(fmt.Sprintf("init openbao client: %v", err))
	}

	// ── 4. Repositories + enqueue-only TxRunner (§7.3.1 — no Glue/SNS here,
	// cmd/server's outbox.Runner drains the shared outbox_events table) ───
	principalRepo := pgadapter.NewPrincipalRepository(pool)
	credentialRepo := pgadapter.NewCredentialRepository(pool)
	processedEventsRepo := pgadapter.NewProcessedEventsRepository(pool)

	enqueueCodec, err := eventbusadapter.NewValidatingCodec(eventbusadapter.NoopCodec{})
	if err != nil {
		panic(fmt.Sprintf("init validating codec: %v", err))
	}
	outboxPublisher := eventbusadapter.New(cfg.ServiceName, enqueueCodec).WithLogger(log)
	txRunner := pgadapter.NewTxRunner(pool, outboxPublisher)

	instrumentedSecrets := metrics.NewInstrumentedSecretStore(openbaoClient) // iam_token_service_openbao_call_duration_seconds
	offboardingConsumer := consumeradapter.NewOffboardingConsumer(
		principalRepo, credentialRepo, instrumentedSecrets, processedEventsRepo, txRunner, log,
	)

	// handleWithMetrics wraps offboardingConsumer.Handle to record
	// iam_token_service_offboarding_cascade_total{result} (§8.4, §11.2)
	// around every delivery, success or failure. (The proposed Tier-2
	// iam_offboarding_cascade_total is not emitted until it is ratified in
	// the Platform Observability Registry.)
	// validated runs the consumed-schema check (inbound_schema.go) before
	// the cascade; a violation is counted as a failed cascade too.
	validated := validateConsumed(offboardingConsumer.Handle, enqueueCodec, log)
	handleWithMetrics := func(ctx context.Context, env events.Envelope[json.RawMessage]) error {
		err := validated(ctx, env)
		result := "ok"
		if err != nil {
			result = "error"
		}
		if metrics.OffboardingCascadeTotal != nil {
			metrics.OffboardingCascadeTotal.WithLabelValues(result).Inc()
		}
		return err
	}

	// ── 5. SQS consumer (§7.1, §12) — platform-events config.LoadSQS,
	// matching iam-user-profile. SQS_OFFBOARDING_QUEUE_URL /
	// SQS_OFFBOARDING_CONCURRENCY remain accepted aliases so existing Helm
	// values keep working until they also set the library's canonical names.
	// Pipeline, outermost first: DLQ router (dlq.go — permanent rejects
	// straight to the -dlq + ack) → cascade metrics → consumed-schema
	// validation → OffboardingConsumer.Handle; GlueDecoder strips the Glue
	// header before any of it (buildSQSConsumer).
	sqsEnv := loadSQSEnv(appEnv)
	eventcfg.LogWarningsTo(log, sqsEnv.Warnings)
	sqsClient, err := newSQSClient(ctx, sqsEnv)
	if err != nil {
		panic(fmt.Sprintf("init sqs client: %v", err))
	}
	sqsConsumer, err := buildSQSConsumer(sqsEnv, sqsClient,
		withDLQRouting(ctx, sqsClient, sqsEnv.QueueURL, handleWithMetrics, log), log)
	if err != nil {
		panic(fmt.Sprintf("build offboarding consumer: %v", err))
	}

	go func() {
		if err := sqsConsumer.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("offboarding consumer stopped", map[string]interface{}{"error": err.Error()})
		}
	}()

	// ── 6. Health + metrics servers ────────────────────────────────────────
	// /healthz and /readyz go through the same Gin + gincommon stack as
	// cmd/server (and iam-user-profile / iam-org-membership): Zap access
	// logs, HTTP metrics, and HTTP traces. /metrics stays on a dedicated
	// stdlib mux — scrapes must not share a listener with probes, matching
	// cmd/server's METRICS_PORT split.
	health := gin.New()
	health.Use(gincommon.TimeoutMiddleware(30 * time.Second))
	health.Use(gincommon.ObservabilityMiddlewares(cfg)...)
	health.GET("/healthz", gincommon.HealthHandler())
	health.GET("/readyz", func(c *gin.Context) {
		checkCtx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
		defer cancel()
		dbHealthy := pool.Health(checkCtx).Healthy
		baoHealthy := openbaoClient.Health(checkCtx) == nil
		if dbHealthy && baoHealthy {
			c.JSON(http.StatusOK, gin.H{"status": "ready"})
			return
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status": "not ready",
			"checks": gin.H{
				"database": healthLabel(dbHealthy),
				"openbao":  healthLabel(baoHealthy),
			},
		})
	})
	healthServer := &http.Server{
		Addr:              ":" + envOr("APP_PORT", "8080"),
		Handler:           health,
		ReadHeaderTimeout: 5 * time.Second,
	}

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsServer := &http.Server{
		Addr:              ":" + envOr("METRICS_PORT", "9090"),
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	log.Info("consumer starting", map[string]interface{}{
		"service": cfg.ServiceName, "version": cfg.BuildVersion, "env": appEnv, "queue_url": sqsEnv.QueueURL,
	})
	go func() {
		if err := healthServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("health server error", map[string]interface{}{"error": err.Error()})
		}
	}()
	go func() {
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics server error", map[string]interface{}{"error": err.Error()})
		}
	}()

	<-quit
	log.Info("shutdown signal received — draining", nil)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := healthServer.Shutdown(shutdownCtx); err != nil {
		log.Error("health server shutdown error", map[string]interface{}{"error": err.Error()})
	}
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		log.Error("metrics server shutdown error", map[string]interface{}{"error": err.Error()})
	}
	if err := sqsConsumer.Stop(); err != nil {
		log.Error("offboarding consumer stop error", map[string]interface{}{"error": err.Error()})
	}
	cancelBackground()
	if err := pool.DrainAndClose(shutdownCtx); err != nil {
		log.Error("app pool drain error", map[string]interface{}{"error": err.Error()})
	}
	shutdownTracing()
	if err := gincommon.Shutdown(log); err != nil {
		log.Error("logger/tracer flush error", map[string]interface{}{"error": err.Error()})
	}
}

func healthLabel(ok bool) string {
	if ok {
		return "ok"
	}
	return "down"
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

// isDevEnv reports whether appEnv is one of the local/CI-only values that
// permit a hard dependency's safe placeholder default (§12.1 fail-fast).
func isDevEnv(appEnv string) bool {
	return appEnv == "dev" || appEnv == "development" || appEnv == "local" || appEnv == "test"
}

// mustEnv panics with a descriptive message if key is empty outside a dev
// APP_ENV; dev falls back to devDefault so a bare-metal `go run` works with
// no .env at all (§12.1 fail-fast).
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

// loadSQSEnv reads SQS consumer config through platform-events
// (SQS_QUEUE_URL / SQS_MAX_MESSAGES / SQS_WAIT_SECONDS /
// SQS_VISIBILITY_TIMEOUT / SQS_CONCURRENCY), matching iam-user-profile.
// Service-specific aliases keep existing Helm/compose values working:
// SQS_OFFBOARDING_QUEUE_URL fills an empty SQS_QUEUE_URL, and
// SQS_OFFBOARDING_CONCURRENCY fills SQS_CONCURRENCY when that var is
// unset. Visibility timeout defaults to 60s (this consumer's LLD value)
// when SQS_VISIBILITY_TIMEOUT is unset — LoadSQS's library default is 30s.
func loadSQSEnv(appEnv string) eventcfg.SQSConfigEnv {
	env := eventcfg.LoadSQS()
	if env.QueueURL == "" {
		env.QueueURL = mustEnv("SQS_OFFBOARDING_QUEUE_URL", appEnv, "http://localhost:4566/000000000000/tenant-lifecycle-tokensvc-q")
	}
	if os.Getenv("SQS_CONCURRENCY") == "" {
		env.Concurrency = envInt("SQS_OFFBOARDING_CONCURRENCY", 2)
	}
	if os.Getenv("SQS_VISIBILITY_TIMEOUT") == "" {
		env.VisibilityTimeout = 60 * time.Second
	}
	return env
}
