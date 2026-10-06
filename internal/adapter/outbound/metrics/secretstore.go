package metrics

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// InstrumentedSecretStore wraps a port.SecretStore and records call
// duration around Write and Delete — the two ops §11.2 freezes labels for
// — on both the legacy Tier-3 iam_token_service_openbao_call_duration_seconds{op}
// and the registry-proposed Tier-1 platform_dependency_request_seconds{dependency="openbao",operation,outcome}
// (Enterprise Platform Observability Standard, dual-emitted during the
// compatibility period). Read and List are recorded on the Tier-1 metric
// only: the Tier-3 op label set is frozen at write|delete (§11.2), but Read
// is the JWKS route's hot path (one per live credential per Keycloak fetch)
// and List is the reconciler's and the offboarding cascade's enumeration,
// so their errors and latency must be visible.
//
// Defined in the metrics package (not on openbao.Client itself) because
// the openbao adapter component may depend only on domain/port
// (.go-arch-lint.yml) — this decorator depends on port only, so any
// port.SecretStore implementation can be wrapped, not just OpenBao's.
type InstrumentedSecretStore struct {
	inner port.SecretStore
}

// NewInstrumentedSecretStore wraps inner with call-duration instrumentation.
func NewInstrumentedSecretStore(inner port.SecretStore) *InstrumentedSecretStore {
	return &InstrumentedSecretStore{inner: inner}
}

var _ port.SecretStore = (*InstrumentedSecretStore)(nil)

// Write delegates to inner, recording write call duration.
func (s *InstrumentedSecretStore) Write(ctx context.Context, path string, secret string) error {
	start := time.Now()
	err := s.inner.Write(ctx, path, secret)
	observeOpenBaoCall("write", err, time.Since(start).Seconds())
	return err
}

// Read delegates to inner, recording read call duration (Tier 1 only).
func (s *InstrumentedSecretStore) Read(ctx context.Context, path string) (string, error) {
	start := time.Now()
	secret, err := s.inner.Read(ctx, path)
	observeDependencyOnly("read", err, time.Since(start).Seconds())
	return secret, err
}

// Delete delegates to inner, recording delete call duration.
func (s *InstrumentedSecretStore) Delete(ctx context.Context, path string) error {
	start := time.Now()
	err := s.inner.Delete(ctx, path)
	observeOpenBaoCall("delete", err, time.Since(start).Seconds())
	return err
}

// List delegates to inner, recording list call duration (Tier 1 only).
func (s *InstrumentedSecretStore) List(ctx context.Context, pathPrefix string) ([]string, error) {
	start := time.Now()
	keys, err := s.inner.List(ctx, pathPrefix)
	observeDependencyOnly("list", err, time.Since(start).Seconds())
	return keys, err
}

// observeOpenBaoCall records elapsed seconds on both the legacy Tier-3
// OpenBaoCallDuration and the registry-proposed Tier-1
// DependencyRequestDuration. Nil-checked (matching dedup.go's defensive
// style) so a decorator constructed before Register runs — e.g. in a test
// that forgets to call it — degrades to a no-op instead of a nil-pointer
// panic.
//
// outcome is "error" when the call returned an error, else "success" — the
// registry's outcome vocabulary for platform_dependency_request_seconds.
func observeOpenBaoCall(op string, err error, elapsedSeconds float64) {
	if OpenBaoCallDuration != nil {
		OpenBaoCallDuration.WithLabelValues(op).Observe(elapsedSeconds)
	}
	observeDependencyOnly(op, err, elapsedSeconds)
}

// observeDependencyOnly records one OpenBao call on the Tier-1
// platform_dependency_request_seconds only.
func observeDependencyOnly(op string, err error, elapsedSeconds float64) {
	observeDependency(DependencyOpenBao, op, err, elapsedSeconds)
}

// observeDependency records one call to dependency on
// platform_dependency_request_seconds with the registry outcome vocabulary.
func observeDependency(dependency, operation string, err error, elapsedSeconds float64) {
	if DependencyRequestDuration == nil {
		return
	}
	outcome := OutcomeSuccess
	if err != nil {
		outcome = OutcomeError
	}
	DependencyRequestDuration.WithLabelValues(dependency, operation, outcome).Observe(elapsedSeconds)
}
