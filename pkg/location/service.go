package location

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"siapp/internal/db"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/presence"
	"siapp/pkg/realtime"
)

const (
	// MaxBatch is the most points one request may carry.
	MaxBatch = 100
	// maxAge is how old a point may be: a phone that was offline for a shift can still send what
	// it recorded, but a day-old point says nothing about where anyone is.
	maxAge = 24 * time.Hour
	// maxFuture is how far ahead of the server clock a point may claim to be (clock skew).
	maxFuture = 5 * time.Minute
	// broadcastEvery is the most often one person's position is pushed to the maps.
	broadcastEvery = 2 * time.Second
	// frameType is what the maps receive (realtime doc §6).
	frameType = "location.broadcast"
)

// Point is one GPS fix as the device recorded it.
type Point struct {
	LoggedAt       time.Time
	Latitude       float64
	Longitude      float64
	AccuracyMeters float64
	// BatteryPct is nil when the device did not report it.
	BatteryPct *int
}

// Result says what became of a batch. Accepted counts the points that passed the checks, whether
// they were stored now or were already stored (a batch sent again is accepted again, so the
// device can drop it from its queue). Rejected counts those dropped for being too old, too far
// in the future or out of range.
type Result struct {
	Accepted int
	Rejected int
}

// Presence is what Service needs of presence.Service.
type Presence interface {
	Touch(ctx context.Context, orgID, personnelID int64) error
	ManyIn(ctx context.Context, q *db.Queries, orgID int64, ids []int64) (map[int64]presence.Status, error)
}

// Service stores GPS points, keeps each person's last location and answers the maps.
type Service struct {
	pool      *pgxpool.Pool
	rdb       redis.UniversalClient
	prefix    string
	clock     clock.Clock
	shifts    Shifts
	presence  Presence
	publisher realtime.Publisher
	logger    *slog.Logger
}

// NewService returns the Service. prefix goes in front of every Redis key, like presence's.
func NewService(pool *pgxpool.Pool, rdb redis.UniversalClient, prefix string, c clock.Clock, shifts Shifts, p Presence,
	publisher realtime.Publisher, logger *slog.Logger,
) *Service {
	return &Service{pool: pool, rdb: rdb, prefix: prefix, clock: c, shifts: shifts, presence: p, publisher: publisher, logger: logger}
}

func (s *Service) broadcastKey(orgID, personnelID int64) string {
	return s.prefix + "org:" + strconv.FormatInt(orgID, 10) + ":location:broadcast:" + strconv.FormatInt(personnelID, 10)
}

// Ingest stores the points of one person. REST and the WebSocket both come through here.
//
// ⚠ The privacy contract. A position is only stored while the person is on a shift at a project
// whose gps_tracking_enabled is on (DL-48): off duty, or at a project that has tracking off,
// nothing is recorded and the answer is TRACKING_DISABLED, so even a phone that keeps sending
// leaves no trail. "On shift" is the same notion as for a check-in (attendance.Scheduled).
func (s *Service) Ingest(ctx context.Context, p auth.Principal, points []Point) (Result, error) {
	if len(points) > MaxBatch {
		return Result{}, ErrBatchTooLarge
	}
	now := s.clock.Now()
	sites, err := s.shifts.OnShift(ctx, p.OrganizationID, p.PersonnelID, now)
	if err != nil {
		return Result{}, fmt.Errorf("location: current shifts: %w", err)
	}
	sites = slices.DeleteFunc(slices.Clone(sites), func(site Site) bool { return !site.TrackingEnabled })
	if len(sites) == 0 {
		return Result{}, ErrTrackingDisabled
	}

	valid := make([]Point, 0, len(points))
	for _, pt := range points {
		if pt.acceptable(now) {
			pt.LoggedAt = pt.LoggedAt.UTC().Truncate(time.Microsecond) // what Postgres keeps
			valid = append(valid, pt)
		}
	}
	res := Result{Accepted: len(valid), Rejected: len(points) - len(valid)}
	if len(valid) == 0 {
		return res, nil
	}
	newest := valid[0]
	for _, pt := range valid[1:] {
		if pt.LoggedAt.After(newest.LoggedAt) {
			newest = pt
		}
	}

	raw, err := encodePoints(valid)
	if err != nil {
		return Result{}, err
	}
	var moved bool
	err = database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		if _, err := q.InsertLocationLogs(ctx, db.InsertLocationLogsParams{PersonnelID: p.PersonnelID, Points: raw}); err != nil {
			return fmt.Errorf("insert location logs: %w", err)
		}
		// ⚠ The last location only moves forward (the query's WHERE): offline batches arrive out
		// of order and must never put a person "back in time".
		n, err := q.UpsertLastLocation(ctx, db.UpsertLastLocationParams{
			PersonnelID: p.PersonnelID, LoggedAt: pgtype.Timestamptz{Time: newest.LoggedAt, Valid: true},
			Latitude: newest.Latitude, Longitude: newest.Longitude, AccuracyMeters: newest.AccuracyMeters,
		})
		if err != nil {
			return fmt.Errorf("upsert last location: %w", err)
		}
		moved = n > 0
		return nil
	})
	if err != nil {
		return Result{}, fmt.Errorf("location: store points: %w", err)
	}

	// A location ping is a sign of life. Presence and the broadcast are best effort: the points
	// are already safe, and a Redis hiccup must not make the device send the batch again.
	if err := s.presence.Touch(ctx, p.OrganizationID, p.PersonnelID); err != nil {
		s.logger.WarnContext(ctx, "location: presence touch failed", slog.String("error", err.Error()))
	}
	if moved {
		s.broadcast(ctx, p, sites, newest)
	}
	return res, nil
}

func (pt Point) acceptable(now time.Time) bool {
	switch {
	case math.IsNaN(pt.Latitude) || math.IsNaN(pt.Longitude) || math.IsNaN(pt.AccuracyMeters),
		math.IsInf(pt.Latitude, 0) || math.IsInf(pt.Longitude, 0) || math.IsInf(pt.AccuracyMeters, 0):
		return false
	case pt.Latitude < -90 || pt.Latitude > 90 || pt.Longitude < -180 || pt.Longitude > 180:
		return false
	case pt.AccuracyMeters < 0 || pt.AccuracyMeters > 999999.99:
		return false
	case pt.BatteryPct != nil && (*pt.BatteryPct < 0 || *pt.BatteryPct > 100):
		return false
	}
	return !pt.LoggedAt.Before(now.Add(-maxAge)) && !pt.LoggedAt.After(now.Add(maxFuture))
}

func encodePoints(points []Point) ([]byte, error) {
	type row struct {
		LoggedAt       time.Time `json:"logged_at"`
		Latitude       float64   `json:"latitude"`
		Longitude      float64   `json:"longitude"`
		AccuracyMeters float64   `json:"accuracy_meters"`
		BatteryPct     *int      `json:"battery_pct"`
	}
	rows := make([]row, len(points))
	for i, pt := range points {
		rows[i] = row(pt)
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return nil, fmt.Errorf("location: encode points: %w", err)
	}
	return raw, nil
}

// broadcast pushes the newest point to the regu and project tracking rooms of the person's
// shifts, at most once per person per broadcastEvery. The debounce is a Redis key set with NX, so
// it holds across API instances: a phone that sends every second costs the maps one frame per two.
//
// The frame names ids and codes only (the realtime doc: no name goes through a socket); the map
// already holds each person's card from REST.
func (s *Service) broadcast(ctx context.Context, p auth.Principal, sites []Site, pt Point) {
	first, err := s.rdb.SetNX(ctx, s.broadcastKey(p.OrganizationID, p.PersonnelID), 1, broadcastEvery).Result()
	if err != nil {
		s.logger.WarnContext(ctx, "location: broadcast debounce failed", slog.String("error", err.Error()))
		return
	}
	if !first {
		return
	}
	var rooms []string
	for _, site := range sites {
		for _, room := range []string{realtime.TeamRoom(site.TeamCode, realtime.ChannelTracking), realtime.ProjectRoom(site.ProjectCode, realtime.ChannelTracking)} {
			if !slices.Contains(rooms, room) {
				rooms = append(rooms, room)
			}
		}
	}
	data := map[string]any{
		"personnel_id": p.PersonnelID, "logged_at": pt.LoggedAt.UTC(),
		"latitude": pt.Latitude, "longitude": pt.Longitude, "accuracy_meters": pt.AccuracyMeters,
		"team_codes": teamCodesOf(sites),
	}
	for _, room := range rooms {
		if err := s.publisher.Publish(ctx, p.OrganizationID, room, frameType, data); err != nil {
			s.logger.WarnContext(ctx, "location: broadcast failed", slog.String("room", room), slog.String("error", err.Error()))
		}
	}
}

func teamCodesOf(sites []Site) []string {
	var out []string
	for _, s := range sites {
		if !slices.Contains(out, s.TeamCode) {
			out = append(out, s.TeamCode)
		}
	}
	return out
}
