package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestError_Status(t *testing.T) {
	cases := []struct {
		code ErrorCode
		want int
	}{
		{ErrMissingIdentityHeaders, 401},
		{ErrPrincipalNotFound, 404},
		{ErrRotationInFlight, 409},
		{ErrOptimisticLockConflict, 409},
		{ErrPrincipalRevoked, 422},
		{ErrSecretStoreUnavailable, 502},
		{ErrInvalidRequest, 400},
		{ErrDBUnavailable, 503},
		{ErrorCode("something_unmapped"), 500},
	}
	for _, tc := range cases {
		t.Run(string(tc.code), func(t *testing.T) {
			e := NewError(tc.code, "msg")
			assert.Equal(t, tc.want, e.Status())
		})
	}
}
