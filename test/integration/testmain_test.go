//go:build integration

// Package integration_test is the "Postgres + OpenBao" full integration
// suite the Makefile's test-integration target already referenced before
// this package existed. Unlike test/postgres (Postgres only, fakes stand in
// for OpenBao) and test/e2e (real Postgres + router, fake OpenBao), this
// package is the one place both real backends run together — see
// credential_service_test.go for the combined lifecycle test, and
// openbao_test.go for OpenBao-only coverage of
// internal/adapter/outbound/openbao/client.go against a real container.
//
// Requires Docker on the runner. Tag: integration.
package integration_test

import (
	"os"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

func TestMain(m *testing.M) {
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{
		ServiceName:  "iam-token-service",
		BuildVersion: "test",
	})
	metrics.Register("test")
	os.Exit(m.Run())
}
