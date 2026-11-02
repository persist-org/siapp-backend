package notifications_test

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"siapp/pkg/notifications"
)

func TestCursorRoundTrips(t *testing.T) {
	// A time with microseconds, the precision of timestamptz: the cursor has to keep every digit,
	// or the next page would start before or after the row it means.
	at := time.Date(2026, time.October, 30, 3, 4, 5, 123456000, time.UTC)

	encoded := notifications.Cursor{UpdatedAt: at, ID: 4401}.Encode()

	require.Regexp(t, regexp.MustCompile(`^[A-Za-z0-9_-]+$`), encoded, "safe in a query string as it is")
	got, err := notifications.ParseCursor(encoded)
	require.NoError(t, err)
	require.True(t, at.Equal(got.UpdatedAt), "got %s", got.UpdatedAt)
	require.Equal(t, int64(4401), got.ID)
}

func TestParseCursorRefusesWhatIsNotOne(t *testing.T) {
	encode := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

	for name, in := range map[string]string{
		"empty":                    "",
		"too long":                 strings.Repeat("A", 129),
		"not base64":               "%%%",
		"padded base64":            base64.URLEncoding.EncodeToString([]byte(`{"u":1,"i":1}`)) + "=",
		"not json":                 encode("hello"),
		"json of another shape":    encode(`[1,2]`),
		"a string where a number":  encode(`{"u":"1","i":1}`),
		"a negative id":            encode(`{"u":1,"i":-1}`),
		"a time before the epoch":  encode(`{"u":-1,"i":1}`),
		"a number too big for 64b": encode(`{"u":99999999999999999999,"i":1}`),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := notifications.ParseCursor(in)
			require.Error(t, err)
		})
	}
}
