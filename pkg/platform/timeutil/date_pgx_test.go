package timeutil_test

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/testkit"
	"siapp/pkg/platform/timeutil"
)

func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }

// A Date must survive a trip through a real Postgres date: encoded as a parameter and
// scanned back, including the zero Date (NULL) and the first and last day of a month.
func TestDatePostgresRoundTrip(t *testing.T) {
	pool := testkit.Pool(t)

	for _, d := range []timeutil.Date{
		timeutil.NewDate(2026, time.March, 5),
		timeutil.NewDate(2028, time.February, 29),
		timeutil.NewDate(2026, time.December, 31),
	} {
		var got timeutil.Date
		require.NoError(t, pool.QueryRow(t.Context(), "SELECT $1::date", d).Scan(&got))
		require.Equal(t, d, got)
	}

	t.Run("zero Date is NULL both ways", func(t *testing.T) {
		var isNull bool
		require.NoError(t, pool.QueryRow(t.Context(), "SELECT $1::date IS NULL", timeutil.Date{}).Scan(&isNull))
		require.True(t, isNull)

		got := timeutil.NewDate(2026, time.March, 5)
		require.NoError(t, pool.QueryRow(t.Context(), "SELECT NULL::date").Scan(&got))
		require.True(t, got.IsZero())
	})

	t.Run("stored as the calendar day, never shifted by a timezone", func(t *testing.T) {
		var asText string
		require.NoError(t, pool.QueryRow(t.Context(), "SELECT ($1::date)::text", timeutil.NewDate(2026, time.March, 5)).Scan(&asText))
		require.Equal(t, "2026-03-05", asText)
	})
}
