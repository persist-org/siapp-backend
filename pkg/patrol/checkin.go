package patrol

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/timeutil"
)

// maxTrackingDays is the longest range GET /zones/{id}/tracking answers: a month of check-ins is a
// report, more is an export.
const maxTrackingDays = 31

// PresenceInput is the body of a check-in and of a check-out: the same QR and position checks as a
// scan. A check-in is a live act of the app, so the time is the server's, not the phone's.
type PresenceInput struct {
	QRPayload      string   `json:"qr_payload" validate:"required,max=200"`
	Latitude       *float64 `json:"latitude" validate:"required,gte=-90,lte=90"`
	Longitude      *float64 `json:"longitude" validate:"required,gte=-180,lte=180"`
	AccuracyMeters *float64 `json:"accuracy_meters" validate:"required,gte=0"`
}

// CheckIn is a guard's stay in a zone, from check-in to check-out.
type CheckIn struct {
	ID          int64
	ZoneID      int64
	PersonnelID int64
	// FullName is set where the reader needs to know who it is (live, tracking).
	FullName   string
	TeamCode   string
	ShiftID    *int64
	CheckInAt  time.Time
	CheckInLat float64
	CheckInLng float64
	// The check-out fields are nil while the guard is still there.
	CheckOutAt  *time.Time
	CheckOutLat *float64
	CheckOutLng *float64
}

func checkInOf(r db.ZoneCheckin, name string) CheckIn {
	out := CheckIn{
		ID: r.ID, ZoneID: r.ZoneID, PersonnelID: r.PersonnelID, FullName: name, TeamCode: r.TeamCode,
		CheckInAt: r.CheckInAt.Time, CheckInLat: r.CheckInLat, CheckInLng: r.CheckInLng,
		CheckOutLat: r.CheckOutLat, CheckOutLng: r.CheckOutLng,
	}
	if r.ShiftID.Valid {
		out.ShiftID = &r.ShiftID.Int64
	}
	if r.CheckOutAt.Valid {
		out.CheckOutAt = &r.CheckOutAt.Time
	}
	return out
}

// loadVisit reads the zone and its project for a check-in or check-out. A switched-off zone is not
// one a guard can check in to.
func loadZone(ctx context.Context, q *db.Queries, zoneID int64) (db.PatrolZone, db.Project, error) {
	zone, err := q.GetPatrolZone(ctx, zoneID)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.PatrolZone{}, db.Project{}, ErrZoneNotFound
	}
	if err != nil {
		return db.PatrolZone{}, db.Project{}, fmt.Errorf("get zone: %w", err)
	}
	project, err := q.GetProject(ctx, zone.ProjectCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.PatrolZone{}, db.Project{}, ErrZoneNotFound
	}
	if err != nil {
		return db.PatrolZone{}, db.Project{}, fmt.Errorf("get project: %w", err)
	}
	return zone, project, nil
}

// CheckIn marks the person present in the zone. They need the zone's QR and to be inside its
// geofence, like a scan, and they can be in one zone at a time: a check-in while another is open is
// ZONE_ALREADY_CHECKED_IN.
func (s *Service) CheckIn(ctx context.Context, p auth.Principal, zoneID int64, in PresenceInput) (CheckIn, error) {
	var out CheckIn
	now := s.clock.Now()
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		zone, project, err := loadZone(ctx, q, zoneID)
		if err != nil {
			return err
		}
		if !zone.Active {
			return ErrZoneNotFound
		}
		v, err := s.admit(ctx, q, p, zone, project, in.QRPayload, *in.Latitude, *in.Longitude, *in.AccuracyMeters, now)
		if err != nil {
			return err
		}
		pl, err := s.place(ctx, q, p, v, now)
		if err != nil {
			return err
		}
		row, err := q.InsertZoneCheckin(ctx, db.InsertZoneCheckinParams{
			ZoneID: zoneID, PersonnelID: p.PersonnelID, TeamCode: pl.team, ShiftID: int8Param(pl.shiftID),
			At: ts(now), Latitude: *in.Latitude, Longitude: *in.Longitude,
		})
		if database.IsUniqueViolation(err) && database.ConstraintName(err) == "zone_checkins_open_uq" {
			return ErrAlreadyCheckedIn
		}
		if err != nil {
			return fmt.Errorf("insert check-in: %w", err)
		}
		out = checkInOf(row, "")
		return nil
	})
	return out, err
}

// CheckOut ends the person's stay in the zone. Without an open check-in of this zone it is
// ZONE_NOT_CHECKED_IN. The same QR and geofence checks apply as at check-in, so a check-out is
// proof that the guard was still there.
func (s *Service) CheckOut(ctx context.Context, p auth.Principal, zoneID int64, in PresenceInput) (CheckIn, error) {
	var out CheckIn
	now := s.clock.Now()
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		zone, project, err := loadZone(ctx, q, zoneID)
		if err != nil {
			return err
		}
		// A zone switched off while someone is in it can still be checked out of, or the guard
		// would be stuck there: nothing else closes an open check-in.
		if _, err := s.admit(ctx, q, p, zone, project, in.QRPayload, *in.Latitude, *in.Longitude, *in.AccuracyMeters, now); err != nil {
			return err
		}
		row, err := q.CloseZoneCheckin(ctx, db.CloseZoneCheckinParams{
			ZoneID: zoneID, PersonnelID: p.PersonnelID, At: ts(now), Latitude: in.Latitude, Longitude: in.Longitude,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotCheckedIn
		}
		if err != nil {
			return fmt.Errorf("close check-in: %w", err)
		}
		out = checkInOf(row, "")
		return nil
	})
	return out, err
}

// readReach says whose check-ins the caller may read in the zone: all of the regu in the reach of
// their approve permission (a DANRU), or only their own (anybody else who serves in the project).
// A person who does not serve in the zone's project reads nothing.
func (s *Service) readReach(ctx context.Context, q *db.Queries, p auth.Principal, zone db.PatrolZone, project db.Project) (teams []string, own bool, err error) {
	sc, err := s.scopeOf(ctx, q, p, zone, project)
	return sc.teams, sc.own, err
}

// readScope is what a caller may read of one zone: the regu of their approve permission when they
// are a DANRU, otherwise only their own records (own), and in any case the regu they serve in
// there (mine), which is where a member's own team is.
type readScope struct {
	teams []string
	own   bool
	mine  []string
}

func (s *Service) scopeOf(ctx context.Context, q *db.Queries, p auth.Principal, zone db.PatrolZone, project db.Project) (readScope, error) {
	if s.perms == nil {
		return readScope{}, errors.New("patrol: permissions are not plugged in")
	}
	loc, err := time.LoadLocation(project.Timezone)
	if err != nil {
		return readScope{}, fmt.Errorf("project %s: timezone %q: %w", project.Code, project.Timezone, err)
	}
	day := timeutil.BusinessDate(s.clock.Now(), loc)
	mine, err := s.membershipIn(ctx, q, p.PersonnelID, zone, day)
	if err != nil {
		return readScope{}, access.ErrPermissionDenied
	}
	perm := access.PatrolApprove
	if zone.Type == db.PatrolZoneTypeGUARD {
		perm = access.GuardApprove
	}
	res, err := s.perms.Resolve(ctx, q, p.PersonnelID, perm, day)
	if err != nil {
		return readScope{}, err
	}
	if res.Allowed && !res.SelfOnly {
		return readScope{teams: res.TeamCodes, mine: mine}, nil
	}
	return readScope{own: true, mine: mine}, nil
}

// Live returns who is checked in to the zone now.
func (s *Service) Live(ctx context.Context, p auth.Principal, zoneID int64) ([]CheckIn, error) {
	var out []CheckIn
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		zone, project, err := loadZone(ctx, q, zoneID)
		if err != nil {
			return err
		}
		teams, own, err := s.readReach(ctx, q, p, zone, project)
		if err != nil {
			return err
		}
		rows, err := q.ListZoneLive(ctx, db.ListZoneLiveParams{
			ZoneID: zoneID, TeamCodes: teamsFilter(teams, own), PersonnelID: ownFilter(p, own),
		})
		if err != nil {
			return fmt.Errorf("list live: %w", err)
		}
		out = make([]CheckIn, 0, len(rows))
		for _, r := range rows {
			out = append(out, checkInOf(zoneCheckinOf(r), r.FullName))
		}
		return nil
	})
	return out, err
}

// TrackingFilter narrows GET /zones/{id}/tracking.
type TrackingFilter struct {
	// From and To are business dates in the project's timezone, both included.
	From, To    timeutil.Date
	TeamCode    string
	PersonnelID int64
	Limit       int
	Offset      int
}

// Tracking returns the check-ins of the zone in a date range, newest first, and their total.
func (s *Service) Tracking(ctx context.Context, p auth.Principal, zoneID int64, f TrackingFilter) ([]CheckIn, int, error) {
	if f.To.Before(f.From) {
		return nil, 0, apperr.ErrValidation.WithFields(apperr.FieldError{Field: "to", Code: "INVALID", Message: "Tidak boleh sebelum from."})
	}
	if f.To.After(f.From.AddDays(maxTrackingDays - 1)) {
		return nil, 0, apperr.ErrValidation.WithFields(apperr.FieldError{
			Field: "to", Code: "INVALID", Message: fmt.Sprintf("Rentang maksimum %d hari.", maxTrackingDays),
		})
	}
	var out []CheckIn
	var total int64
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		zone, project, err := loadZone(ctx, q, zoneID)
		if err != nil {
			return err
		}
		teams, own, err := s.readReach(ctx, q, p, zone, project)
		if err != nil {
			return err
		}
		loc, err := time.LoadLocation(project.Timezone)
		if err != nil {
			return fmt.Errorf("project %s: timezone %q: %w", project.Code, project.Timezone, err)
		}
		// The range is whole days of the project: its midnight to the midnight after To.
		from, to := timeutil.At(f.From, 0, loc), timeutil.At(f.To.AddDays(1), 0, loc)

		filterTeams := teamsFilter(teams, own)
		if f.TeamCode != "" {
			// A regu outside the caller's reach is refused, not silently empty.
			if !own && !containsString(teams, f.TeamCode) {
				return access.ErrPermissionDenied
			}
			filterTeams = []string{f.TeamCode}
		}
		personnel := ownFilter(p, own)
		if f.PersonnelID > 0 {
			if own && f.PersonnelID != p.PersonnelID {
				return access.ErrPermissionDenied
			}
			personnel = pgtype.Int8{Int64: f.PersonnelID, Valid: true}
		}
		rows, err := q.ListZoneTracking(ctx, db.ListZoneTrackingParams{
			ZoneID: zoneID, FromAt: ts(from), ToAt: ts(to), TeamCodes: filterTeams, PersonnelID: personnel,
			PageLimit: int32(f.Limit), PageOffset: int32(f.Offset), //nolint:gosec // bounded by ParseListQuery
		})
		if err != nil {
			return fmt.Errorf("list tracking: %w", err)
		}
		total, err = q.CountZoneTracking(ctx, db.CountZoneTrackingParams{
			ZoneID: zoneID, FromAt: ts(from), ToAt: ts(to), TeamCodes: filterTeams, PersonnelID: personnel,
		})
		if err != nil {
			return fmt.Errorf("count tracking: %w", err)
		}
		out = make([]CheckIn, 0, len(rows))
		for _, r := range rows {
			out = append(out, checkInOf(trackingRowOf(r), r.FullName))
		}
		return nil
	})
	return out, int(total), err
}

// teamsFilter is the regu a query is narrowed to: those of the reach, or nil (no narrowing by regu)
// when the caller reads only their own.
func teamsFilter(teams []string, own bool) []string {
	if own {
		return nil
	}
	if teams == nil {
		return []string{}
	}
	return teams
}

func ownFilter(p auth.Principal, own bool) pgtype.Int8 {
	if !own {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: p.PersonnelID, Valid: true}
}

func int8Param(v *int64) pgtype.Int8 {
	if v == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *v, Valid: true}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func zoneCheckinOf(r db.ListZoneLiveRow) db.ZoneCheckin {
	return db.ZoneCheckin{
		ID: r.ID, OrganizationID: r.OrganizationID, ZoneID: r.ZoneID, PersonnelID: r.PersonnelID, TeamCode: r.TeamCode,
		ShiftID: r.ShiftID, CheckInAt: r.CheckInAt, CheckInLat: r.CheckInLat, CheckInLng: r.CheckInLng,
		CheckOutAt: r.CheckOutAt, CheckOutLat: r.CheckOutLat, CheckOutLng: r.CheckOutLng,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func trackingRowOf(r db.ListZoneTrackingRow) db.ZoneCheckin {
	return db.ZoneCheckin{
		ID: r.ID, OrganizationID: r.OrganizationID, ZoneID: r.ZoneID, PersonnelID: r.PersonnelID, TeamCode: r.TeamCode,
		ShiftID: r.ShiftID, CheckInAt: r.CheckInAt, CheckInLat: r.CheckInLat, CheckInLng: r.CheckInLng,
		CheckOutAt: r.CheckOutAt, CheckOutLat: r.CheckOutLat, CheckOutLng: r.CheckOutLng,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}
