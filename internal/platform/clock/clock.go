// Package clock abstracts the current time so services can be tested at a chosen instant.
package clock

import "time"

// Clock returns the current time.
type Clock interface {
	Now() time.Time
}

// System is the real clock. It always returns UTC.
type System struct{}

// Now returns the current time in UTC.
func (System) Now() time.Time { return time.Now().UTC() }

// Fixed is a clock frozen at T, for tests.
type Fixed struct{ T time.Time }

// Now returns T.
func (f Fixed) Now() time.Time { return f.T }
