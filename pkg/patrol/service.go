package patrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/organization"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/timeutil"
)

// Service keeps the zones and their checklists, and takes the scans and check-ins of the guards.
type Service struct {
	pool  *pgxpool.Pool
	clock clock.Clock

	// Plugged in at start-up by the Use methods; scans and check-ins need the first three.
	shifts   ShiftSource
	perms    *access.Service
	attach   Attacher
	notifier Notifier
}

// NewService returns the Service.
func NewService(pool *pgxpool.Pool, c clock.Clock) *Service {
	return &Service{pool: pool, clock: c}
}

// UseShifts plugs in the source of a person's shifts, which decides the regu and the work date of a
// visit (organization.NewShifts).
func (s *Service) UseShifts(sh ShiftSource) { s.shifts = sh }

// ShiftSource is the part of organization.LocalShifts the patrol needs. It reads on the transaction
// the patrol already holds: a second connection per request would deadlock the pool under load.
type ShiftSource interface {
	CurrentIn(ctx context.Context, q *db.Queries, personnelID int64, at time.Time) ([]organization.ShiftInstance, error)
}

// UsePermissions plugs in the field permission matrix: who may scan, who may see who is checked in.
func (s *Service) UsePermissions(p *access.Service) { s.perms = p }

// UseAttachments plugs in the module that takes uploaded photos into a record (media.Service).
func (s *Service) UseAttachments(a Attacher) { s.attach = a }

// UseNotifier plugs in the sender of notices. Without one, nothing is sent.
func (s *Service) UseNotifier(n Notifier) { s.notifier = n }

// Now is the service's clock, for the handler's check of a scan's occurred_at.
func (s *Service) Now() time.Time { return s.clock.Now() }

// ZoneInput is a request to create a zone.
type ZoneInput struct {
	ProjectCode  string         `json:"project_code" validate:"required,notblank,max=100"`
	TeamCode     *string        `json:"team_code" validate:"omitempty,max=100"`
	Type         string         `json:"type" validate:"required,oneof=PATROL GUARD"`
	Name         string         `json:"name" validate:"required,notblank,max=200"`
	Alias        *string        `json:"alias" validate:"omitempty,max=200"`
	Description  *string        `json:"description" validate:"omitempty,max=2000"`
	PopupText    *string        `json:"popup_text" validate:"omitempty,max=2000"`
	Latitude     *float64       `json:"latitude" validate:"omitempty,gte=-90,lte=90"`
	Longitude    *float64       `json:"longitude" validate:"omitempty,gte=-180,lte=180"`
	RadiusMeters *int           `json:"radius_meters" validate:"omitempty,min=1,max=5000"`
	MinVisit     *int           `json:"min_visit" validate:"omitempty,min=1,max=100"`
	StartDate    *timeutil.Date `json:"start_date"`
	Active       *bool          `json:"is_active"`
	// CheckpointIDs are the zone's checkpoints, in order (positions 1..n).
	CheckpointIDs []string `json:"checkpoint_ids" validate:"omitempty,max=100"`
}

// ZoneUpdate is a request to change a zone. A field that is left out keeps its value; null
// empties the ones that may be empty. The project is not here: a zone does not move, because
// the scans it holds belong to that project's people.
type ZoneUpdate struct {
	TeamCode     optional[string]  `json:"team_code"`
	Type         *string           `json:"type" validate:"omitempty,oneof=PATROL GUARD"`
	Name         *string           `json:"name" validate:"omitempty,notblank,max=200"`
	Alias        optional[string]  `json:"alias"`
	Description  optional[string]  `json:"description"`
	PopupText    optional[string]  `json:"popup_text"`
	Latitude     optional[float64] `json:"latitude"`
	Longitude    optional[float64] `json:"longitude"`
	RadiusMeters optional[int]     `json:"radius_meters"`
	MinVisit     *int              `json:"min_visit" validate:"omitempty,min=1,max=100"`
	StartDate    optional[string]  `json:"start_date"`
	Active       *bool             `json:"is_active"`
	// CheckpointIDs, when sent, replaces the zone's checkpoints and their order; left out, they stay.
	CheckpointIDs *[]string `json:"checkpoint_ids" validate:"omitempty,max=100"`
}

// optional is a field of a patch that can be left out, set, or set to null. encoding/json does
// not tell a custom unmarshaler the name of its field, so UnmarshalJSON only keeps the JSON and
// resolve (called with the name) reads it, which keeps the field in a type error.
type optional[T any] struct {
	set   bool
	null  bool
	value T
	raw   json.RawMessage
}

// UnmarshalJSON marks the field as sent.
func (o *optional[T]) UnmarshalJSON(b []byte) error {
	o.set = true
	o.raw = append(o.raw[:0], b...)
	o.null = bytes.Equal(bytes.TrimSpace(b), []byte("null"))
	return nil
}

// resolve reads the value the request sent, or says that it is of the wrong type.
func (o *optional[T]) resolve(field string) *apperr.FieldError {
	if !o.set || o.null {
		return nil
	}
	if err := json.Unmarshal(o.raw, &o.value); err != nil {
		var kind string
		switch any(o.value).(type) {
		case string:
			kind = "teks"
		default:
			kind = "angka"
		}
		return &apperr.FieldError{Field: field, Code: "INVALID", Message: "Harus bertipe " + kind + "."}
	}
	return nil
}

// ptr is the value of a field the request set, or nil for null and for a field left out.
func (o *optional[T]) ptr() *T {
	if !o.set || o.null {
		return nil
	}
	v := o.value
	return &v
}

// ListFilter narrows the admin list.
type ListFilter struct {
	ProjectCode string
	TeamCode    string
	Type        string
	Active      *bool
	Q           string
	Limit       int
	Offset      int
}

func assertRegion(scope access.AdminScope, projectCode string) error {
	if !scope.AllowsRegion(access.RegionOfCode(projectCode)) {
		return access.ErrOutsideAdminScope
	}
	return nil
}

// ListAdmin returns one page of the zones the admin's region holds, with how many checkpoints each
// has, and the total.
func (s *Service) ListAdmin(ctx context.Context, actor auth.Principal, scope access.AdminScope, f ListFilter) ([]Zone, int, error) {
	if f.ProjectCode != "" {
		if err := assertRegion(scope, f.ProjectCode); err != nil {
			return nil, 0, err
		}
	}
	var out []Zone
	var total int64
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		region := pgtype.Text{String: scope.RegionCode, Valid: !scope.All}
		typ := db.NullPatrolZoneType{PatrolZoneType: db.PatrolZoneType(f.Type), Valid: f.Type != ""}
		rows, err := q.ListPatrolZones(ctx, db.ListPatrolZonesParams{
			ScopeRegion: region, ProjectCode: text(f.ProjectCode), TeamCode: text(f.TeamCode), Type: typ,
			Active: boolOrNull(f.Active), Q: text(f.Q),
			PageLimit: int32(f.Limit), PageOffset: int32(f.Offset), //nolint:gosec // bounded by ParseListQuery
		})
		if err != nil {
			return fmt.Errorf("list zones: %w", err)
		}
		total, err = q.CountPatrolZones(ctx, db.CountPatrolZonesParams{
			ScopeRegion: region, ProjectCode: text(f.ProjectCode), TeamCode: text(f.TeamCode), Type: typ,
			Active: boolOrNull(f.Active), Q: text(f.Q),
		})
		if err != nil {
			return fmt.Errorf("count zones: %w", err)
		}
		out, err = withCounts(ctx, q, rows)
		return err
	})
	return out, int(total), err
}

// GetAdmin returns one zone with its checkpoints, in order.
func (s *Service) GetAdmin(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64) (Zone, error) {
	var out Zone
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetPatrolZone(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrZoneNotFound
		}
		if err != nil {
			return fmt.Errorf("get zone: %w", err)
		}
		if err := assertRegion(scope, row.ProjectCode); err != nil {
			return err
		}
		out, err = withCheckpoints(ctx, q, row)
		return err
	})
	return out, err
}

// Create adds a zone, with its checkpoints when they are sent, and gives it a QR secret.
func (s *Service) Create(ctx context.Context, actor auth.Principal, scope access.AdminScope, in ZoneInput) (Zone, error) {
	if err := assertRegion(scope, in.ProjectCode); err != nil {
		return Zone{}, err
	}
	geo, err := geofence(in.Latitude, in.Longitude, in.RadiusMeters, false)
	if err != nil {
		return Zone{}, err
	}
	ids, err := parseCheckpointIDs(in.CheckpointIDs)
	if err != nil {
		return Zone{}, err
	}
	team := nonBlank(in.TeamCode)
	minVisit := 1
	if in.MinVisit != nil {
		minVisit = *in.MinVisit
	}

	var out Zone
	err = database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		if err := checkProjectAndTeam(ctx, q, in.ProjectCode, team); err != nil {
			return err
		}
		row, err := q.InsertPatrolZone(ctx, db.InsertPatrolZoneParams{
			ProjectCode: in.ProjectCode, TeamCode: textOrNull(team), Type: db.PatrolZoneType(in.Type), Name: strings.TrimSpace(in.Name),
			Alias: textOrNull(in.Alias), Description: textOrNull(in.Description), PopupText: textOrNull(in.PopupText),
			Latitude: geo.lat, Longitude: geo.lng, RadiusMeters: intParam(geo.radius), MinVisit: int32(minVisit), //nolint:gosec // 1..100
			StartDate: dateParam(in.StartDate), Active: in.Active == nil || *in.Active, Now: ts(s.clock.Now()),
		})
		if database.IsUniqueViolation(err) && database.ConstraintName(err) == "patrol_zones_name_uq" {
			return ErrZoneNameTaken
		}
		if err != nil {
			return fmt.Errorf("insert zone: %w", err)
		}
		// A zone nobody can see yet has no deletion to wait for, so locking its checkpoints after
		// inserting it keeps the order of locks (checkpoints first) for everyone else.
		found, err := lockCheckpoints(ctx, q, ids)
		if err != nil {
			return err
		}
		if err := linkCheckpoints(ctx, q, row, ids, found); err != nil {
			return err
		}
		out, err = withCheckpoints(ctx, q, row)
		if err != nil {
			return err
		}
		return auditZone(ctx, q, actor, "patrol.zone.create", out, map[string]any{"after": zoneMeta(out)})
	})
	return out, err
}

// Update changes a zone. The zone row is locked for the whole change, so two admins editing it
// take turns.
func (s *Service) Update(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64, in ZoneUpdate) (Zone, error) {
	var problems []*apperr.FieldError
	problems = append(problems,
		in.TeamCode.resolve("team_code"), in.Alias.resolve("alias"), in.Description.resolve("description"),
		in.PopupText.resolve("popup_text"), in.Latitude.resolve("latitude"), in.Longitude.resolve("longitude"),
		in.RadiusMeters.resolve("radius_meters"), in.StartDate.resolve("start_date"))
	var startDate *timeutil.Date
	if p := in.StartDate.ptr(); p != nil {
		d, err := timeutil.ParseDate(*p)
		if err != nil {
			problems = append(problems, &apperr.FieldError{Field: "start_date", Code: "INVALID", Message: "Harus berformat YYYY-MM-DD."})
		} else {
			startDate = &d
		}
	}
	if fields := collect(problems); len(fields) > 0 {
		return Zone{}, apperr.ErrValidation.WithFields(fields...)
	}
	var ids []pgtype.UUID
	if in.CheckpointIDs != nil {
		var err error
		if ids, err = parseCheckpointIDs(*in.CheckpointIDs); err != nil {
			return Zone{}, err
		}
	}

	var out Zone
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		// ⚠ Checkpoints are locked before the zone, as everywhere (see LockCheckpointsByIDs). Locking
		// only reads nothing a caller could learn: the zone's own checks come right after.
		var found map[string]db.PatrolCheckpoint
		if in.CheckpointIDs != nil {
			var err error
			if found, err = lockCheckpoints(ctx, q, ids); err != nil {
				return err
			}
		}
		row, err := q.GetPatrolZoneForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrZoneNotFound
		}
		if err != nil {
			return fmt.Errorf("get zone: %w", err)
		}
		if err := assertRegion(scope, row.ProjectCode); err != nil {
			return err
		}
		before, err := withCheckpoints(ctx, q, row)
		if err != nil {
			return err
		}

		next := db.UpdatePatrolZoneParams{
			ID: id, TeamCode: row.TeamCode, Type: row.Type, Name: row.Name, Alias: row.Alias, Description: row.Description,
			PopupText: row.PopupText, Latitude: row.Latitude, Longitude: row.Longitude, RadiusMeters: row.RadiusMeters,
			MinVisit: row.MinVisit, StartDate: row.StartDate, Active: row.Active,
			Now: ts(s.clock.Now()),
		}
		if in.Type != nil {
			next.Type = db.PatrolZoneType(*in.Type)
		}
		if in.Name != nil {
			next.Name = strings.TrimSpace(*in.Name)
		}
		if in.Alias.set {
			next.Alias = textOrNull(in.Alias.ptr())
		}
		if in.Description.set {
			next.Description = textOrNull(in.Description.ptr())
		}
		if in.PopupText.set {
			next.PopupText = textOrNull(in.PopupText.ptr())
		}
		if in.MinVisit != nil {
			next.MinVisit = int32(*in.MinVisit) //nolint:gosec // 1..100
		}
		if in.StartDate.set {
			next.StartDate = dateParam(startDate)
		}
		if in.Active != nil {
			next.Active = *in.Active
		}
		if in.TeamCode.set {
			team := nonBlank(in.TeamCode.ptr())
			if err := checkProjectAndTeam(ctx, q, row.ProjectCode, team); err != nil {
				return err
			}
			next.TeamCode = textOrNull(team)
		}

		// The geofence is the point and the radius together. Start from what the zone has, apply what
		// was sent, and check the result as a whole.
		lat, lng, radius := row.Latitude, row.Longitude, intOf(row.RadiusMeters)
		if in.Latitude.set {
			lat = in.Latitude.ptr()
		}
		if in.Longitude.set {
			lng = in.Longitude.ptr()
		}
		if in.RadiusMeters.set {
			radius = in.RadiusMeters.ptr()
		} else if lat == nil && lng == nil {
			// Taking the point away takes the radius with it; the admin need not say so twice.
			radius = nil
		}
		geo, err := geofence(lat, lng, radius, in.RadiusMeters.set)
		if err != nil {
			return err
		}
		next.Latitude, next.Longitude, next.RadiusMeters = geo.lat, geo.lng, intParam(geo.radius)

		row, err = q.UpdatePatrolZone(ctx, next)
		if database.IsUniqueViolation(err) && database.ConstraintName(err) == "patrol_zones_name_uq" {
			return ErrZoneNameTaken
		}
		if err != nil {
			return fmt.Errorf("update zone: %w", err)
		}
		if in.CheckpointIDs != nil {
			if err := linkCheckpoints(ctx, q, row, ids, found); err != nil {
				return err
			}
			if err := releaseIfEmpty(ctx, q, id, s.clock.Now()); err != nil {
				return err
			}
		}
		out, err = withCheckpoints(ctx, q, row)
		if err != nil {
			return err
		}
		return auditZone(ctx, q, actor, "patrol.zone.update", out, map[string]any{"before": zoneMeta(before), "after": zoneMeta(out)})
	})
	return out, err
}

// AssignCheckpoints replaces the checkpoints of the zone and their order, and nothing else of the zone.
// ids are in the order the guard walks them: they become positions 1..n. An empty list takes them all out.
func (s *Service) AssignCheckpoints(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64, in CheckpointsReplace) (Zone, error) {
	ids, err := parseCheckpointIDs(in.CheckpointIDs)
	if err != nil {
		return Zone{}, err
	}
	var out Zone
	err = database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		// Checkpoints first, then the zone: the order every writer of the links uses (see LockCheckpointsByIDs).
		found, err := lockCheckpoints(ctx, q, ids)
		if err != nil {
			return err
		}
		row, err := q.GetPatrolZoneForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrZoneNotFound
		}
		if err != nil {
			return fmt.Errorf("get zone: %w", err)
		}
		if err := assertRegion(scope, row.ProjectCode); err != nil {
			return err
		}
		before, err := q.ListZoneLinks(ctx, id)
		if err != nil {
			return fmt.Errorf("list zone links: %w", err)
		}
		if err := linkCheckpoints(ctx, q, row, ids, found); err != nil {
			return err
		}
		if err := releaseIfEmpty(ctx, q, id, s.clock.Now()); err != nil {
			return err
		}
		// The checkpoints are part of what the zone looks like to the app, so their change moves updated_at.
		if err := q.TouchPatrolZone(ctx, db.TouchPatrolZoneParams{ID: id, Now: ts(s.clock.Now())}); err != nil {
			return fmt.Errorf("touch zone: %w", err)
		}
		row, err = q.GetPatrolZone(ctx, id)
		if err != nil {
			return fmt.Errorf("get zone: %w", err)
		}
		out, err = withCheckpoints(ctx, q, row)
		if err != nil {
			return err
		}
		return auditZone(ctx, q, actor, "patrol.zone.checkpoints", out, map[string]any{
			"before": len(before), "after": len(ids),
		})
	})
	return out, err
}

// Delete removes a zone: it stays in the table, inactive, because scans point at it.
func (s *Service) Delete(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64) error {
	return database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetPatrolZoneForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrZoneNotFound
		}
		if err != nil {
			return fmt.Errorf("get zone: %w", err)
		}
		if err := assertRegion(scope, row.ProjectCode); err != nil {
			return err
		}
		if _, err := q.SoftDeletePatrolZone(ctx, db.SoftDeletePatrolZoneParams{ID: id, Now: ts(s.clock.Now())}); err != nil {
			return fmt.Errorf("delete zone: %w", err)
		}
		// ⚠ A guard still checked in here could never check out of a zone that no longer loads, and one
		// open check-in blocks a check-in in every zone (ZONE_ALREADY_CHECKED_IN): close them now.
		if _, err := q.CloseZoneCheckinsOfZone(ctx, db.CloseZoneCheckinsOfZoneParams{ZoneID: id, At: ts(s.clock.Now())}); err != nil {
			return fmt.Errorf("close check-ins of deleted zone: %w", err)
		}
		zone := zoneOf(row)
		return auditZone(ctx, q, actor, "patrol.zone.delete", zone, map[string]any{"before": zoneMeta(zone)})
	})
}

// QR is a rendered QR code of a checkpoint, ready to be sent as a file.
type QR struct {
	Body        []byte
	ContentType string
	FileName    string
}

// FieldFilter narrows the zones a field user lists.
type FieldFilter struct {
	ProjectCode string
	TeamCode    string
	Type        string
	// WithCheckpoints (?include=checkpoints) fills each zone as its own read does: the bundle an app keeps
	// for offline scans (bundle.go).
	WithCheckpoints bool
}

// ListForField returns the active zones the caller can work: those of the projects they serve in
// today that are for the whole project or for one of their own regu, each with how many checkpoints
// it has. With WithCheckpoints each zone also has its checkpoints, as the zone's own read shows them.
//
// A project_code that is not one of the caller's is PERMISSION_DENIED, not an empty list: the
// caller asked about something they have no part in.
func (s *Service) ListForField(ctx context.Context, actor auth.Principal, f FieldFilter) ([]Zone, error) {
	var out []Zone
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		m, err := s.membershipsOf(ctx, q, actor.PersonnelID)
		if err != nil {
			return err
		}
		projects := m.projects()
		if f.ProjectCode != "" {
			if !slices.Contains(projects, f.ProjectCode) {
				return access.ErrPermissionDenied
			}
			projects = []string{f.ProjectCode}
		}
		rows, err := q.ListPatrolZonesForField(ctx, db.ListPatrolZonesForFieldParams{
			ProjectCodes: projects, OwnTeamCodes: m.teams(), TeamCode: text(f.TeamCode),
			Type: db.NullPatrolZoneType{PatrolZoneType: db.PatrolZoneType(f.Type), Valid: f.Type != ""},
		})
		if err != nil {
			return fmt.Errorf("list zones for field: %w", err)
		}
		out, err = withCounts(ctx, q, rows)
		if err != nil || !f.WithCheckpoints {
			return err
		}
		out, err = s.withFieldBundle(ctx, q, actor, rows, out)
		return err
	})
	return out, err
}

// GetForField returns one zone to a person who serves in its project (and regu, when the zone has
// one), with its active checkpoints, their checklists, what the caller has done today and which
// checkpoint comes next. An inactive zone is still readable, so an app that holds it can see it was
// switched off.
func (s *Service) GetForField(ctx context.Context, actor auth.Principal, id int64) (Zone, error) {
	var out Zone
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetPatrolZone(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrZoneNotFound
		}
		if err != nil {
			return fmt.Errorf("get zone: %w", err)
		}
		m, err := s.membershipsOf(ctx, q, actor.PersonnelID)
		if err != nil {
			return err
		}
		// ⚠ Another organization's zone is not here at all (row-level security), so it answered 404
		// above. A zone of this organization the caller has no part in is 403: the plan asks for the
		// difference, and an id says nothing a project member does not already know.
		if !slices.Contains(m.projects(), row.ProjectCode) || (row.TeamCode.Valid && !slices.Contains(m.teams(), row.TeamCode.String)) {
			return access.ErrPermissionDenied
		}
		out, err = s.withFieldCheckpoints(ctx, q, actor, row)
		return err
	})
	return out, err
}

// memberships are the regu a person serves in today, each in its own project's timezone.
type memberships []db.ListCurrentMembershipsOfPersonnelRow

func (s *Service) membershipsOf(ctx context.Context, q *db.Queries, personnelID int64) (memberships, error) {
	rows, err := q.ListCurrentMembershipsOfPersonnel(ctx, db.ListCurrentMembershipsOfPersonnelParams{
		PersonnelID: personnelID, Now: ts(s.clock.Now()),
	})
	if err != nil {
		return nil, fmt.Errorf("list memberships: %w", err)
	}
	return rows, nil
}

func (m memberships) projects() []string {
	out := make([]string, 0, len(m))
	for _, r := range m {
		out = append(out, r.ProjectCode)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func (m memberships) teams() []string {
	out := make([]string, 0, len(m))
	for _, r := range m {
		out = append(out, r.TeamCode)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// checkProjectAndTeam makes sure the project exists and the regu, when there is one, exists and
// belongs to it.
func checkProjectAndTeam(ctx context.Context, q *db.Queries, projectCode string, teamCode *string) error {
	if _, err := q.GetProject(ctx, projectCode); errors.Is(err, pgx.ErrNoRows) {
		return organization.ErrProjectNotFound
	} else if err != nil {
		return fmt.Errorf("get project: %w", err)
	}
	if teamCode == nil {
		return nil
	}
	team, err := q.GetTeam(ctx, *teamCode)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && team.ProjectCode != projectCode) {
		return apperr.ErrValidation.WithFields(apperr.FieldError{
			Field: "team_code", Code: "INVALID", Message: "Regu tidak ditemukan di proyek ini.",
		})
	}
	if err != nil {
		return fmt.Errorf("get team: %w", err)
	}
	return nil
}

type fence struct {
	lat, lng *float64
	radius   *int
}

// geofence checks the point and the radius as one thing: all three, or none. A point without a
// radius gets DefaultRadiusMeters, unless the caller said the radius should be empty
// (radiusCleared), which then conflicts with the point.
func geofence(lat, lng *float64, radius *int, radiusCleared bool) (fence, error) {
	return geofenceWith(lat, lng, radius, radiusCleared, zoneFence)
}

// fenceRules are what differs between the geofence of a zone and of a checkpoint: the radius a
// point gets when none is sent, the largest radius, and what the message calls the place.
type fenceRules struct {
	defaultRadius int
	maxRadius     int
	subject       string
}

var zoneFence = fenceRules{defaultRadius: DefaultRadiusMeters, maxRadius: MaxRadiusMeters, subject: "Zona"}

// geofenceWith is geofence with the rules of the place it is for.
func geofenceWith(lat, lng *float64, radius *int, radiusCleared bool, rules fenceRules) (fence, error) {
	var problems []apperr.FieldError
	fail := func(field, msg string) {
		problems = append(problems, apperr.FieldError{Field: field, Code: "INVALID", Message: msg})
	}
	switch {
	case (lat == nil) != (lng == nil):
		fail("latitude", "Latitude dan longitude harus diisi bersama.")
	case lat == nil && radius != nil:
		fail("radius_meters", "Radius butuh titik (latitude dan longitude).")
	case lat != nil && radius == nil && radiusCleared:
		fail("radius_meters", rules.subject+" dengan titik butuh radius.")
	case lat != nil && radius == nil:
		r := rules.defaultRadius
		radius = &r
	}
	// The same limits the create body declares: a PATCH has no validate tags (a field may be left
	// out or null), so without these a latitude of 95 or a radius of 0 would reach the database's
	// CHECK and come back as a 500, and a radius past int32 would wrap.
	if lat != nil && (*lat < -90 || *lat > 90) {
		fail("latitude", "Harus antara -90 dan 90.")
	}
	if lng != nil && (*lng < -180 || *lng > 180) {
		fail("longitude", "Harus antara -180 dan 180.")
	}
	if radius != nil && (*radius < 1 || *radius > rules.maxRadius) {
		fail("radius_meters", fmt.Sprintf("Harus antara 1 dan %d.", rules.maxRadius))
	}
	if len(problems) > 0 {
		return fence{}, apperr.ErrValidation.WithFields(problems...)
	}
	return fence{lat: lat, lng: lng, radius: radius}, nil
}

// withCounts is a zone row as a list shows it: with how many checkpoints it has, from one query for all.
func withCounts(ctx context.Context, q *db.Queries, rows []db.PatrolZone) ([]Zone, error) {
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	counts, err := q.CountCheckpointsOfZones(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("count checkpoints of zones: %w", err)
	}
	byZone := make(map[int64]int, len(counts))
	for _, c := range counts {
		byZone[c.ZoneID] = int(c.Checkpoints)
	}
	out := make([]Zone, 0, len(rows))
	for _, r := range rows {
		z := zoneOf(r)
		z.CheckpointCount = byZone[r.ID]
		out = append(out, z)
	}
	return out, nil
}

// withCheckpoints is a zone row as the admin's detail shows it: its checkpoints in order, the inactive
// ones too, without their checklists.
func withCheckpoints(ctx context.Context, q *db.Queries, row db.PatrolZone) (Zone, error) {
	rows, err := q.ListZoneCheckpointsAdmin(ctx, row.ID)
	if err != nil {
		return Zone{}, fmt.Errorf("list zone checkpoints: %w", err)
	}
	z := zoneOf(row)
	z.Checkpoints = make([]ZoneCheckpoint, 0, len(rows))
	for _, r := range rows {
		cp := zoneCheckpointOf(adminLinkRow(r), r.Position)
		z.Checkpoints = append(z.Checkpoints, cp)
		if cp.Active {
			z.CheckpointCount++
		}
	}
	return z, nil
}

// checkpointRowOf is the checkpoint columns of a zone-link row.
func adminLinkRow(r db.ListZoneCheckpointsAdminRow) db.PatrolCheckpoint {
	return db.PatrolCheckpoint{
		ID: r.ID, OrganizationID: r.OrganizationID, ProjectCode: r.ProjectCode, Name: r.Name, Description: r.Description,
		Latitude: r.Latitude, Longitude: r.Longitude, RadiusMeters: r.RadiusMeters, QrSecret: r.QrSecret,
		QrRotatedAt: r.QrRotatedAt, Active: r.Active, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, DeletedAt: r.DeletedAt,
	}
}

// withFieldCheckpoints is a zone row as the field's detail shows it.
//
// The caller's own progress is read for the work date their next scan would get: the date of their
// shift, or the business date in the project's timezone when they have none (the same rule the scan
// uses, so a night-shift guard after midnight reads yesterday's walk). my_scans, scannable and
// next_checkpoint_id all come from loadOrder, the function the scan checks with, so the app and the
// server cannot disagree about what is next.
func (s *Service) withFieldCheckpoints(ctx context.Context, q *db.Queries, actor auth.Principal, row db.PatrolZone) (Zone, error) {
	z := zoneOf(row)
	z.Checkpoints = []ZoneCheckpoint{}
	rows, err := q.ListZoneCheckpoints(ctx, row.ID)
	if err != nil {
		return Zone{}, fmt.Errorf("list zone checkpoints: %w", err)
	}
	z.CheckpointCount = len(rows)
	if len(rows) == 0 {
		return z, nil
	}

	project, err := q.GetProject(ctx, row.ProjectCode)
	if err != nil {
		return Zone{}, fmt.Errorf("get project: %w", err)
	}
	loc, err := time.LoadLocation(project.Timezone)
	if err != nil {
		return Zone{}, fmt.Errorf("project %s: timezone %q: %w", project.Code, project.Timezone, err)
	}
	where, err := s.placeIn(ctx, q, actor, row.ProjectCode, row.TeamCode, loc, s.clock.Now())
	if err != nil {
		return Zone{}, err
	}
	order, err := loadOrder(ctx, q, actor.PersonnelID, row.ID, where.workDate, endOfTime)
	if err != nil {
		return Zone{}, err
	}
	links := make([]linkedCheckpoint, 0, len(rows))
	ids := make([]pgtype.UUID, 0, len(rows))
	for _, r := range rows {
		links = append(links, linkedCheckpoint{cp: activeLinkRow(r), position: r.Position})
		ids = append(ids, r.ID)
	}
	// One query for the checklists of every checkpoint of the zone, not one per checkpoint.
	actionsOf, err := actionsByCheckpoint(ctx, q, ids)
	if err != nil {
		return Zone{}, err
	}
	return fieldZoneOf(z, links, order, actionsOf), nil
}

// linkedCheckpoint is an active checkpoint of a zone with its position there.
type linkedCheckpoint struct {
	cp       db.PatrolCheckpoint
	position int32
}

// actionsByCheckpoint reads the active actions of many checkpoints in one query, grouped by checkpoint id.
func actionsByCheckpoint(ctx context.Context, q *db.Queries, ids []pgtype.UUID) (map[string][]CheckpointAction, error) {
	actions, err := q.ListActionsOfCheckpoints(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("list checkpoint actions: %w", err)
	}
	out := make(map[string][]CheckpointAction, len(ids))
	for _, a := range actions {
		key := uuidString(a.CheckpointID)
		out[key] = append(out[key], actionOf(a))
	}
	return out, nil
}

// fieldZoneOf puts a zone's active checkpoints on it as the field read shows them: each with its checklist and
// what the caller has done (my_scans, scannable), and the zone with next_checkpoint_id. The one place that
// shapes it, for the detail of one zone and the bundled list of all, so the two read the same.
func fieldZoneOf(z Zone, links []linkedCheckpoint, order zoneOrder, actionsOf map[string][]CheckpointAction) Zone {
	stepOf := make(map[string]orderStep, len(order.Steps))
	for _, st := range order.Steps {
		stepOf[uuidString(st.CheckpointID)] = st
	}
	z.Checkpoints = make([]ZoneCheckpoint, 0, len(links))
	z.CheckpointCount = len(links)
	for _, l := range links {
		cp := zoneCheckpointOf(l.cp, l.position)
		st := stepOf[cp.ID]
		scans, scannable := st.Scans, st.Scannable
		cp.MyScans, cp.Scannable = &scans, &scannable
		cp.Actions = actionsOf[cp.ID]
		if cp.Actions == nil {
			cp.Actions = []CheckpointAction{}
		}
		z.Checkpoints = append(z.Checkpoints, cp)
	}
	if order.Next >= 0 && order.Next < len(order.Steps) {
		next := uuidString(order.Steps[order.Next].CheckpointID)
		z.NextCheckpointID = &next
	}
	return z
}

// activeLinkRow is the checkpoint columns of an active zone-link row.
func activeLinkRow(r db.ListZoneCheckpointsRow) db.PatrolCheckpoint {
	return db.PatrolCheckpoint{
		ID: r.ID, OrganizationID: r.OrganizationID, ProjectCode: r.ProjectCode, Name: r.Name, Description: r.Description,
		Latitude: r.Latitude, Longitude: r.Longitude, RadiusMeters: r.RadiusMeters, QrSecret: r.QrSecret,
		QrRotatedAt: r.QrRotatedAt, Active: r.Active, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, DeletedAt: r.DeletedAt,
	}
}

func auditZone(ctx context.Context, q *db.Queries, actor auth.Principal, action string, z Zone, meta map[string]any) error {
	actorID := actor.PersonnelID
	if meta == nil {
		meta = map[string]any{}
	}
	meta["project_code"] = z.ProjectCode
	return audit.Record(ctx, q, audit.Entry{
		ActorPersonnelID: &actorID, Action: action, EntityType: "patrol_zone", EntityID: z.ID, Metadata: meta,
	})
}

// zoneMeta is what the audit log keeps of a zone: its settings and how many checkpoints it has.
func zoneMeta(z Zone) map[string]any {
	return map[string]any{
		"type": z.Type, "name": z.Name, "team_code": z.TeamCode, "latitude": z.Latitude, "longitude": z.Longitude,
		"radius_meters": z.RadiusMeters, "min_visit": z.MinVisit,
		"active": z.Active, "checkpoints": z.CheckpointCount,
	}
}

func collect(problems []*apperr.FieldError) []apperr.FieldError {
	var out []apperr.FieldError
	for _, p := range problems {
		if p != nil {
			out = append(out, *p)
		}
	}
	return out
}

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func boolOrNull(b *bool) pgtype.Bool {
	if b == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *b, Valid: true}
}

func intOf(v pgtype.Int4) *int {
	if !v.Valid {
		return nil
	}
	i := int(v.Int32)
	return &i
}
