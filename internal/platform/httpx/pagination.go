package httpx

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"siapp/internal/platform/apperr"
)

// Page limits.
const (
	DefaultLimit = 20
	MaxLimit     = 100
)

// Cursor is the position after the last row of a page: the sort timestamp and the row ID
// as tie-breaker. Queries continue with (t < cursor.T OR (t = cursor.T AND id < cursor.ID)).
type Cursor struct {
	T  time.Time `json:"t"`
	ID string    `json:"id"`
}

// EncodeCursor returns the opaque base64url form of c. The timestamp is normalised to UTC
// so the same instant always encodes to the same string.
func EncodeCursor(c Cursor) string {
	c.T = c.T.UTC()
	b, err := json.Marshal(c)
	if err != nil {
		// A struct of time.Time and string cannot fail to marshal.
		panic("httpx: encode cursor: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor parses a cursor produced by EncodeCursor. A malformed cursor is a client
// error, returned as VALIDATION_FAILED.
func DecodeCursor(s string) (Cursor, error) {
	var c Cursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || json.Unmarshal(b, &c) != nil || c.ID == "" || c.T.IsZero() {
		return Cursor{}, apperr.ErrValidationFailed.WithDetails([]FieldError{
			{Field: "cursor", Messages: []string{"tidak valid"}},
		})
	}
	return c, nil
}

// Page is the response wrapper for every list endpoint. NextCursor is null on the last page.
type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

// FetchLimit is how many rows a list query must request for a page of size limit: one
// extra row tells NewPage whether another page exists, without a COUNT query.
func FetchLimit(limit int) int32 {
	return int32(limit + 1) //nolint:gosec // limit is capped at MaxLimit by ListQuery
}

// NewPage builds a Page from rows fetched with FetchLimit. When there are more than limit
// rows it drops the extra one and sets NextCursor from the last row that is kept.
func NewPage[T any](rows []T, limit int, cursorOf func(T) Cursor) Page[T] {
	page := Page[T]{Items: rows}
	if len(rows) > limit {
		page.Items = rows[:limit]
		next := EncodeCursor(cursorOf(page.Items[limit-1]))
		page.NextCursor = &next
	}
	if page.Items == nil {
		page.Items = []T{} // "items": [], never null
	}
	return page
}
