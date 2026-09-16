package postgres

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// nullableUUID is a pure helper — no Postgres needed, unlike the rest of
// this package's tests (test/postgres, tag=integration).
func TestNullableUUID(t *testing.T) {
	t.Run("zero UUID becomes nil so gen_random_uuid() default fires", func(t *testing.T) {
		assert.Nil(t, nullableUUID(uuid.Nil))
	})
	t.Run("non-zero UUID passes through unchanged", func(t *testing.T) {
		id := uuid.New()
		assert.Equal(t, id, nullableUUID(id))
	})
}
