// Package httpx supplies the single HTTP transport this service's outbound
// adapters build on, so W3C traceparent injection and client-span emission
// cannot be forgotten on a new call site (§11.3) — mirrors the sibling
// Realm Provisioner's identical package. Currently used only by
// internal/adapter/outbound/realmprovisioner (§16 TSQ-6 Resolved,
// cmd/scheduler's RP-17 relay); this service otherwise makes no outbound
// HTTP calls (TS-INV-1 — it never calls Keycloak, and RP-17 is the one
// exception, an internal-service call, not Keycloak itself).
package httpx

import (
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// NewTransport wraps base — nil meaning http.DefaultTransport — with the
// OpenTelemetry round-tripper, which injects the traceparent header carried
// on the request context and records a client span per call. Both
// composition roots call gincommon.InitTracingWithConfig unconditionally, so
// the global propagator and tracer provider are already installed.
func NewTransport(base http.RoundTripper) http.RoundTripper {
	return otelhttp.NewTransport(base)
}

// NewClient returns an http.Client with the instrumented transport and the
// given per-request timeout.
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: NewTransport(nil)}
}
