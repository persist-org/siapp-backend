package organization

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
)

// Conversions between the request and domain types (pointers: "not sent" is nil) and the
// pgtype values sqlc takes and returns (Valid: false is NULL).

// nullableID is a PATCH field that can be left out (Set false), cleared with null (Set true,
// Value nil) or set. A plain pointer cannot tell the first two apart, so a value once set
// could never be removed.
type nullableID struct {
	Set   bool
	Value *int64
}

// UnmarshalJSON records that the field was sent; encoding/json calls it for null too.
func (n *nullableID) UnmarshalJSON(b []byte) error {
	n.Set = true
	return json.Unmarshal(b, &n.Value)
}

// param is the value for a CASE WHEN @set THEN @value query argument.
func (n nullableID) param() pgtype.Int8 { return int8OrNull(n.Value) }

// textValue is a nullable text column as a pointer.
func textValue(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

// trimmed returns s without surrounding spaces, nil staying nil.
func trimmed(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	return &t
}

func int4OrNull(n *int) pgtype.Int4 {
	if n == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: clampInt32(*n), Valid: true}
}

func int8OrNull(n *int64) pgtype.Int8 {
	if n == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *n, Valid: true}
}

func intValue(n pgtype.Int4) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int32)
	return &v
}

func int64Value(n pgtype.Int8) *int64 {
	if !n.Valid {
		return nil
	}
	return &n.Int64
}

// personnelRef is the caller as the created_by / modify_by of a row.
func personnelRef(actor auth.Principal) pgtype.Int8 {
	return pgtype.Int8{Int64: actor.PersonnelID, Valid: true}
}

// clampInt32 narrows a request number to what the column holds. The validate tags bound
// every number the API takes far below this; the clamp only keeps the conversion honest.
func clampInt32(n int) int32 {
	switch {
	case n > math.MaxInt32:
		return math.MaxInt32
	case n < math.MinInt32:
		return math.MinInt32
	}
	return int32(n) //nolint:gosec // bounded just above
}

// wrapUnexpected returns an API error as it is and gives anything else the context of what
// failed.
func wrapUnexpected(err error, what string) error {
	var appErr *apperr.Error
	if errors.As(err, &appErr) {
		return err
	}
	return fmt.Errorf("%s: %w", what, err)
}
