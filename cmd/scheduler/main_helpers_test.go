package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDefaultOverlapSecondsFromEnv(t *testing.T) {
	t.Run("unset uses the built-in default", func(t *testing.T) {
		t.Setenv("ROTATION_DEFAULT_OVERLAP_SECONDS", "")
		assert.Equal(t, 300, defaultOverlapSecondsFromEnv())
	})
	t.Run("in range is used as is", func(t *testing.T) {
		t.Setenv("ROTATION_DEFAULT_OVERLAP_SECONDS", "900")
		assert.Equal(t, 900, defaultOverlapSecondsFromEnv())
	})
	t.Run("zero is a legitimate hard cutover", func(t *testing.T) {
		t.Setenv("ROTATION_DEFAULT_OVERLAP_SECONDS", "0")
		assert.Equal(t, 0, defaultOverlapSecondsFromEnv())
	})
	for _, bad := range []string{"901", "-1", "abc", "1.5"} {
		t.Run("invalid "+bad+" fails startup (TS-CONFIG-4)", func(t *testing.T) {
			t.Setenv("ROTATION_DEFAULT_OVERLAP_SECONDS", bad)
			assert.Panics(t, func() { defaultOverlapSecondsFromEnv() })
		})
	}
}
