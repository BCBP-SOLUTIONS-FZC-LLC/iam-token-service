package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/migrate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTolerateNewerSchema(t *testing.T) {
	newer := fmt.Errorf("%w: version 99", pgmigrate.ErrVersionNotInSource)

	t.Run("newer schema is tolerated and logged", func(t *testing.T) {
		log := &recordingLogger{}
		assert.True(t, tolerateNewerSchema(newer, "domain migrations", log))
		assert.Equal(t, "warn", log.level)
		assert.Equal(t, "domain migrations", log.fields["step"])
		assert.Equal(t, newer.Error(), log.fields["error"])
	})
	t.Run("newer schema is tolerated with a nil logger", func(t *testing.T) {
		assert.True(t, tolerateNewerSchema(newer, "outbox schema", nil))
	})
	t.Run("any other error is not tolerated", func(t *testing.T) {
		log := &recordingLogger{}
		assert.False(t, tolerateNewerSchema(errors.New("connection refused"), "outbox schema", log))
		assert.Empty(t, log.level, "nothing is logged for a non-tolerated error")
	})
}

func TestMigrate_UnreachableDatabaseFailsAtOutboxStep(t *testing.T) {
	log := &recordingLogger{}
	err := Migrate(context.Background(), "postgres://u:p@127.0.0.1:1/none?sslmode=disable&connect_timeout=1", log)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outbox schema:")
	assert.Empty(t, log.level, "a connection failure is not the tolerated newer-schema case")
}

func TestMigrationsEnabledFromEnv(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{
		{val: "", want: true},
		{val: "true", want: true},
		{val: "false", want: false},
		{val: " FALSE ", want: false},
		{val: "no", want: true},
	} {
		t.Run(fmt.Sprintf("%q", tc.val), func(t *testing.T) {
			t.Setenv("RUN_MIGRATIONS", tc.val)
			assert.Equal(t, tc.want, MigrationsEnabledFromEnv())
		})
	}
}

func TestMigrateOnlyFromEnv(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{
		{val: "", want: false},
		{val: "false", want: false},
		{val: "1", want: false},
		{val: "true", want: true},
		{val: " True ", want: true},
	} {
		t.Run(fmt.Sprintf("%q", tc.val), func(t *testing.T) {
			t.Setenv("MIGRATE_ONLY", tc.val)
			assert.Equal(t, tc.want, MigrateOnlyFromEnv())
		})
	}
}
