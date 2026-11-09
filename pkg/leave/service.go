package leave

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/timeutil"
)

// deltaOverlap is how far back a delta pull moves its lower bound, so a row whose transaction
// committed just after the client's last pull is not lost (see masterdata: updated_at is the
// statement time, the row is visible at commit). A change of the last seconds can arrive twice,
// which is harmless: the app upserts by code.
const deltaOverlap = 5 * time.Second

// RegionLookup answers which koordinator wilayah a person serves under today. It is what a
// REGION_ADMIN's reach over a person is checked against; organization.Directory is one.
type RegionLookup interface {
	RegionCodesOf(ctx context.Context, q *db.Queries, personnelID int64) ([]string, error)
}

// UsedDays is how many days of a category a person has used, or has pending, in a calendar year.
// Part B plugs it in from the requests; until then nothing is used.
type UsedDays func(ctx context.Context, q *db.Queries, personnelID, categoryID int64, year int) (int, error)

// Service is the leave use cases.
type Service struct {
	pool    *pgxpool.Pool
	clock   clock.Clock
	regions RegionLookup
	used    UsedDays
}

// NewService returns the Service. regions may be nil until it is wired: a REGION_ADMIN is then
// refused every person's ledger, as outside their reach.
func NewService(pool *pgxpool.Pool, clk clock.Clock) *Service {
	return &Service{pool: pool, clock: clk}
}

// UseRegionLookup plugs in the lookup of a person's regions. Call it while wiring.
func (s *Service) UseRegionLookup(r RegionLookup) { s.regions = r }

// UseUsedDays plugs in the count of used days. Call it while wiring.
func (s *Service) UseUsedDays(f UsedDays) { s.used = f }

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func textOrNull(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func boolOrNull(b *bool) pgtype.Bool {
	if b == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *b, Valid: true}
}

func intOrNull(n *int) pgtype.Int4 {
	if n == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*n), Valid: true} //nolint:gosec // validated to a small range by the request
}

func textPtrOrNull(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

func pgInt8(n int64) pgtype.Int8 { return pgtype.Int8{Int64: n, Valid: true} }

func pgInt8Ptr(n *int64) pgtype.Int8 {
	if n == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *n, Valid: true}
}

func dateParam(d timeutil.Date) pgtype.Date { return pgtype.Date{Time: d.Time(), Valid: true} }

func lower(s string) string { return strings.ToLower(s) }

func int8Ptr(n pgtype.Int8) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

// sinceBound is the lower bound of a delta pull: a little before since, and never before the
// epoch (a device with a broken clock pulls from the start).
func sinceBound(since time.Time) time.Time {
	if since.Before(time.Unix(0, 0)) {
		since = time.Unix(0, 0)
	}
	return since.Add(-deltaOverlap)
}
