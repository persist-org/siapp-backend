package timeutil

import (
	"encoding/json"
	"testing"
	"time"
	_ "time/tzdata" // the tests must not depend on the host's zoneinfo

	"github.com/stretchr/testify/require"
)

func loadLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	require.NoError(t, err)
	return loc
}

func TestBusinessDate(t *testing.T) {
	tests := []struct {
		name string
		utc  time.Time
		zone string
		want string
	}{
		// 23:30 UTC is 06:30 / 07:30 / 08:30 the next morning: the day rolls over everywhere.
		{"23:30 UTC in Jakarta", time.Date(2026, 3, 10, 23, 30, 0, 0, time.UTC), "Asia/Jakarta", "2026-03-11"},
		{"23:30 UTC in Makassar", time.Date(2026, 3, 10, 23, 30, 0, 0, time.UTC), "Asia/Makassar", "2026-03-11"},
		{"23:30 UTC in Jayapura", time.Date(2026, 3, 10, 23, 30, 0, 0, time.UTC), "Asia/Jayapura", "2026-03-11"},
		// 16:30 UTC is where the zones differ: still the 10th in WIB, already the 11th in WITA/WIT.
		{"16:30 UTC in Jakarta", time.Date(2026, 3, 10, 16, 30, 0, 0, time.UTC), "Asia/Jakarta", "2026-03-10"},
		{"16:30 UTC in Makassar", time.Date(2026, 3, 10, 16, 30, 0, 0, time.UTC), "Asia/Makassar", "2026-03-11"},
		{"16:30 UTC in Jayapura", time.Date(2026, 3, 10, 16, 30, 0, 0, time.UTC), "Asia/Jayapura", "2026-03-11"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, BusinessDate(tc.utc, loadLoc(t, tc.zone)).String())
		})
	}
}

func TestAt(t *testing.T) {
	jayapura := loadLoc(t, "Asia/Jayapura") // UTC+9
	d := NewDate(2026, time.March, 11)

	got := At(d, 6*60+30, jayapura)
	require.Equal(t, time.Date(2026, 3, 10, 21, 30, 0, 0, time.UTC), got)
	require.Equal(t, time.UTC, got.Location())

	require.Equal(t, time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC), At(d, 0, jayapura))
}

func TestMonthRange(t *testing.T) {
	tests := []struct {
		month       string
		first, last string
	}{
		{"2028-02", "2028-02-01", "2028-02-29"}, // leap year
		{"2026-02", "2026-02-01", "2026-02-28"},
		{"2026-04", "2026-04-01", "2026-04-30"},
		{"2026-12", "2026-12-01", "2026-12-31"},
	}
	for _, tc := range tests {
		first, last, err := MonthRange(tc.month)
		require.NoError(t, err, tc.month)
		require.Equal(t, tc.first, first.String())
		require.Equal(t, tc.last, last.String())
	}

	for _, bad := range []string{"", "2026-13", "2026-2", "2026-02-01", "abc"} {
		_, _, err := MonthRange(bad)
		require.Error(t, err, bad)
	}
}

func TestDateJSON(t *testing.T) {
	type payload struct {
		D Date  `json:"d"`
		P *Date `json:"p"`
	}
	in := payload{D: NewDate(2026, time.March, 5)}

	b, err := json.Marshal(in)
	require.NoError(t, err)
	require.JSONEq(t, `{"d":"2026-03-05","p":null}`, string(b))

	var out payload
	require.NoError(t, json.Unmarshal(b, &out))
	require.Equal(t, in.D, out.D)
	require.Nil(t, out.P)

	zero, err := json.Marshal(Date{})
	require.NoError(t, err)
	require.Equal(t, "null", string(zero))

	for _, bad := range []string{`"2026-3-5"`, `"2026-02-30"`, `"05/03/2026"`, `20260305`, `""`} {
		var d Date
		require.Error(t, json.Unmarshal([]byte(bad), &d), bad)
	}
}

func TestDateSQL(t *testing.T) {
	d := NewDate(2026, time.March, 5)

	v, err := d.Value()
	require.NoError(t, err)

	var back Date
	require.NoError(t, back.Scan(v))
	require.Equal(t, d, back)

	// A DATE the driver returns in a non-UTC location keeps its calendar day.
	jayapura := loadLoc(t, "Asia/Jayapura")
	require.NoError(t, back.Scan(time.Date(2026, 3, 5, 0, 0, 0, 0, jayapura)))
	require.Equal(t, d, back)

	require.NoError(t, back.Scan([]byte("2026-03-05")))
	require.Equal(t, d, back)
	require.NoError(t, back.Scan("2026-03-05"))
	require.Equal(t, d, back)

	require.NoError(t, back.Scan(nil))
	require.True(t, back.IsZero())
	v, err = Date{}.Value()
	require.NoError(t, err)
	require.Nil(t, v)

	require.Error(t, back.Scan(42))
	require.Error(t, back.Scan("not a date"))
}

func TestDateArithmetic(t *testing.T) {
	d := NewDate(2028, time.February, 28)
	require.Equal(t, "2028-02-29", d.AddDays(1).String())
	require.Equal(t, "2028-03-01", d.AddDays(2).String())
	require.True(t, d.Before(d.AddDays(1)))
	require.True(t, d.AddDays(1).After(d))
	require.False(t, d.Before(d))
}
