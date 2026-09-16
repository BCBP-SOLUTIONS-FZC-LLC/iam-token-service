package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClampOverlapSeconds(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"below minimum clamps to minimum", -1, MinOverlapSeconds},
		{"far below minimum still clamps to minimum", -1000, MinOverlapSeconds},
		{"at minimum is unchanged", MinOverlapSeconds, MinOverlapSeconds},
		{"in range is unchanged", 300, 300},
		{"at maximum is unchanged", MaxOverlapSeconds, MaxOverlapSeconds},
		{"above maximum clamps to maximum", MaxOverlapSeconds + 1, MaxOverlapSeconds},
		{"far above maximum still clamps to maximum", 100000, MaxOverlapSeconds},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ClampOverlapSeconds(tc.in))
		})
	}
}
