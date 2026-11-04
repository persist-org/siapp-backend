package masterdata

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/personel"
)

// deltaOverlap is how far back a delta pull moves its lower bound, so a row whose transaction
// committed just after the client's last pull is not lost (the same reasoning as the inbox's:
// updated_at is the statement time, the row is visible only at commit). The price is that a change
// of the last few seconds can arrive twice, which is harmless: the app upserts by id.
const deltaOverlap = 5 * time.Second

// LeaveSource is where leave categories come from: Personel, through its cache.
type LeaveSource interface {
	LeaveTypes(ctx context.Context, orgID int64) ([]personel.LeaveType, error)
}

// UsageCheck reports whether a record elsewhere points at the master with this id. It runs in the
// tenant transaction of the delete, on the row's lock, so a reference cannot appear between the
// check and the delete without waiting for it.
type UsageCheck func(ctx context.Context, q *db.Queries, id int64) (bool, error)

// Service is the master data use cases.
type Service struct {
	pool  *pgxpool.Pool
	clock clock.Clock
	leave LeaveSource

	// Modules that point at a master register a check, while the router is wired: reports and
	// instructions for their categories (11, 12), panic for its types (09).
	categoryUsage  map[db.CategoryKind][]UsageCheck
	panicTypeUsage []UsageCheck
}

// NewService returns the service. leave may be nil: /permission-categories then answers 503 until
// Personel serves leave types.
func NewService(pool *pgxpool.Pool, clk clock.Clock, leave LeaveSource) *Service {
	return &Service{pool: pool, clock: clk, leave: leave, categoryUsage: map[db.CategoryKind][]UsageCheck{}}
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
