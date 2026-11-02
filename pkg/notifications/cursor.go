package notifications

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Cursor is a position in the delta stream: the (updated_at, id) of the last row a page sent.
// The next page starts strictly after it. It is opaque to clients (next_cursor).
//
// A cursor names no person and grants nothing: every query that takes one is already limited to
// the caller's own rows, so a forged or borrowed cursor can only move the caller around in
// their own inbox.
type Cursor struct {
	UpdatedAt time.Time
	ID        int64
}

// cursorWire is the encoded form. The time is in microseconds, the precision of timestamptz, so
// that a cursor read from a row compares equal to that row in the database.
type cursorWire struct {
	Micros int64 `json:"u"`
	ID     int64 `json:"i"`
}

var errBadCursor = errors.New("notifications: malformed cursor")

// Encode returns the cursor as next_cursor: unpadded base64url of a small JSON object.
func (c Cursor) Encode() string {
	raw := fmt.Appendf(nil, `{"u":%d,"i":%d}`, c.UpdatedAt.UnixMicro(), c.ID)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// ParseCursor reads a cursor a client sent back. Text that does not decode to a position is an
// error, so the handler can answer 422 instead of starting from nowhere.
func ParseCursor(s string) (Cursor, error) {
	if s == "" || len(s) > maxCursorLength {
		return Cursor{}, errBadCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, errBadCursor
	}
	var w cursorWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return Cursor{}, errBadCursor
	}
	// A row id starts at 1, and a position before the epoch is not one this server wrote.
	if w.ID < 0 || w.Micros < 0 {
		return Cursor{}, errBadCursor
	}
	return Cursor{UpdatedAt: time.UnixMicro(w.Micros).UTC(), ID: w.ID}, nil
}
