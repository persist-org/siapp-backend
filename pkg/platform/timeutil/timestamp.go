package timeutil

import "time"

const timestampLayout = "2006-01-02T15:04:05.000Z"

// FormatTimestamp renders t the way the API sends timestamps: RFC 3339, UTC, always with
// three fractional digits ("2026-03-10T06:30:00.000Z"). encoding/json would drop trailing
// zeros, which makes the format vary from one value to the next.
func FormatTimestamp(t time.Time) string {
	return t.UTC().Format(timestampLayout)
}
