package metrics

import (
	"errors"
	"fmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgmetrics"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// ObservabilityDomain is this service's business domain under the
// Enterprise Platform Observability Standard — the `domain` label on every
// platform_* series and the iam_ prefix of its own metrics. Every binary
// sets it on gincommon.Config.Domain (OBSERVABILITY_DOMAIN overrides it).
const ObservabilityDomain = "iam"

// InitLibraryMetrics registers platform-events' and platform-pgcommon's
// Tier 1 metrics on gincommon's registerer with exactly the identity
// gincommon resolved ({domain, service, environment} from
// gincommon.MetricsConstLabels), so every platform_* series in one scrape
// carries the same labels.
//
// Call it AFTER gincommon.ObservabilityMiddlewares (the identity is unknown
// before) and BEFORE pgcommon.NewPool: NewPool registers a pool's
// platform_db_pool_* connection gauges only when pgmetrics is already
// initialised. It must also run before Register so platform-events owns the
// shared platform_dependency_request_seconds / platform_duplicate_messages_total
// collectors this service records into.
//
// gincommon.MetricsRegisterer() is passed, never gincommon.LabeledRegisterer:
// both libraries inject the identity labels themselves.
//
// RegistrationWarnings (a platform_* metric disabled because the registry
// already holds the name with another shape) are logged; an invalid identity
// is returned as an error and must fail startup.
func InitLibraryMetrics(log port.Logger) error {
	labels := gincommon.MetricsConstLabels()
	if labels["domain"] == "" {
		return errors.New("library metrics: gincommon.ObservabilityMiddlewares must run first (identity labels are not set)")
	}
	reg := gincommon.MetricsRegisterer()

	evWarnings, err := events.InitMetrics(events.MetricsIdentityFromLabels(labels), reg)
	if err != nil {
		return fmt.Errorf("platform-events metrics: %w", err)
	}
	for _, w := range evWarnings {
		warn(log, "platform-events metric disabled", w.Metric, w.Err)
	}

	pgWarnings, err := pgmetrics.InitWithIdentity(pgmetrics.IdentityFromLabels(labels), reg)
	if err != nil {
		return fmt.Errorf("platform-pgcommon metrics: %w", err)
	}
	for _, w := range pgWarnings {
		warn(log, "platform-pgcommon metric disabled", w.Metric, w.Err)
	}
	return nil
}

func warn(log port.Logger, msg, metric string, err error) {
	if log == nil {
		return
	}
	fields := map[string]interface{}{"metric": metric}
	if err != nil {
		fields["error"] = err.Error()
	}
	log.Warn(msg, fields)
}
