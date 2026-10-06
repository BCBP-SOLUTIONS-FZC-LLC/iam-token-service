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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	consumeradapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/inbound/consumer"
	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/openbao"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"

	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/logger"
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
		// RequestTimeout appends gincommon's TimeoutMiddleware as the
		// innermost observability handler, so metrics, the access log and the
		// server span record the same 503 the client receives. As an outer
		// r.Use it ran before observability, which recorded 200.
		RequestTimeout: 30 * time.Second,
	}

	// Tracing with the SAME identity as metrics (service, version, domain,
	// environment) — InitTracingFromEnv read OTEL_SERVICE_NAME and the
	// domain/environment from the environment on its own, so traces could
	// disagree with metrics. Always installs gincommon's TracerProvider so
	// in-process spans get valid trace IDs even with no OTLP endpoint (dev);
	// ObservabilityMiddlewares' EnsureInitTelemetry is then a no-op.
	shutdownTracing, err := gincommon.InitTracingWithConfig(gincommon.TracingConfig{
		ServiceName:  cfg.ServiceName,
		BuildVersion: cfg.BuildVersion,
		Domain:       cfg.Domain,
		Environment:  cfg.Environment,
		Logger:       log,
	})
	if err != nil {
		panic("init tracing: " + err.Error())
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
	pgCfg.Tracer = gincommon.NewSpanTracer(cfg.ServiceName)
	pool, err := pgcommon.NewPool(context.Background(), pgCfg)
	if err != nil {
		panic(fmt.Sprintf("connect to postgres: %v", err))
	}
	defer pool.Close()

	ctx, cancelBackground := context.WithCancel(context.Background())
	defer cancelBackground()

	// Migrations (§4.4/MIG-2 order inside pgadapter.Migrate) — skipped when
	// RUN_MIGRATIONS=false (Helm: the migrate Job owns them).
	if pgadapter.MigrationsEnabledFromEnv() {
		if err := pgadapter.Migrate(ctx, migrationDSN, log); err != nil {
			panic(fmt.Sprintf("migrate: %v", err))
		}
	}

	// ── 3. OpenBao KV v2 secret store (§6.3, §10.5) ──────────────────────
	openbaoClient, err := openbao.New(openbao.Config{
		Addr:     mustEnv("OPENBAO_ADDR", appEnv, "https://openbao.iam.svc.cluster.local:8200"),
		AuthRole: envOr("OPENBAO_ROLE", "iam-token-service"),
		KVMount:  envOr("OPENBAO_KV_MOUNT", "iam"),
		// Empty = the default ServiceAccount token path; Helm sets it to an
		// OpenBao-audience projected token (openbao.tokenAudience).
		KubernetesTokenPath: os.Getenv("OPENBAO_K8S_TOKEN_PATH"),
	}, log)
	if err != nil {
		panic(fmt.Sprintf("init openbao client: %v", err))
	}

	// ── 4. Repositories + enqueue-only TxRunner (§7.3.1 — no Glue/SNS here,
	// cmd/server's outbox.Runner drains the shared outbox_events table) ───
	principalRepo := pgadapter.NewPrincipalRepository(pool)
	credentialRepo := pgadapter.NewCredentialRepository(pool)
	enqueueCodec, err := eventbusadapter.NewValidatingCodec(eventbusadapter.NoopCodec{})
	if err != nil {
		panic(fmt.Sprintf("init validating codec: %v", err))
	}
	outboxPublisher := eventbusadapter.New(cfg.ServiceName, enqueueCodec).WithLogger(log)
	// platform-events' inbox over processed_events: the claim, the cascade's
	// writes and its outbox events commit in one transaction (§9.2).
	inboxRepo := pgadapter.NewInboxRepository(pool, outboxPublisher)

	instrumentedSecrets := metrics.NewInstrumentedSecretStore(openbaoClient) // iam_token_service_openbao_call_duration_seconds
	offboardingConsumer := consumeradapter.NewOffboardingConsumer(
		principalRepo, credentialRepo, instrumentedSecrets, inboxRepo, log,
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

	// ── 5. SQS consumer (§7.1, §12) — platform-events config.LoadSQS
	// (canonical SQS_* names only, see loadSQSEnv).
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

	// If the receive loop ends on its own (a startup DLQ check, an
	// unrecoverable SQS error), the pod must not stay Ready while consuming
	// nothing: consumerDone ends main, which exits non-zero so Kubernetes
	// restarts the pod.
	consumerDone := make(chan error, 1)
	go func() {
		consumerDone <- sqsConsumer.Start(ctx)
	}()

	// ── 6. Health + metrics servers ────────────────────────────────────────
	// /healthz and /readyz go through the same Gin + gincommon stack as
	// cmd/server (and iam-user-profile / iam-org-membership): Zap access
	// logs, HTTP metrics, and HTTP traces. /metrics stays on a dedicated
	// stdlib mux — scrapes must not share a listener with probes, matching
	// cmd/server's METRICS_PORT split.
	health := gin.New()
	health.Use(gincommon.ObservabilityMiddlewares(cfg)...)
	health.GET("/healthz", gincommon.HealthHandler())
	health.GET("/readyz", readyzHandler(&draining,
		func(ctx context.Context) bool { return pool.Health(ctx).Healthy },
		openbaoClient.Health))
	healthServer := &http.Server{
		Addr:              ":" + envOr("APP_PORT", "8080"),
		Handler:           health,
		ReadHeaderTimeout: 5 * time.Second,
	}

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", gincommon.MetricsHandler())
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

	exitCode := awaitStop(quit, consumerDone, &draining, envDuration("SHUTDOWN_DRAIN_DELAY", 5*time.Second), time.Sleep, log)

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
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

// awaitStop blocks until either a shutdown signal or the end of the SQS
// receive loop, flips draining in both cases, and returns the process exit
// code.
//
// On a signal, draining is set BEFORE the SHUTDOWN_DRAIN_DELAY wait, so
// /readyz is already 503 while the endpoint removal propagates; exit 0.
//
// If consumerDone fires first, nothing has cancelled the consumer's ctx
// yet, so any return — with or without an error — means the receive loop
// died on its own (a startup DLQ check, an unrecoverable SQS error). The
// pod must not stay Ready consuming nothing: exit 1 so Kubernetes restarts
// it. No drain delay there — there is no traffic to drain.
func awaitStop(quit <-chan os.Signal, consumerDone <-chan error, draining *atomic.Bool, drainDelay time.Duration, sleep func(time.Duration), log port.Logger) int {
	select {
	case <-quit:
		draining.Store(true)
		log.Info("shutdown signal received — draining", map[string]interface{}{"drain_delay": drainDelay.String()})
		sleep(drainDelay)
		return 0
	case err := <-consumerDone:
		draining.Store(true)
		log.Error("offboarding consumer stopped — exiting so the pod restarts", map[string]interface{}{"error": errString(err)})
		return 1
	}
}

// readinessCheckTimeout bounds the /readyz dependency checks server-side.
// It sits below the readinessProbe's timeoutSeconds (3, Helm) so the
// handler answers 503 itself instead of the kubelet timing the probe out
// while a hung Postgres/OpenBao call keeps running.
const readinessCheckTimeout = 2 * time.Second

// readyzHandler is the consumer's /readyz: 503 "draining" as soon as
// draining is set (SIGTERM or a dead receive loop) without touching the
// dependencies, otherwise ready only when Postgres and OpenBao both answer.
func readyzHandler(draining *atomic.Bool, dbHealthy func(context.Context) bool, baoHealth func(context.Context) error) gin.HandlerFunc {
	return func(c *gin.Context) {
		if draining.Load() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "draining"})
			return
		}
		checkCtx, cancel := context.WithTimeout(c.Request.Context(), readinessCheckTimeout)
		defer cancel()
		db := dbHealthy(checkCtx)
		bao := baoHealth(checkCtx) == nil
		if db && bao {
			c.JSON(http.StatusOK, gin.H{"status": "ready"})
			return
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status": "not ready",
			"checks": gin.H{
				"database": healthLabel(db),
				"openbao":  healthLabel(bao),
			},
		})
	}
}

// draining is set on SIGTERM or when the consumer loop has ended; /readyz
// fails while it is.
var draining atomic.Bool

func errString(err error) string {
	if err == nil {
		return "consumer returned without error"
	}
	return err.Error()
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
// (config.LoadSQS: SQS_QUEUE_URL, SQS_CONCURRENCY, SQS_MAX_MESSAGES,
// SQS_WAIT_SECONDS, SQS_VISIBILITY_TIMEOUT, SQS_DRAIN_TIMEOUT,
// SQS_HANDLER_TIMEOUT, SQS_QUEUE_DEPTH_INTERVAL), matching
// iam-org-membership. SQS_QUEUE_URL is required outside dev. Service
// defaults apply when a variable is unset:
//   - concurrency 2;
//   - visibility timeout 60s (this consumer's LLD value; the library's is 30s);
//   - handler timeout 45s, below the visibility timeout, so a hung cascade
//     (the OpenBao deletes run inside the inbox transaction) fails and is
//     redelivered instead of hiding its message forever;
//   - drain timeout 15s, inside the 30s terminationGracePeriodSeconds;
//   - queue-depth sampling every 60s ("0s" disables), feeding
//     platform_queue_depth / platform_dlq_depth.
func loadSQSEnv(appEnv string) eventcfg.SQSConfigEnv {
	env := eventcfg.LoadSQS()
	if env.QueueURL == "" {
		if !isDevEnv(appEnv) {
			// Named Helm value, not just the env var: the operator fixing
			// this edits values.yaml, never the Deployment's env directly.
			panic("startup aborted — required env var SQS_QUEUE_URL is not set: the tenant-lifecycle-tokensvc-q URL, without which no TenantMembershipsPurged is ever consumed; set Helm value sqs.queueUrl")
		}
		env.QueueURL = "http://localhost:4566/000000000000/tenant-lifecycle-tokensvc-q"
	}
	if os.Getenv("SQS_CONCURRENCY") == "" {
		env.Concurrency = 2
	}
	if os.Getenv("SQS_VISIBILITY_TIMEOUT") == "" {
		env.VisibilityTimeout = 60 * time.Second
	}
	if os.Getenv("SQS_HANDLER_TIMEOUT") == "" {
		env.HandlerTimeout = 45 * time.Second
	}
	if os.Getenv("SQS_DRAIN_TIMEOUT") == "" {
		env.DrainTimeout = 15 * time.Second
	}
	if os.Getenv("SQS_QUEUE_DEPTH_INTERVAL") == "" {
		env.QueueDepthInterval = 60 * time.Second
	}
	return env
}
