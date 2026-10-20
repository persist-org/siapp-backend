// Package timeutil holds the calendar helpers for business dates and project timezones.
package timeutil

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

const dateLayout = time.DateOnly // "2006-01-02"

// Date is a calendar day without a time or zone: a business date such as work_date.
// It is JSON "YYYY-MM-DD" and a Postgres date. The zero value is "no date".
//
// Internally it is midnight UTC, so two Dates compare with == and sort with Before/After
// regardless of where they were built.
type Date struct{ t time.Time }

// NewDate returns the date for the given year, month and day.
func NewDate(year int, month time.Month, day int) Date {
	return Date{time.Date(year, month, day, 0, 0, 0, 0, time.UTC)}
}

// ParseDate parses a strict "YYYY-MM-DD" string.
func ParseDate(s string) (Date, error) {
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return Date{}, fmt.Errorf("parse date %q: %w", s, err)
	}
	return Date{t}, nil
}

// String returns "YYYY-MM-DD", or "" for the zero Date.
func (d Date) String() string {
	if d.IsZero() {
		return ""
	}
	return d.t.Format(dateLayout)
}

// IsZero reports whether d is the zero Date.
func (d Date) IsZero() bool { return d.t.IsZero() }

// Time returns midnight UTC of the date.
func (d Date) Time() time.Time { return d.t }

// AddDays returns the date n days later (or earlier when n is negative).
func (d Date) AddDays(n int) Date { return Date{d.t.AddDate(0, 0, n)} }

// Before reports whether d is earlier than other.
func (d Date) Before(other Date) bool { return d.t.Before(other.t) }

// After reports whether d is later than other.
func (d Date) After(other Date) bool { return d.t.After(other.t) }

// MarshalJSON encodes the date as "YYYY-MM-DD", or null for the zero Date.
func (d Date) MarshalJSON() ([]byte, error) {
	if d.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(d.String())
}

// UnmarshalJSON decodes "YYYY-MM-DD". JSON null leaves the Date unchanged.
//
// A value that is not such a date is a *json.UnmarshalTypeError, which encoding/json decorates
// with the field it came from, so an API can answer "field X must be YYYY-MM-DD" instead of
// "the body could not be read".
func (d *Date) UnmarshalJSON(b []byte) error {
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return &json.UnmarshalTypeError{Value: string(b), Type: reflect.TypeFor[Date]()}
	}
	parsed, err := ParseDate(s)
	if err != nil {
		return &json.UnmarshalTypeError{Value: s, Type: reflect.TypeFor[Date]()}
	}
	*d = parsed
	return nil
}

// DateValue implements pgtype.DateValuer, so pgx encodes a Date as a Postgres date (NULL
// for the zero Date).
func (d Date) DateValue() (pgtype.Date, error) {
	if d.IsZero() {
		return pgtype.Date{}, nil
	}
	return pgtype.Date{Time: d.t, Valid: true}, nil
}

// ScanDate implements pgtype.DateScanner, so pgx scans a Postgres date into a Date. NULL
// and infinite dates become the zero Date.
func (d *Date) ScanDate(v pgtype.Date) error {
	if !v.Valid || v.InfinityModifier != pgtype.Finite {
		*d = Date{}
		return nil
	}
	// Take the calendar fields as reported; converting zones here could shift the day.
	*d = NewDate(v.Time.Year(), v.Time.Month(), v.Time.Day())
	return nil
}

// Value implements driver.Valuer for database/sql users: a date, or NULL for the zero Date.
func (d Date) Value() (driver.Value, error) {
	if d.IsZero() {
		return nil, nil //nolint:nilnil // NULL is the correct driver value
	}
	return d.t, nil
}

// Scan implements sql.Scanner for a date column.
func (d *Date) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*d = Date{}
	case time.Time:
		*d = NewDate(v.Year(), v.Month(), v.Day())
	case []byte:
		return d.scanString(string(v))
	case string:
		return d.scanString(v)
	default:
		return fmt.Errorf("scan date: unsupported type %T", src)
	}
	return nil
}

func (d *Date) scanString(s string) error {
	parsed, err := ParseDate(s)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}
