package apperr_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/apperr"
)

func TestIsSeesThroughCopies(t *testing.T) {
	shared := apperr.New(http.StatusConflict, apperr.CodeConflict, "SOME_REASON", "Pesan umum.")
	other := apperr.New(http.StatusConflict, apperr.CodeConflict, "OTHER_REASON", "Pesan lain.")

	copied := shared.WithMessage("Pesan khusus.")
	require.ErrorIs(t, copied, shared, "a copy with another message is still the same error")
	require.ErrorIs(t, fmt.Errorf("wrapped: %w", shared.WithDetails(map[string]any{"x": 1})), shared)
	require.NotErrorIs(t, copied, other, "another reason is another error")

	// Without a reason an error only matches itself, as before.
	require.True(t, errors.Is(apperr.ErrNotFound, apperr.ErrNotFound))
	require.False(t, errors.Is(apperr.ErrNotFound.WithMessage("x"), apperr.ErrNotFound))
}
