package organization

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/geo"
	"siapp/pkg/platform/timeutil"
)

// defaultAttendanceRadiusMeters is a project's geofence radius until an admin sets one.
const defaultAttendanceRadiusMeters = 150

// EmergencyContact is one phone number a guard may need at the site: Polisi, RS or Pemadam
// (DL-14, the project's own override of module 06's list).
type EmergencyContact struct {
	Type  string `json:"type" validate:"required,notblank,max=50"`
	Name  string `json:"name" validate:"required,notblank,max=200"`
	Phone string `json:"phone" validate:"required,notblank,max=50"`
}

// Project is one proyek. Its description is its name.
type Project struct {
	Code                     string
	ClientCode               string
	Description              string
	CompanyCode              *string
	CompanyPrefix            *string
	CompanyBidangUsaha       *string
	BusinessLine             string
	City                     *string
	Address                  *string
	Phone                    *string
	Email                    *string
	Timezone                 string
	Latitude                 *float64
	Longitude                *float64
	AttendanceRadiusMeters   int
	StrictAttendanceGeofence bool
	GPSTrackingEnabled       bool
	PatrolIntervalMinutes    *int
	EmergencyContacts        []EmergencyContact
	Active                   bool
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

// Configured reports whether the project's location is known. Without it a feature that
// needs the site (attendance, patrol) answers PROJECT_NOT_CONFIGURED.
func (p Project) Configured() bool { return p.Latitude != nil && p.Longitude != nil }

// ProjectInput is a request to create a project. The code is built from ClientCode and
// BusinessLine (D19), so the client never sends one.
type ProjectInput struct {
	ClientCode               string             `json:"client_code" validate:"required"`
	BusinessLine             string             `json:"business_line" validate:"required,oneof=security cleaning"`
	Description              string             `json:"description" validate:"required,notblank,max=200"`
	Timezone                 string             `json:"timezone" validate:"required"`
	CompanyCode              *string            `json:"company_code" validate:"omitempty,max=50"`
	CompanyPrefix            *string            `json:"company_prefix" validate:"omitempty,max=10"`
	CompanyBidangUsaha       *string            `json:"company_bidang_usaha" validate:"omitempty,max=200"`
	City                     *string            `json:"city" validate:"omitempty,max=100"`
	Address                  *string            `json:"address" validate:"omitempty,max=500"`
	Phone                    *string            `json:"phone" validate:"omitempty,max=50"`
	Email                    *string            `json:"email" validate:"omitempty,max=200"`
	Latitude                 *float64           `json:"latitude" validate:"omitempty,gte=-90,lte=90"`
	Longitude                *float64           `json:"longitude" validate:"omitempty,gte=-180,lte=180"`
	AttendanceRadiusMeters   *int               `json:"attendance_radius_meters" validate:"omitempty,gte=1,lte=10000"`
	StrictAttendanceGeofence *bool              `json:"strict_attendance_geofence"`
	GPSTrackingEnabled       *bool              `json:"gps_tracking_enabled"`
	PatrolIntervalMinutes    *int               `json:"patrol_interval_minutes" validate:"omitempty,gte=1,lte=1440"`
	EmergencyContacts        []EmergencyContact `json:"emergency_contacts" validate:"omitempty,dive"`
	Active                   *bool              `json:"is_active"`
}

// ProjectUpdate is a request to change a project. The code, the client and the business line
// have no field: the code is built from them, so a client that sends one gets 422 from the
// decoder's unknown-field rule. A value that is not sent is not changed, and one that was set
// can be replaced but not cleared.
type ProjectUpdate struct {
	Description              *string            `json:"description" validate:"omitempty,notblank,max=200"`
	Timezone                 *string            `json:"timezone"`
	CompanyCode              *string            `json:"company_code" validate:"omitempty,max=50"`
	CompanyBidangUsaha       *string            `json:"company_bidang_usaha" validate:"omitempty,max=200"`
	City                     *string            `json:"city" validate:"omitempty,max=100"`
	Address                  *string            `json:"address" validate:"omitempty,max=500"`
	Phone                    *string            `json:"phone" validate:"omitempty,max=50"`
	Email                    *string            `json:"email" validate:"omitempty,max=200"`
	Latitude                 *float64           `json:"latitude" validate:"omitempty,gte=-90,lte=90"`
	Longitude                *float64           `json:"longitude" validate:"omitempty,gte=-180,lte=180"`
	AttendanceRadiusMeters   *int               `json:"attendance_radius_meters" validate:"omitempty,gte=1,lte=10000"`
	StrictAttendanceGeofence *bool              `json:"strict_attendance_geofence"`
	GPSTrackingEnabled       *bool              `json:"gps_tracking_enabled"`
	PatrolIntervalMinutes    *int               `json:"patrol_interval_minutes" validate:"omitempty,gte=1,lte=1440"`
	EmergencyContacts        []EmergencyContact `json:"emergency_contacts" validate:"omitempty,dive"`
	Active                   *bool              `json:"is_active"`
}

// ProjectFilter narrows a list. Zero values match everything.
type ProjectFilter struct {
	Q            string
	ClientCode   string
	BusinessLine string
	Active       *bool
	Limit        int
	Offset       int
}

// Ancestors is a project with the client, branch and region above it, for reports.
type Ancestors struct {
	ClientCode        string
	ClientName        string
	BranchCode        string
	BranchName        string
	RegionCode        string
	RegionDescription string
}

// Projects manages the proyek of an organization, and answers other modules' questions about
// them (Lookup, Location, Ancestors).
type Projects struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

// NewProjects returns the Projects usecase.
func NewProjects(pool *pgxpool.Pool, c clock.Clock) *Projects {
	return &Projects{pool: pool, clock: c}
}

// List returns one page of the projects the caller may see, and their total. A REGION_ADMIN
// sees only the projects under their own region.
func (p *Projects) List(ctx context.Context, actor auth.Principal, scope access.AdminScope, f ProjectFilter) ([]Project, int, error) {
	var out []Project
	var total int64
	err := database.WithTenant(ctx, p.pool, actor.OrganizationID, func(q *db.Queries) error {
		if f.Offset > math.MaxInt32 || f.Limit > math.MaxInt32 {
			return nil
		}
		line := db.NullBusinessLine{BusinessLine: db.BusinessLine(f.BusinessLine), Valid: f.BusinessLine != ""}
		rows, err := q.ListProjects(ctx, db.ListProjectsParams{
			ScopeRegion: scopeFilter(scope), ClientCode: textOrNull(f.ClientCode), BusinessLine: line,
			Active: boolOrNull(f.Active), Q: textOrNull(f.Q),
			PageLimit: int32(f.Limit), PageOffset: int32(f.Offset), //nolint:gosec // bounded just above
		})
		if err != nil {
			return fmt.Errorf("list projects: %w", err)
		}
		for _, row := range rows {
			out = append(out, toProject(row))
		}
		total, err = q.CountProjects(ctx, db.CountProjectsParams{
			ScopeRegion: scopeFilter(scope), ClientCode: textOrNull(f.ClientCode), BusinessLine: line,
			Active: boolOrNull(f.Active), Q: textOrNull(f.Q),
		})
		if err != nil {
			return fmt.Errorf("count projects: %w", err)
		}
		return nil
	})
	return out, int(total), err
}

// Get returns one project. A REGION_ADMIN may read only one under their own region.
func (p *Projects) Get(ctx context.Context, actor auth.Principal, scope access.AdminScope, code string) (Project, error) {
	if !scope.AllowsRegion(RegionOfCode(code)) {
		return Project{}, access.ErrOutsideAdminScope
	}
	return p.Lookup(ctx, actor.OrganizationID, code)
}

// Create adds a project under a client. A REGION_ADMIN may create only under their own
// region. A client has at most one project per business line: a second is CODE_TAKEN.
func (p *Projects) Create(ctx context.Context, actor auth.Principal, scope access.AdminScope, in ProjectInput) (Project, error) {
	in.ClientCode = strings.TrimSpace(in.ClientCode)
	code, err := BuildProjectCode(in.ClientCode, in.BusinessLine)
	if err != nil {
		return Project{}, err
	}
	if err := validateTimezone(in.Timezone); err != nil {
		return Project{}, err
	}
	if err := validatePoint(in.Latitude, in.Longitude); err != nil {
		return Project{}, err
	}
	if err := validateCompanyPrefix(in.CompanyPrefix); err != nil {
		return Project{}, err
	}
	if !scope.AllowsRegion(RegionOfCode(in.ClientCode)) {
		return Project{}, access.ErrOutsideAdminScope
	}
	contacts, err := encodeContacts(in.EmergencyContacts)
	if err != nil {
		return Project{}, err
	}
	radius := int32(defaultAttendanceRadiusMeters)
	if in.AttendanceRadiusMeters != nil {
		radius = clampInt32(*in.AttendanceRadiusMeters)
	}

	var out Project
	err = database.WithTenant(ctx, p.pool, actor.OrganizationID, func(q *db.Queries) error {
		// The client is locked, so a delete of it cannot commit between this check and the insert.
		if _, err := q.GetClientForUpdate(ctx, in.ClientCode); errors.Is(err, pgx.ErrNoRows) {
			return ErrClientNotFound
		} else if err != nil {
			return fmt.Errorf("get client: %w", err)
		}
		row, err := q.CreateProject(ctx, db.CreateProjectParams{
			Code: code, ClientCode: in.ClientCode, Description: strings.TrimSpace(in.Description),
			CompanyCode: textPtr(in.CompanyCode), CompanyPrefix: textPtr(in.CompanyPrefix), CompanyBidangUsaha: textPtr(in.CompanyBidangUsaha),
			BusinessLine: db.BusinessLine(in.BusinessLine), City: textPtr(in.City), Address: textPtr(in.Address),
			Phone: textPtr(in.Phone), Email: textPtr(in.Email), Timezone: in.Timezone,
			Latitude: in.Latitude, Longitude: in.Longitude, AttendanceRadiusMeters: radius,
			StrictAttendanceGeofence: in.StrictAttendanceGeofence != nil && *in.StrictAttendanceGeofence,
			GpsTrackingEnabled:       in.GPSTrackingEnabled == nil || *in.GPSTrackingEnabled,
			PatrolIntervalMinutes:    int4OrNull(in.PatrolIntervalMinutes), EmergencyContacts: contacts,
			Active: in.Active == nil || *in.Active, Actor: personnelRef(actor), Now: ts(p.clock.Now()),
		})
		if err != nil {
			return err
		}
		out = toProject(row)
		return recordAudit(ctx, q, actor, "project.create", "project", code, nil, projectMeta(out))
	})
	if database.IsUniqueViolation(err) {
		return Project{}, ErrCodeTaken
	}
	if err != nil {
		return Project{}, fmt.Errorf("create project: %w", err)
	}
	return out, nil
}

// Update changes a project's details and settings. A REGION_ADMIN may change only one under
// their region.
func (p *Projects) Update(ctx context.Context, actor auth.Principal, scope access.AdminScope, code string, in ProjectUpdate) (Project, error) {
	if !scope.AllowsRegion(RegionOfCode(code)) {
		return Project{}, access.ErrOutsideAdminScope
	}
	if in.Timezone != nil {
		if err := validateTimezone(*in.Timezone); err != nil {
			return Project{}, err
		}
	}
	contacts, err := encodeContactsPatch(in.EmergencyContacts)
	if err != nil {
		return Project{}, err
	}
	var out Project
	err = database.WithTenant(ctx, p.pool, actor.OrganizationID, func(q *db.Queries) error {
		before, err := q.GetProjectForUpdate(ctx, code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrProjectNotFound
		}
		if err != nil {
			return fmt.Errorf("get project: %w", err)
		}
		// One coordinate alone would be half a place: the pair has to be whole after the
		// change (the table's CHECK says the same, but answers 500).
		lat, lng := pickCoordinate(in.Latitude, before.Latitude), pickCoordinate(in.Longitude, before.Longitude)
		if err := validatePoint(lat, lng); err != nil {
			return err
		}
		row, err := q.UpdateProject(ctx, db.UpdateProjectParams{
			Code: code, Description: textPtr(trimmed(in.Description)), CompanyCode: textPtr(in.CompanyCode),
			CompanyBidangUsaha: textPtr(in.CompanyBidangUsaha), City: textPtr(in.City), Address: textPtr(in.Address),
			Phone: textPtr(in.Phone), Email: textPtr(in.Email), Timezone: textPtr(in.Timezone),
			Latitude: in.Latitude, Longitude: in.Longitude, AttendanceRadiusMeters: int4OrNull(in.AttendanceRadiusMeters),
			StrictAttendanceGeofence: boolOrNull(in.StrictAttendanceGeofence), GpsTrackingEnabled: boolOrNull(in.GPSTrackingEnabled),
			PatrolIntervalMinutes: int4OrNull(in.PatrolIntervalMinutes), EmergencyContacts: contacts,
			Active: boolOrNull(in.Active), Actor: personnelRef(actor), Now: ts(p.clock.Now()),
		})
		if err != nil {
			return fmt.Errorf("update project: %w", err)
		}
		out = toProject(row)
		return recordAudit(ctx, q, actor, "project.update", "project", code, projectMeta(toProject(before)), projectMeta(out))
	})
	return out, err
}

// Delete removes a project (a soft delete: the row and its code stay, D19). A REGION_ADMIN
// may delete only one under their region. A project with an active team is
// HAS_ACTIVE_CHILDREN.
func (p *Projects) Delete(ctx context.Context, actor auth.Principal, scope access.AdminScope, code string) error {
	if !scope.AllowsRegion(RegionOfCode(code)) {
		return access.ErrOutsideAdminScope
	}
	return database.WithTenant(ctx, p.pool, actor.OrganizationID, func(q *db.Queries) error {
		before, err := q.GetProjectForUpdate(ctx, code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrProjectNotFound
		}
		if err != nil {
			return fmt.Errorf("get project: %w", err)
		}
		teams, err := q.CountTeamsOfProject(ctx, code)
		if err != nil {
			return fmt.Errorf("count teams of project: %w", err)
		}
		if teams > 0 {
			return ErrHasActiveChildren.WithMessage("Masih ada regu (aktif maupun nonaktif) di proyek ini.")
		}
		if _, err := q.SoftDeleteProject(ctx, db.SoftDeleteProjectParams{Code: code, Actor: personnelRef(actor), Now: ts(p.clock.Now())}); err != nil {
			return fmt.Errorf("delete project: %w", err)
		}
		return recordAudit(ctx, q, actor, "project.delete", "project", code, projectMeta(toProject(before)), nil)
	})
}

// Lookup returns a project of an organization by its code, for other modules (admin reads go
// through Get, which also applies the admin's scope). organizationID is the caller's (from
// auth.From), never a request value.
func (p *Projects) Lookup(ctx context.Context, organizationID int64, code string) (Project, error) {
	var out Project
	err := database.WithTenant(ctx, p.pool, organizationID, func(q *db.Queries) error {
		row, err := q.GetProject(ctx, code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrProjectNotFound
		}
		if err != nil {
			return fmt.Errorf("get project: %w", err)
		}
		out = toProject(row)
		return nil
	})
	return out, err
}

// Location returns where a project is: its timezone, its GPS point and the attendance radius
// in meters. A project whose coordinates are not entered is PROJECT_NOT_CONFIGURED, so a
// guard cannot check in at a site whose location is unknown.
func (p *Projects) Location(ctx context.Context, organizationID int64, code string) (*time.Location, geo.Point, int, error) {
	project, err := p.Lookup(ctx, organizationID, code)
	if err != nil {
		return nil, geo.Point{}, 0, err
	}
	return project.location()
}

func (p Project) location() (*time.Location, geo.Point, int, error) {
	if !p.Configured() {
		return nil, geo.Point{}, 0, ErrProjectNotConfigured
	}
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return nil, geo.Point{}, 0, fmt.Errorf("project %s: load timezone %q: %w", p.Code, p.Timezone, err)
	}
	return loc, geo.Point{Lat: *p.Latitude, Lng: *p.Longitude}, p.AttendanceRadiusMeters, nil
}

// Ancestors returns the client, branch and region above a project, for reports. Rows that
// were deleted since still answer: a report about the past names them.
func (p *Projects) Ancestors(ctx context.Context, organizationID int64, code string) (Ancestors, error) {
	var out Ancestors
	err := database.WithTenant(ctx, p.pool, organizationID, func(q *db.Queries) error {
		row, err := q.GetProjectAncestors(ctx, code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrProjectNotFound
		}
		if err != nil {
			return fmt.Errorf("get project ancestors: %w", err)
		}
		out = Ancestors{
			ClientCode: row.ClientCode, ClientName: row.ClientDescription,
			BranchCode: row.BranchCode, BranchName: row.BranchDescription,
			RegionCode: row.RegionCode, RegionDescription: row.RegionDescription,
		}
		return nil
	})
	return out, err
}

func validateTimezone(tz string) error {
	if !slices.Contains(timeutil.SupportedTimezones, tz) {
		return validationError("timezone", "Harus salah satu dari "+strings.Join(timeutil.SupportedTimezones, ", ")+".")
	}
	return nil
}

// validatePoint requires a latitude and a longitude together or not at all.
func validatePoint(lat, lng *float64) error {
	if (lat == nil) != (lng == nil) {
		return validationError("latitude", "Latitude dan longitude harus diisi bersama.")
	}
	return nil
}

func validateCompanyPrefix(prefix *string) error {
	// An empty prefix is invalid too: stored as '' it breaks the table's CHECK (a 500).
	if prefix != nil && !clientPrefixPattern.MatchString(*prefix) {
		return ErrClientPrefixInvalid
	}
	return nil
}

// pickCoordinate is the value a coordinate has after a patch: the new one, or the stored one.
func pickCoordinate(patch, stored *float64) *float64 {
	if patch != nil {
		return patch
	}
	return stored
}

func encodeContacts(contacts []EmergencyContact) (json.RawMessage, error) {
	if contacts == nil {
		contacts = []EmergencyContact{}
	}
	b, err := json.Marshal(contacts)
	if err != nil {
		return nil, fmt.Errorf("encode emergency contacts: %w", err)
	}
	return b, nil
}

// encodeContactsPatch is encodeContacts for a patch, where nil means "leave it alone" (a NULL
// in the query) and an empty list means "no contacts".
func encodeContactsPatch(contacts []EmergencyContact) (json.RawMessage, error) {
	if contacts == nil {
		return nil, nil
	}
	return encodeContacts(contacts)
}

func toProject(r db.Project) Project {
	var contacts []EmergencyContact
	// The column is NOT NULL DEFAULT '[]' and only this package writes it, so a value that
	// does not decode is a bug, not input: the list comes back empty rather than failing a read.
	if err := json.Unmarshal(r.EmergencyContacts, &contacts); err != nil || contacts == nil {
		contacts = []EmergencyContact{}
	}
	return Project{
		Code: r.Code, ClientCode: r.ClientCode, Description: r.Description,
		CompanyCode: textValue(r.CompanyCode), CompanyPrefix: textValue(r.CompanyPrefix), CompanyBidangUsaha: textValue(r.CompanyBidangUsaha),
		BusinessLine: string(r.BusinessLine), City: textValue(r.City), Address: textValue(r.Address),
		Phone: textValue(r.Phone), Email: textValue(r.Email), Timezone: r.Timezone,
		Latitude: r.Latitude, Longitude: r.Longitude, AttendanceRadiusMeters: int(r.AttendanceRadiusMeters),
		StrictAttendanceGeofence: r.StrictAttendanceGeofence, GPSTrackingEnabled: r.GpsTrackingEnabled,
		PatrolIntervalMinutes: intValue(r.PatrolIntervalMinutes), EmergencyContacts: contacts,
		Active: r.Active, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}
}

// projectMeta is what an audit row keeps of a project. The emergency contacts are only
// counted: their phone numbers are not an audit matter.
func projectMeta(p Project) map[string]any {
	return map[string]any{
		"client_code": p.ClientCode, "business_line": p.BusinessLine, "description": p.Description,
		"company_code": p.CompanyCode, "company_prefix": p.CompanyPrefix, "company_bidang_usaha": p.CompanyBidangUsaha,
		"city": p.City, "address": p.Address, "phone": p.Phone, "email": p.Email, "timezone": p.Timezone,
		"latitude": p.Latitude, "longitude": p.Longitude, "attendance_radius_meters": p.AttendanceRadiusMeters,
		"strict_attendance_geofence": p.StrictAttendanceGeofence, "gps_tracking_enabled": p.GPSTrackingEnabled,
		"patrol_interval_minutes": p.PatrolIntervalMinutes, "emergency_contacts": len(p.EmergencyContacts),
		"active": p.Active,
	}
}
