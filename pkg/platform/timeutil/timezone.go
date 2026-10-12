package timeutil

import (
	"fmt"
	"time"
)

// SupportedTimezones are the IANA zones a project may use: WIB, WITA and WIT.
var SupportedTimezones = []string{"Asia/Jakarta", "Asia/Makassar", "Asia/Jayapura"}

// BusinessDate returns the calendar date of t in loc. Business dates (work_date,
// occurred_date) must be computed this way in the project timezone, never from the UTC
// timestamp: 23:30 UTC is already the next day in all three Indonesian zones.
func BusinessDate(t time.Time, loc *time.Location) Date {
	y, m, d := t.In(loc).Date()
	return NewDate(y, m, d)
}

// At returns the UTC instant for minuteOfDay minutes after midnight of d in loc.
// Indonesia has no daylight saving, so the offset is the same all year.
func At(d Date, minuteOfDay int, loc *time.Location) time.Time {
	y, m, day := d.t.Date()
	return time.Date(y, m, day, 0, minuteOfDay, 0, 0, loc).UTC()
}

// MonthRange returns the first and last day of a "YYYY-MM" month.
func MonthRange(month string) (first, last Date, err error) {
	t, err := time.Parse("2006-01", month)
	if err != nil {
		return Date{}, Date{}, fmt.Errorf("parse month %q: %w", month, err)
	}
	first = NewDate(t.Year(), t.Month(), 1)
	// Day 0 of the next month is the last day of this one, so leap years need no table.
	last = NewDate(t.Year(), t.Month()+1, 0)
	return first, last, nil
}
