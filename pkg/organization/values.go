package organization

import (
	"math"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"siapp/pkg/platform/auth"
)

// Conversions between the request and domain types (pointers: "not sent" is nil) and the
// pgtype values sqlc takes and returns (Valid: false is NULL).

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
