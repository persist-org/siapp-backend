package clock

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFixed(t *testing.T) {
	at := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	var c Clock = Fixed{T: at}
	require.Equal(t, at, c.Now())
}

func TestSystemIsUTC(t *testing.T) {
	require.Equal(t, time.UTC, System{}.Now().Location())
}
