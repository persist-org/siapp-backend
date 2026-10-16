package identity_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/pkg/identity"
)

func TestMaskNIK(t *testing.T) {
	tests := map[string]string{
		"":                 "",
		"1":                "*",
		"1234":             "****",
		"12345":            "*2345",
		"SIAPPA0001":       "******0001",
		"3271234567891234": "************1234",
		// Masking counts characters, not bytes.
		"ÀÉÎÕÜ1234": "*****1234",
	}
	for nik, want := range tests {
		t.Run(nik, func(t *testing.T) {
			require.Equal(t, want, identity.MaskNIK(nik))
		})
	}
}
