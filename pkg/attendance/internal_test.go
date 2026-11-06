package attendance

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"siapp/pkg/organization"
	"siapp/pkg/platform/geo"
	"siapp/pkg/platform/timeutil"
)

func TestShiftRefRoundTrips(t *testing.T) {
	key := shiftKey{TeamCode: "1K-IBMI-S1", WorkDate: timeutil.NewDate(2026, 3, 10), ShiftID: 42}
	got, ok := parseShiftRef(key.ref())
	require.True(t, ok)
	require.Equal(t, key, got)
}

func TestParseShiftRefRefusesAnythingElse(t *testing.T) {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for name, ref := range map[string]string{
		"empty":           "",
		"not base64":      "!!!",
		"padded base64":   base64.URLEncoding.EncodeToString([]byte("a|2026-03-10|1")),
		"two parts":       enc("1K-IBMI-S1|2026-03-10"),
		"four parts":      enc("1K-IBMI-S1|2026-03-10|1|x"),
		"no team":         enc("|2026-03-10|1"),
		"bad date":        enc("1K-IBMI-S1|2026-13-40|1"),
		"bad shift id":    enc("1K-IBMI-S1|2026-03-10|x"),
		"zero shift id":   enc("1K-IBMI-S1|2026-03-10|0"),
		"negative shift":  enc("1K-IBMI-S1|2026-03-10|-5"),
		"shift too large": enc("1K-IBMI-S1|2026-03-10|99999999999999999999"),
	} {
		t.Run(name, func(t *testing.T) {
			_, ok := parseShiftRef(ref)
			require.False(t, ok)
		})
	}
}

func TestLateMinutes(t *testing.T) {
	start := time.Date(2026, 3, 10, 1, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return start.Add(d) }

	for _, tc := range []struct {
		name      string
		at        time.Time
		tolerance int
		want      int
	}{
		{"early", at(-30 * time.Minute), 15, 0},
		{"on the dot", at(0), 15, 0},
		{"exactly the tolerance", at(15 * time.Minute), 15, 0},
		{"one second past the tolerance counts its whole minutes", at(15*time.Minute + time.Second), 15, 15},
		{"past the tolerance counts from the start", at(16 * time.Minute), 15, 16},
		{"a partial minute is dropped", at(16*time.Minute + 59*time.Second), 15, 16},
		{"an hour late", at(time.Hour), 15, 60},
		{"no tolerance", at(time.Minute), 0, 1},
		{"no tolerance, seconds only", at(30 * time.Second), 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, lateMinutes(start, tc.at, tc.tolerance))
		})
	}
}

func TestMeasure(t *testing.T) {
	lat, lng := -6.2, 106.8
	project := func(strict bool) organization.Project {
		return organization.Project{Latitude: &lat, Longitude: &lng, AttendanceRadiusMeters: 150, StrictAttendanceGeofence: strict}
	}
	at := func(metersNorth float64) *geo.Point { return &geo.Point{Lat: lat + metersNorth/111195, Lng: lng} }

	for _, tc := range []struct {
		name    string
		project organization.Project
		fix     *geo.Point
		inArea  bool
		refused bool
	}{
		{"on the site", project(true), at(0), true, false},
		{"inside the radius", project(true), at(100), true, false},
		{"outside, strict", project(true), at(400), false, true},
		{"outside, loose", project(false), at(400), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := measure(tc.project, tc.fix)
			if tc.refused {
				require.ErrorIs(t, err, ErrOutsideArea)
				return
			}
			require.NoError(t, err)
			require.True(t, m.inArea.Valid)
			require.Equal(t, tc.inArea, m.inArea.Bool)
			require.True(t, m.distance.Valid)
		})
	}

	t.Run("no fix measures nothing", func(t *testing.T) {
		m, err := measure(project(true), nil)
		require.NoError(t, err)
		require.False(t, m.inArea.Valid)
		require.False(t, m.distance.Valid)
	})

	t.Run("a project without a point is not configured", func(t *testing.T) {
		_, err := measure(organization.Project{AttendanceRadiusMeters: 150}, at(0))
		require.ErrorIs(t, err, organization.ErrProjectNotConfigured)
	})
}
