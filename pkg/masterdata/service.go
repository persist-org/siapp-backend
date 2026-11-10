package masterdata

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/media"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/clock"
)

// deltaOverlap is how far back a delta pull moves its lower bound, so a row whose transaction
// committed just after the client's last pull is not lost (the same reasoning as the inbox's:
// updated_at is the statement time, the row is visible only at commit). The price is that a change
// of the last few seconds can arrive twice, which is harmless: the app upserts by id.
const deltaOverlap = 5 * time.Second

// LogoStore is what a letterhead's logo needs from the media module: claiming the uploaded photo
// for the letterhead, and a signed URL to show it. *media.Service is one.
type LogoStore interface {
	Attach(ctx context.Context, q *db.Queries, in media.AttachInput) error
	Detach(ctx context.Context, q *db.Queries, entityType string, entityID int64, keep []int64) error
	Views(ctx context.Context, q *db.Queries, entityType string, entityID int64, ids []int64) ([]media.View, error)
}

// UsageCheck reports whether a record elsewhere points at the master with this id. It runs in the
// tenant transaction of the delete, on the row's lock, so a reference cannot appear between the
// check and the delete without waiting for it.
type UsageCheck func(ctx context.Context, q *db.Queries, id int64) (bool, error)

// Service is the master data use cases.
type Service struct {
	pool  *pgxpool.Pool
	clock clock.Clock

	// Modules that point at a master register a check, while the router is wired: reports and
	// instructions for their categories (11, 12), panic for its types (09).
	categoryUsage   map[db.CategoryKind][]UsageCheck
	panicTypeUsage  []UsageCheck
	letterheadUsage []UsageCheck
	logos           LogoStore
}

// NewService returns the service.
func NewService(pool *pgxpool.Pool, clk clock.Clock) *Service {
	return &Service{pool: pool, clock: clk, categoryUsage: map[db.CategoryKind][]UsageCheck{}}
}

// UseCategoryUsage registers a check that stops the delete of a category of this kind that
// something points at. Call it while wiring, before the service takes requests.
func (s *Service) UseCategoryUsage(kind Kind, check UsageCheck) {
	s.categoryUsage[kind] = append(s.categoryUsage[kind], check)
}

// UsePanicTypeUsage registers a check that stops the delete of a panic type that something
// points at. Call it while wiring, before the service takes requests.
func (s *Service) UsePanicTypeUsage(check UsageCheck) {
	s.panicTypeUsage = append(s.panicTypeUsage, check)
}

// UseLetterheadUsage registers a check that stops the delete of a letterhead that something
// points at (exports, module 14). Call it while wiring, before the service takes requests.
func (s *Service) UseLetterheadUsage(check UsageCheck) {
	s.letterheadUsage = append(s.letterheadUsage, check)
}

// UseLogoStore plugs in the media module, which letterhead logos go through. Without it a
// request that names a logo is a server error, not a client one: the router must wire it.
func (s *Service) UseLogoStore(l LogoStore) { s.logos = l }

// Optional is a field of a patch that can be left out, set, or set to null (to clear it). A plain
// pointer cannot tell "absent" from "null", and a patch needs to.
//
// ⚠ The value is read in two steps. UnmarshalJSON only keeps the raw JSON; the use case calls
// resolve with the field's name, which reads it. encoding/json does not pass a field's name down to
// a custom unmarshaler, so a wrong type ({"url": 42}) decoded there would be reported without the
// field it is about.
type Optional[T any] struct {
	// Set is true when the field was in the request, null or not.
	Set bool
	// Value is nil for null. It is filled by resolve.
	Value *T
	raw   json.RawMessage
}

// UnmarshalJSON marks the field as sent and keeps its JSON for resolve.
func (o *Optional[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	o.raw = append(o.raw[:0], b...)
	return nil
}

// resolve reads the value the request sent, or says that it is of the wrong type.
func (o *Optional[T]) resolve(field string) *apperr.FieldError {
	if !o.Set || o.Value != nil || bytes.Equal(bytes.TrimSpace(o.raw), []byte("null")) {
		return nil
	}
	var v T
	if err := json.Unmarshal(o.raw, &v); err != nil {
		kind := "nilai yang sesuai"
		switch any(v).(type) {
		case string:
			kind = "teks"
		case int64, int, int32:
			kind = "angka"
		}
		return &apperr.FieldError{Field: field, Code: "INVALID", Message: "Harus bertipe " + kind + "."}
	}
	o.Value = &v
	return nil
}

// resolveAll resolves the optional fields of a request and reports every wrong one at once.
func resolveAll(problems ...*apperr.FieldError) error {
	var fields []apperr.FieldError
	for _, p := range problems {
		if p != nil {
			fields = append(fields, *p)
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return apperr.ErrValidation.WithFields(fields...)
}

// Tombstone says a row is gone for the app's cache: deleted, deactivated, or no longer offered to
// the caller.
type Tombstone struct {
	ID        int64
	DeletedAt time.Time
}

// Changes is the answer of a delta pull: rows to upsert and tombstones to drop, and the server
// time the next pull starts from. There is no cursor: a master is a few dozen rows.
type Changes[T any] struct {
	ServerTime time.Time
	Upserted   []T
	Deleted    []Tombstone
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func textOrNull(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func textPtr(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

func boolOrNull(b *bool) pgtype.Bool {
	if b == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *b, Valid: true}
}

func pgInt8(n int64) pgtype.Int8 { return pgtype.Int8{Int64: n, Valid: true} }

func int8Ptr(n *int64) pgtype.Int8 {
	if n == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *n, Valid: true}
}

func int8Value(n pgtype.Int8) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

func textValue(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	v := t.String
	return &v
}

func intPtr(n *int) pgtype.Int4 {
	if n == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*n), Valid: true} //nolint:gosec // validated to 0..10000 by the request
}

// sinceBound is the lower bound of a delta pull: a little before `since`, and never before the
// epoch (a device with a broken clock pulls from the start).
func sinceBound(since time.Time) time.Time {
	if since.Before(time.Unix(0, 0)) {
		since = time.Unix(0, 0)
	}
	return since.Add(-deltaOverlap)
}

// maxNameLength is the longest name a master takes, in characters.
const maxNameLength = 100

// cleanName is a master's name as it is stored and compared: runs of spaces, tabs and newlines
// become one space and the ends are trimmed, so "Kejadian  Besar" and "Kejadian Besar" are one
// name. A name that is empty after that, too long, or has a control character is refused.
func cleanName(raw string) (string, error) {
	name := strings.Join(strings.Fields(raw), " ")
	switch {
	case name == "":
		return "", validationError("name", "Wajib diisi.")
	case utf8.RuneCountInString(name) > maxNameLength:
		return "", validationError("name", "Maksimum 100 karakter.")
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return "", validationError("name", "Tidak boleh berisi karakter kontrol.")
	}
	return name, nil
}

// isAppErr reports whether err is an error meant for the client (an *apperr.Error).
func isAppErr(err error) bool {
	_, ok := errors.AsType[*apperr.Error](err)
	return ok
}
