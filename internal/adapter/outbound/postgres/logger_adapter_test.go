package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgdomain "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/domain"
)

type recordingLogger struct {
	level  string
	msg    string
	fields map[string]any
}

func (r *recordingLogger) Debug(msg string, fields map[string]any) { r.record("debug", msg, fields) }
func (r *recordingLogger) Info(msg string, fields map[string]any)  { r.record("info", msg, fields) }
func (r *recordingLogger) Warn(msg string, fields map[string]any)  { r.record("warn", msg, fields) }
func (r *recordingLogger) Error(msg string, fields map[string]any) { r.record("error", msg, fields) }

func (r *recordingLogger) record(level, msg string, fields map[string]any) {
	r.level, r.msg, r.fields = level, msg, fields
}

func TestLoggerAdapter_ForwardsEveryLevel(t *testing.T) {
	cases := []struct {
		level string
		call  func(a LoggerAdapter, msg string, fields ...pgdomain.Field)
	}{
		{"debug", LoggerAdapter.Debug},
		{"info", LoggerAdapter.Info},
		{"warn", LoggerAdapter.Warn},
		{"error", LoggerAdapter.Error},
	}
	for _, tc := range cases {
		t.Run(tc.level, func(t *testing.T) {
			rec := &recordingLogger{}
			a := NewLoggerAdapter(rec)
			tc.call(a, "hello", pgdomain.Field{Key: "k", Value: "v"})
			assert.Equal(t, tc.level, rec.level)
			assert.Equal(t, "hello", rec.msg)
			assert.Equal(t, map[string]any{"k": "v"}, rec.fields)
		})
	}
}

func TestFieldMap(t *testing.T) {
	t.Run("multiple fields", func(t *testing.T) {
		got := fieldMap([]pgdomain.Field{{Key: "a", Value: 1}, {Key: "b", Value: "two"}})
		require.Equal(t, map[string]any{"a": 1, "b": "two"}, got)
	})
	t.Run("empty", func(t *testing.T) {
		got := fieldMap(nil)
		require.Empty(t, got)
	})
}
