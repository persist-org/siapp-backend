package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"siapp/internal/platform/apperr"
	"siapp/internal/platform/timeutil"
)

func TestCursorRoundTrip(t *testing.T) {
	jayapura := time.FixedZone("WIT", 9*3600)
	in := Cursor{T: time.Date(2026, 3, 11, 6, 30, 15, 123_000_000, jayapura), ID: "0193a5d8-7c1e-7000-8000-000000000001"}

	encoded := EncodeCursor(in)
	require.NotContains(t, encoded, "=", "must be unpadded base64url")

	out, err := DecodeCursor(encoded)
	require.NoError(t, err)
	require.True(t, in.T.Equal(out.T), "same instant")
	require.Equal(t, in.ID, out.ID)

	// The same instant expressed in another zone encodes identically.
	require.Equal(t, encoded, EncodeCursor(Cursor{T: in.T.UTC(), ID: in.ID}))
}

func TestDecodeCursorRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"!!!", "e30", "bm90LWpzb24", EncodeCursor(Cursor{})} {
		_, err := DecodeCursor(bad)
		var appErr *apperr.Error
		require.ErrorAs(t, err, &appErr, bad)
		require.Equal(t, apperr.CodeValidationFailed, appErr.Code)
	}
}

func TestNewPage(t *testing.T) {
	type row struct {
		ID string
		At time.Time
	}
	at := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	rows := func(n int) []row {
		out := make([]row, n)
		for i := range out {
			out[i] = row{ID: string(rune('a' + i)), At: at.Add(time.Duration(-i) * time.Minute)}
		}
		return out
	}
	cursorOf := func(r row) Cursor { return Cursor{T: r.At, ID: r.ID} }

	t.Run("limit+1 rows: drops the extra and sets nextCursor from the last kept row", func(t *testing.T) {
		page := NewPage(rows(3), 2, cursorOf)
		require.Len(t, page.Items, 2)
		require.NotNil(t, page.NextCursor)
		c, err := DecodeCursor(*page.NextCursor)
		require.NoError(t, err)
		require.Equal(t, "b", c.ID)
	})
	t.Run("exactly limit rows: last page", func(t *testing.T) {
		page := NewPage(rows(2), 2, cursorOf)
		require.Len(t, page.Items, 2)
		require.Nil(t, page.NextCursor)
	})
	t.Run("no rows: empty list, not null", func(t *testing.T) {
		page := NewPage[row](nil, 2, cursorOf)
		require.NotNil(t, page.Items)
		require.Empty(t, page.Items)
		require.Nil(t, page.NextCursor)
	})
	t.Run("FetchLimit asks for one extra row", func(t *testing.T) {
		require.EqualValues(t, 21, FetchLimit(20))
	})
}

func TestParseListQuery(t *testing.T) {
	cursor := EncodeCursor(Cursor{T: time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC), ID: "x"})
	catID := "0193a5d8-7c1e-7000-8000-000000000001"

	parse := func(query string) (ListQuery, error) {
		return ParseListQuery(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x?"+query, nil))
	}

	t.Run("defaults", func(t *testing.T) {
		q, err := parse("")
		require.NoError(t, err)
		require.Equal(t, DefaultLimit, q.Limit)
		require.Nil(t, q.Cursor)
		require.True(t, q.Date.IsZero())
	})

	t.Run("all fields", func(t *testing.T) {
		q, err := parse("q=+budi+&date=2026-03-10&categoryId=" + catID + "&cursor=" + cursor + "&limit=50")
		require.NoError(t, err)
		require.Equal(t, "budi", q.Q)
		require.Equal(t, timeutil.NewDate(2026, time.March, 10), q.Date)
		require.Equal(t, catID, q.CategoryID)
		require.Equal(t, "x", q.Cursor.ID)
		require.Equal(t, 50, q.Limit)
	})

	t.Run("month", func(t *testing.T) {
		q, err := parse("month=2028-02")
		require.NoError(t, err)
		require.Equal(t, "2028-02", q.Month)
	})

	t.Run("limit above max is clamped", func(t *testing.T) {
		q, err := parse("limit=5000")
		require.NoError(t, err)
		require.Equal(t, MaxLimit, q.Limit)
	})

	invalid := []struct{ name, query, field string }{
		{"date with month", "date=2026-03-10&month=2026-03", "date"},
		{"bad date", "date=10-03-2026", "date"},
		{"bad month", "month=2026-13", "month"},
		{"bad category", "categoryId=abc", "categoryId"},
		{"bad cursor", "cursor=zzz", "cursor"},
		{"zero limit", "limit=0", "limit"},
		{"negative limit", "limit=-3", "limit"},
		{"non-numeric limit", "limit=many", "limit"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(tc.query)
			var appErr *apperr.Error
			require.ErrorAs(t, err, &appErr)
			require.Equal(t, apperr.CodeValidationFailed, appErr.Code)
			fields, ok := appErr.Details.([]FieldError)
			require.True(t, ok)
			require.Equal(t, tc.field, fields[0].Field)
		})
	}
}
