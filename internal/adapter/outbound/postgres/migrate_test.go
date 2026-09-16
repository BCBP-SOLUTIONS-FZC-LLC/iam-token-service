package postgres

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/stretchr/testify/assert"
)

func TestRunMigrations_WithLogger_LoggerAssignedBeforeUpCall(t *testing.T) {
	err := RunMigrations(context.Background(), "postgres://invalid:5432/does_not_exist", &migrateTestLogger{})
	assert.Error(t, err, "Up() must fail on an invalid DSN")
}

type migrateTestLogger struct{}

func (*migrateTestLogger) Debug(string, map[string]interface{}) {}
func (*migrateTestLogger) Info(string, map[string]interface{})  {}
func (*migrateTestLogger) Warn(string, map[string]interface{})  {}
func (*migrateTestLogger) Error(string, map[string]interface{}) {}

var _ port.Logger = (*migrateTestLogger)(nil)
