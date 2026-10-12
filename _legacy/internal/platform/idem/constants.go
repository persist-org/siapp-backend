package idem

import "time"

const (
	// maxFutureSkew is how far ahead of the server an offline device clock may be.
	maxFutureSkew = 5 * time.Minute
	// maxAge is how long a device may hold a record offline before it is refused.
	maxAge = 7 * 24 * time.Hour
)
