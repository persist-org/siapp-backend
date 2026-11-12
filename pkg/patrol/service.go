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
	shifts   organization.Shifts
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
func (s *Service) UseShifts(sh organization.Shifts) { s.shifts = sh }

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
	ProjectCode   string               `json:"project_code" validate:"required,notblank,max=100"`
	TeamCode      *string              `json:"team_code" validate:"omitempty,max=100"`
	Type          string               `json:"type" validate:"required,oneof=PATROL GUARD"`
	Name          string               `json:"name" validate:"required,notblank,max=200"`
	Alias         *string              `json:"alias" validate:"omitempty,max=200"`
	Description   *string              `json:"description" validate:"omitempty,max=2000"`
	PopupText     *string              `json:"popup_text" validate:"omitempty,max=2000"`
	Latitude      *float64             `json:"latitude" validate:"omitempty,gte=-90,lte=90"`
	Longitude     *float64             `json:"longitude" validate:"omitempty,gte=-180,lte=180"`
	RadiusMeters  *int                 `json:"radius_meters" validate:"omitempty,min=1,max=5000"`
	MinVisit      *int                 `json:"min_visit" validate:"omitempty,min=1,max=100"`
	StartDate     *timeutil.Date       `json:"start_date"`
	PhotoRequired *bool                `json:"photo_required"`
	Active        *bool                `json:"is_active"`
	Checklist     []ChecklistItemInput `json:"checklist" validate:"omitempty,max=100,dive"`
}

// ZoneUpdate is a request to change a zone. A field that is left out keeps its value; null
// empties the ones that may be empty. The project is not here: a zone does not move, because
// the scans it holds belong to that project's people.
type ZoneUpdate struct {
	TeamCode      optional[string]      `json:"team_code"`
	Type          *string               `json:"type" validate:"omitempty,oneof=PATROL GUARD"`
	Name          *string               `json:"name" validate:"omitempty,notblank,max=200"`
	Alias         optional[string]      `json:"alias"`
	Description   optional[string]      `json:"description"`
	PopupText     optional[string]      `json:"popup_text"`
	Latitude      optional[float64]     `json:"latitude"`
	Longitude     optional[float64]     `json:"longitude"`
	RadiusMeters  optional[int]         `json:"radius_meters"`
	MinVisit      *int                  `json:"min_visit" validate:"omitempty,min=1,max=100"`
	StartDate     optional[string]      `json:"start_date"`
	PhotoRequired *bool                 `json:"photo_required"`
	Active        *bool                 `json:"is_active"`
	Checklist     *[]ChecklistItemInput `json:"checklist" validate:"omitempty,max=100,dive"`
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

// ListAdmin returns one page of the zones the admin's region holds, with their checklists, and
// the total.
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
		out, err = withChecklists(ctx, q, rows)
		return err
	})
	return out, int(total), err
}

// GetAdmin returns one zone with its checklist.
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
		out, err = withChecklist(ctx, q, row)
		return err
	})
	return out, err
}

// Create adds a zone, with its checklist, and gives it a QR secret.
func (s *Service) Create(ctx context.Context, actor auth.Principal, scope access.AdminScope, in ZoneInput) (Zone, error) {
	if err := assertRegion(scope, in.ProjectCode); err != nil {
		return Zone{}, err
	}
	geo, err := geofence(in.Latitude, in.Longitude, in.RadiusMeters, false)
	if err != nil {
		return Zone{}, err
	}
	specs, err := parseChecklist(in.Checklist)
	if err != nil {
		return Zone{}, err
	}
	secret, err := newSecret()
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
			StartDate: dateParam(in.StartDate), PhotoRequired: in.PhotoRequired == nil || *in.PhotoRequired,
			QrSecret: secret, Active: in.Active == nil || *in.Active, Now: ts(s.clock.Now()),
		})
		if err != nil {
			return fmt.Errorf("insert zone: %w", err)
		}
		if _, _, _, err := replaceChecklist(ctx, q, s.clock, row.ID, specs); err != nil {
			return err
		}
		out, err = withChecklist(ctx, q, row)
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
	var specs []itemSpec
	if in.Checklist != nil {
		var err error
		if specs, err = parseChecklist(*in.Checklist); err != nil {
			return Zone{}, err
		}
	}

	var out Zone
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
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
		before, err := withChecklist(ctx, q, row)
		if err != nil {
			return err
		}

		next := db.UpdatePatrolZoneParams{
			ID: id, TeamCode: row.TeamCode, Type: row.Type, Name: row.Name, Alias: row.Alias, Description: row.Description,
			PopupText: row.PopupText, Latitude: row.Latitude, Longitude: row.Longitude, RadiusMeters: row.RadiusMeters,
			MinVisit: row.MinVisit, StartDate: row.StartDate, PhotoRequired: row.PhotoRequired, Active: row.Active,
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
		if in.PhotoRequired != nil {
			next.PhotoRequired = *in.PhotoRequired
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
		if err != nil {
			return fmt.Errorf("update zone: %w", err)
		}
		if in.Checklist != nil {
			if _, _, _, err := replaceChecklist(ctx, q, s.clock, id, specs); err != nil {
				return err
			}
		}
		out, err = withChecklist(ctx, q, row)
		if err != nil {
			return err
		}
		return auditZone(ctx, q, actor, "patrol.zone.update", out, map[string]any{"before": zoneMeta(before), "after": zoneMeta(out)})
	})
	return out, err
}

// ReplaceChecklist replaces the checklist and nothing else of the zone.
func (s *Service) ReplaceChecklist(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64, in ChecklistReplace) (Zone, error) {
	specs, err := parseChecklist(in.Checklist)
	if err != nil {
		return Zone{}, err
	}
	var out Zone
	err = database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
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
		created, updated, deactivated, err := replaceChecklist(ctx, q, s.clock, id, specs)
		if err != nil {
			return err
		}
		// The checklist is part of what the zone looks like to the app, so its change moves
		// updated_at.
		if err := q.TouchPatrolZone(ctx, db.TouchPatrolZoneParams{ID: id, Now: ts(s.clock.Now())}); err != nil {
			return fmt.Errorf("touch zone: %w", err)
		}
		row, err = q.GetPatrolZone(ctx, id)
		if err != nil {
			return fmt.Errorf("get zone: %w", err)
		}
		out, err = withChecklist(ctx, q, row)
		if err != nil {
			return err
		}
		return auditZone(ctx, q, actor, "patrol.zone.checklist", out, map[string]any{
			"created": created, "updated": updated, "deactivated": deactivated,
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
		zone, err := zoneOf(row, nil)
		if err != nil {
			return err
		}
		return auditZone(ctx, q, actor, "patrol.zone.delete", zone, map[string]any{"before": zoneMeta(zone)})
	})
}

// RotateQR gives the zone a new QR secret, so every sticker printed with the old one stops
// working.
func (s *Service) RotateQR(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64) (Zone, error) {
	secret, err := newSecret()
	if err != nil {
		return Zone{}, err
	}
	var out Zone
	err = database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
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
		row, err = q.RotatePatrolZoneSecret(ctx, db.RotatePatrolZoneSecretParams{ID: id, QrSecret: secret, Now: ts(s.clock.Now())})
		if err != nil {
			return fmt.Errorf("rotate zone secret: %w", err)
		}
		out, err = withChecklist(ctx, q, row)
		if err != nil {
			return err
		}
		// The secret itself is never written to the audit log.
		return auditZone(ctx, q, actor, "patrol.zone.rotate_qr", out, nil)
	})
	return out, err
}

// QR is a rendered QR code of a zone.
type QR struct {
	Body        []byte
	ContentType string
	FileName    string
}

// QRImage renders the zone's QR code, in the format (FormatPNG or FormatSVG).
func (s *Service) QRImage(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64, format string) (QR, error) {
	var out QR
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
		body, contentType, err := renderQR(Payload(actor.OrganizationID, row.ID, row.QrSecret), format)
		if err != nil {
			return err
		}
		out = QR{Body: body, ContentType: contentType, FileName: fileName(row.Name, format)}
		return nil
	})
	return out, err
}

// FieldFilter narrows the zones a field user lists.
type FieldFilter struct {
	ProjectCode string
	TeamCode    string
	Type        string
}

// ListForField returns the active zones the caller can work: those of the projects they serve in
// today that are for the whole project or for one of their own regu, with the checklists the app
// draws the form from.
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
		out, err = withChecklists(ctx, q, rows)
		return err
	})
	return out, err
}

// GetForField returns one zone to a person who serves in its project (and regu, when the zone has
// one). An inactive zone is still readable, so an app that holds it can see it was switched off.
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
		out, err = withChecklist(ctx, q, row)
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
		fail("radius_meters", "Zona dengan titik butuh radius.")
	case lat != nil && radius == nil:
		r := DefaultRadiusMeters
		radius = &r
	}
	if len(problems) > 0 {
		return fence{}, apperr.ErrValidation.WithFields(problems...)
	}
	return fence{lat: lat, lng: lng, radius: radius}, nil
}

func withChecklist(ctx context.Context, q *db.Queries, row db.PatrolZone) (Zone, error) {
	items, err := q.ListChecklistItems(ctx, row.ID)
	if err != nil {
		return Zone{}, fmt.Errorf("list checklist: %w", err)
	}
	return zoneOf(row, items)
}

// withChecklists reads the checklists of all the zones in one query.
func withChecklists(ctx context.Context, q *db.Queries, rows []db.PatrolZone) ([]Zone, error) {
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	items, err := q.ListChecklistItemsOfZones(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("list checklists: %w", err)
	}
	byZone := make(map[int64][]db.PatrolChecklistItem, len(rows))
	for _, it := range items {
		byZone[it.ZoneID] = append(byZone[it.ZoneID], it)
	}
	out := make([]Zone, 0, len(rows))
	for _, r := range rows {
		z, err := zoneOf(r, byZone[r.ID])
		if err != nil {
			return nil, err
		}
		out = append(out, z)
	}
	return out, nil
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

// zoneMeta is what the audit log keeps of a zone: its settings and how many questions it asks,
// not the questions themselves.
func zoneMeta(z Zone) map[string]any {
	return map[string]any{
		"type": z.Type, "name": z.Name, "team_code": z.TeamCode, "latitude": z.Latitude, "longitude": z.Longitude,
		"radius_meters": z.RadiusMeters, "min_visit": z.MinVisit, "photo_required": z.PhotoRequired,
		"active": z.Active, "checklist_items": len(z.Checklist),
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
