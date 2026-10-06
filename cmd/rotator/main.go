// Package main is the composition root for the Token Service's scheduled
// maintenance jobs (§8.3, §8.6, §4.2/§13.1): the overlap-expiry sweep, the
// orphan-material reconciler, and the outbox/processed_events prune. A
// separate binary from cmd/server and cmd/consumer, run as a Kubernetes
// CronJob (default schedule every 5 minutes, Helm rotator.schedule) —
// each invocation runs all three jobs once, logs a summary, and exits;
// Kubernetes owns the re-scheduling, not this binary. Running the
// reconciler/prune tasks on the same 5-minute cadence as the sweep (rather
// than their own "default hourly" cadence, §8.6) is a deliberate
// simplification: both are idempotent and cheap at this service's scale
// (§21), so a single CronJob resource converges faster than the LLD's
// documented default without any correctness cost.
//
// Does NOT run its own outbox.Runner SNS-publish loop — cmd/server's
// replica(s) already drain the shared outbox_events table (claim-lease
// safe across multiple pollers); this process only enqueues
// (ServiceAccountCredentialRevoked, on sweep) and, via PrunePublished,
// deletes already-published rows — it never needs Glue/SNS credentials.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/openbao"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/realmprovisioner"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"

	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/outbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/logger"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// buildVersion is injected by -ldflags at build time (see Dockerfile / Makefile).
var buildVersion = "dev"

func main() {
	appEnv := envOr("APP_ENV", "dev")

	// Zap via platform-gincommon (same sink as iam-org-membership).
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
	defer shutdownTracing()
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

	// ── Database: two pools — the normal RLS-scoped app pool for the
	// sweep's per-row revokes, and the BYPASSRLS serviceaccount_reconciler
	// pool for cross-tenant enumeration ONLY (§4.3/RLS-7). ────────────────
	pgCfg, pgWarnings := pgcommon.ConfigFromEnv()
	for _, w := range pgWarnings {
		log.Warn("postgres config warning", map[string]interface{}{"key": w.Key, "reason": w.Reason})
	}
	migrationDSN := pgadapter.MigrationDSNFromEnv()

	pgCfg.DSN = pgadapter.DSNFromEnv()
	pgCfg.GUCProvider = pgcommon.GUCSetFromContext
	pgCfg.Logger = pgadapter.NewLoggerAdapter(log)
	pgCfg.Tracer = gincommon.NewSpanTracer(cfg.ServiceName)
	appPool, err := pgcommon.NewPool(context.Background(), pgCfg)
	if err != nil {
		panic(fmt.Sprintf("connect app pool: %v", err))
	}
	defer appPool.Close()

	reconDSN := pgadapter.ReconcilerDSNFromEnv()
	reconCfg := pgadapter.SystemPoolConfig(reconDSN, log)
	reconCfg.Tracer = gincommon.NewSpanTracer(cfg.ServiceName)
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
	//
	// SIGTERM/SIGINT (a node drain, a Job deletion, the k8s deadline) cancels
	// the run context: no new tenant/principal/prune step starts, the
	// in-flight tenant finishes on its own detached context (workCtx) —
	// including its RP-17 call — and the summary/metrics path below still
	// runs. terminationGracePeriodSeconds (Helm: 45s) covers rowReserve +
	// rp17Timeout. Anything a SIGKILL still cuts off after a commit is left
	// in keys_refresh_pending and retried by the next run.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stopSignals()
	ctx, cancel := context.WithTimeout(sigCtx, envDuration("ROTATOR_RUN_TIMEOUT", 180*time.Second))
	defer cancel()
	runStart := time.Now()

	// Migrations (§4.4/MIG-2 order inside pgadapter.Migrate) — skipped when
	// RUN_MIGRATIONS=false (Helm: the migrate Job owns them).
	if pgadapter.MigrationsEnabledFromEnv() {
		if err := pgadapter.Migrate(ctx, migrationDSN, log); err != nil {
			panic(fmt.Sprintf("migrate: %v", err))
		}
	}

	// ── OpenBao ────────────────────────────────────────────────────────
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

	instrumentedSecrets := metrics.NewInstrumentedSecretStore(openbaoClient) // iam_token_service_openbao_call_duration_seconds

	// ── Realm Provisioner client (RP-17) — the sweep's revoke must trigger
	// ClearServiceAccountKeysCache the same way cmd/scheduler's rotate does
	// (EXT-6, rev 1.3, TS-INV-7): a cached Keycloak key otherwise keeps
	// authenticating indefinitely, since there is no Keycloak-side TTL. ────
	// platform_dependency_request_seconds{dependency="realm_provisioner"}
	rpClient := metrics.NewInstrumentedRealmProvisionerClient(realmprovisioner.New(
		mustEnv("REALM_PROVISIONER_BASE_URL", appEnv, "http://iam-realm-provisioner.iam.svc.cluster.local:8080"),
		log,
	))

	// ── Repositories + enqueue-only TxRunner (no Glue/SNS — see package
	// doc comment) ────────────────────────────────────────────────────────
	credentialRepo := pgadapter.NewCredentialRepository(appPool)
	principalRepo := pgadapter.NewPrincipalRepository(appPool)
	reconcilerRepo := pgadapter.NewReconcilerRepository(reconcilerPool)
	keysRefreshRepo := pgadapter.NewKeysRefreshRepository(appPool, reconcilerPool)
	processedEventsRepo := pgadapter.NewInboxRepository(appPool, nil)

	enqueueCodec, err := eventbusadapter.NewValidatingCodec(eventbusadapter.NoopCodec{})
	if err != nil {
		panic(fmt.Sprintf("init validating codec: %v", err))
	}
	outboxPublisher := eventbusadapter.New(cfg.ServiceName, enqueueCodec).WithLogger(log)
	txRunner := pgadapter.NewTxRunner(appPool, outboxPublisher)

	// outboxRunner is constructed ONLY to call PrunePublished — Start() is
	// never called, so noopPublisher (never invoked) merely satisfies the
	// constructor's required-Publisher validation. Config still comes from
	// platform-events LoadOutbox / RunnerConfigFromEnv so prune tunables
	// share one env source with cmd/server.
	outboxEnv := eventcfg.LoadOutbox()
	eventcfg.LogWarningsTo(log, outboxEnv.Warnings)
	outboxRunner, err := outbox.NewRunner(eventcfg.RunnerConfigFromEnv(outboxEnv, appPool, noopPublisher{}, log))
	if err != nil {
		panic(fmt.Sprintf("create outbox runner: %v", err))
	}

	// ── Run all three jobs once ────────────────────────────────────────
	// Each job is a top-level span on gincommon's TracerProvider (same
	// pattern as iam-org-membership cmd/reconciler), so CronJob work shows
	// up in traces even though this binary has no HTTP middleware.
	//
	// First, RP-17 refreshes a previous run (of this job or cmd/scheduler)
	// owed but never completed (keys_refresh_pending). A failure there is a
	// sweep failure: a revoked key may still be trusted at Keycloak.
	sweepCtx, endSweep := startJobSpan(ctx, cfg.ServiceName, "overlap_sweep")
	pending := retryPendingKeyRefreshes(sweepCtx, keysRefreshRepo, rpClient, envInt("ROTATOR_BATCH_LIMIT", 500), log)
	log.Info("pending keys refresh retry complete", fieldsWithTrace(sweepCtx, map[string]interface{}{
		"refreshed": pending.Refreshed, "failed": pending.Failed, "dropped": pending.Dropped, "deferred": pending.Deferred,
	}))
	sweep := runOverlapSweep(sweepCtx, reconcilerRepo, credentialRepo, instrumentedSecrets, txRunner, keysRefreshRepo, rpClient,
		envInt("ROTATOR_BATCH_LIMIT", 500), log)
	log.Info("overlap sweep complete", fieldsWithTrace(sweepCtx, map[string]interface{}{"revoked": sweep.Revoked, "skipped": sweep.Skipped, "failed": sweep.Failed, "deferred": sweep.Deferred}))
	endSweep()
	sweep.Failed += pending.Failed
	recordSweepMetric(sweep)

	materialCtx, endMaterial := startJobSpan(ctx, cfg.ServiceName, "orphan_material")
	material := runOrphanMaterialReconciler(materialCtx, reconcilerRepo, instrumentedSecrets,
		orphanLock{tx: txRunner, principals: principalRepo, credentials: credentialRepo}, runStart.UnixMilli(), log)
	log.Info("orphan material reconciler complete", fieldsWithTrace(materialCtx, map[string]interface{}{
		"orphan_deleted": material.OrphanDeleted, "missing_material": material.MissingMaterial,
		"ok": material.OK, "failed": material.Failed, "skipped": material.Skipped, "deferred": material.Deferred,
	}))
	endMaterial()
	recordMaterialReconcileMetric(material)

	pruneCtx, endPrune := startJobSpan(ctx, cfg.ServiceName, "prune")
	prune := runPrune(pruneCtx, processedEventsRepo,
		outboxRunner,
		envInt("PROCESSED_EVENTS_TTL_DAYS", 8),
		envInt("PRUNE_BATCH_LIMIT", 10000),
		envDuration("OUTBOX_PRUNE_OLDER_THAN", 7*24*time.Hour),
		log,
	)
	rlsDeleted, rlsPruneFailed := runRLSViolationPrune(pruneCtx, pgadapter.NewRLSViolationRepository(appPool),
		envInt("RLS_VIOLATION_LOG_TTL_DAYS", 30),
		envInt("PRUNE_BATCH_LIMIT", 10000),
		log,
	)
	log.Info("prune complete", fieldsWithTrace(pruneCtx, map[string]interface{}{
		"processed_events_deleted": prune.ProcessedEventsDeleted, "outbox_deleted": prune.OutboxDeleted,
		"rls_violation_log_deleted": rlsDeleted, "deferred_steps": prune.Deferred,
	}))
	endPrune()
	if sigCtx.Err() != nil {
		log.Warn("rotator: terminated by signal — remaining work deferred to the next run", nil)
	}

	// This is a run-to-completion CronJob Pod, not a long-lived Service —
	// there is no Pushgateway in this deployment, so the only way
	// Prometheus can scrape this run's iam_token_service_rotation_sweep_total
	// / material_reconcile_total samples is a Pod-level scrape annotation
	// hitting /metrics during a brief window before the container exits.
	// Kept short (default 15s) so the CronJob still converges quickly.
	serveMetricsBriefly(log, envDuration("ROTATOR_METRICS_SCRAPE_GRACE", 15*time.Second))

	if err := appPool.DrainAndClose(context.Background()); err != nil {
		log.Error("app pool drain error", map[string]interface{}{"error": err.Error()})
	}
	if err := reconcilerPool.DrainAndClose(context.Background()); err != nil {
		log.Error("reconciler pool drain error", map[string]interface{}{"error": err.Error()})
	}
	if err := gincommon.Shutdown(log); err != nil {
		log.Error("logger/tracer flush error", map[string]interface{}{"error": err.Error()})
	}

	// A page-worthy divergence or an outright job failure both fail the
	// Kubernetes Job so it's visible via kubectl/alerting, without
	// blocking the next scheduled run (CronJob semantics) — the sweep and
	// reconciler are each independently availability-first (§8.3/§8.6), so
	// a non-zero exit here is a signal, not a retry mechanism.
	// Enumeration and prune failures count too — a run that cannot list or
	// prune anything is not a success (it would hide a broken reconciler
	// role or DSN behind a green Job).
	if sweep.Failed > 0 || material.Failed > 0 || material.MissingMaterial > 0 || prune.Failed > 0 || rlsPruneFailed {
		os.Exit(1)
	}
}

// noopPublisher satisfies outbox.Config.Publisher's required-field
// validation for the prune-only Runner in main() above; Start() is never
// called on that Runner, so Publish/PublishBatch are never invoked.
type noopPublisher struct{}

func (noopPublisher) Publish(context.Context, events.Envelope[json.RawMessage]) error { return nil }
func (noopPublisher) PublishBatch(context.Context, []events.Envelope[json.RawMessage]) error {
	return nil
}

// serveMetricsBriefly serves /metrics for grace, then shuts down — the
// window a Pod-level Prometheus scrape annotation has to capture this
// run's samples before the CronJob container exits (see call site comment
// in main()).
func serveMetricsBriefly(log port.Logger, grace time.Duration) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", gincommon.MetricsHandler())
	srv := &http.Server{Addr: ":" + envOr("METRICS_PORT", "9090"), Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			logError(context.Background(), log, "rotator metrics server error", nil, err)
		}
	case <-time.After(grace):
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logError(shutdownCtx, log, "rotator metrics server shutdown error", nil, err)
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
