package organization

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
)

// Team is one regu. Its description is its name.
type Team struct {
	Code                           string
	ProjectCode                    string
	Description                    string
	Phone                          *string
	MinPersonnel                   int
	Notes                          *string
	ValidateShift                  bool
	RejectAttendanceAfterTolerance bool
	RelationshipOfficerPersonnelID *int64
	Active                         bool
	CreatedAt                      time.Time
	UpdatedAt                      time.Time
}

// TeamInput is a request to create a team. The code is built from ProjectCode and the
// project's next team number (D19), so the client never sends one.
type TeamInput struct {
	ProjectCode                    string  `json:"project_code" validate:"required"`
	Description                    string  `json:"description" validate:"required,notblank,max=200"`
	Phone                          *string `json:"phone" validate:"omitempty,max=50"`
	MinPersonnel                   *int    `json:"min_personnel" validate:"omitempty,gte=0,lte=10000"`
	Notes                          *string `json:"notes" validate:"omitempty,max=1000"`
	ValidateShift                  *bool   `json:"validate_shift"`
	RejectAttendanceAfterTolerance *bool   `json:"reject_attendance_after_tolerance"`
	RelationshipOfficerPersonnelID *int64  `json:"relationship_officer_personnel_id" validate:"omitempty,gte=1"`
	Active                         *bool   `json:"is_active"`
}

// TeamUpdate is a request to change a team. The code and the project have no field: the code
// is built from them, so a client that sends one gets 422 from the decoder's unknown-field
// rule.
type TeamUpdate struct {
	Description                    *string `json:"description" validate:"omitempty,notblank,max=200"`
	Phone                          *string `json:"phone" validate:"omitempty,max=50"`
	MinPersonnel                   *int    `json:"min_personnel" validate:"omitempty,gte=0,lte=10000"`
	Notes                          *string `json:"notes" validate:"omitempty,max=1000"`
	ValidateShift                  *bool   `json:"validate_shift"`
	RejectAttendanceAfterTolerance *bool   `json:"reject_attendance_after_tolerance"`
	RelationshipOfficerPersonnelID *int64  `json:"relationship_officer_personnel_id" validate:"omitempty,gte=1"`
	Active                         *bool   `json:"is_active"`
}

// TeamFilter narrows a list. Zero values match everything.
type TeamFilter struct {
	Q           string
	ProjectCode string
	Active      *bool
	Limit       int
	Offset      int
}

// Teams manages the regu of an organization.
type Teams struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

// NewTeams returns the Teams usecase.
func NewTeams(pool *pgxpool.Pool, c clock.Clock) *Teams {
	return &Teams{pool: pool, clock: c}
}

// List returns one page of the teams the caller may see, and their total. A REGION_ADMIN
// sees only the teams under their own region.
func (t *Teams) List(ctx context.Context, actor auth.Principal, scope access.AdminScope, f TeamFilter) ([]Team, int, error) {
	var out []Team
	var total int64
	err := database.WithTenant(ctx, t.pool, actor.OrganizationID, func(q *db.Queries) error {
		if f.Offset > math.MaxInt32 || f.Limit > math.MaxInt32 {
			return nil
		}
		rows, err := q.ListTeams(ctx, db.ListTeamsParams{
			ScopeRegion: scopeFilter(scope), ProjectCode: textOrNull(f.ProjectCode), Active: boolOrNull(f.Active),
			Q: textOrNull(f.Q), PageLimit: int32(f.Limit), PageOffset: int32(f.Offset), //nolint:gosec // bounded just above
		})
		if err != nil {
			return fmt.Errorf("list teams: %w", err)
		}
		for _, row := range rows {
			out = append(out, toTeam(row))
		}
		total, err = q.CountTeams(ctx, db.CountTeamsParams{
			ScopeRegion: scopeFilter(scope), ProjectCode: textOrNull(f.ProjectCode), Active: boolOrNull(f.Active), Q: textOrNull(f.Q),
		})
		if err != nil {
			return fmt.Errorf("count teams: %w", err)
		}
		return nil
	})
	return out, int(total), err
}

// Get returns one team. A REGION_ADMIN may read only one under their own region.
func (t *Teams) Get(ctx context.Context, actor auth.Principal, scope access.AdminScope, code string) (Team, error) {
	if !scope.AllowsRegion(RegionOfCode(code)) {
		return Team{}, access.ErrOutsideAdminScope
	}
	var out Team
	err := database.WithTenant(ctx, t.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetTeam(ctx, code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTeamNotFound
		}
		if err != nil {
			return fmt.Errorf("get team: %w", err)
		}
		out = toTeam(row)
		return nil
	})
	return out, err
}

// Create adds a team under a project. A REGION_ADMIN may create only under their own region.
//
// ⚠ The team's number comes from the project's counter, which is raised in the same
// transaction as the insert. Numbers are never reused: after S2 is deleted, the next team is
// S3 (or S5 when S4 exists), because past attendance and reports keep pointing at the old
// code.
func (t *Teams) Create(ctx context.Context, actor auth.Principal, scope access.AdminScope, in TeamInput) (Team, error) {
	in.ProjectCode = strings.TrimSpace(in.ProjectCode)
	if !scope.AllowsRegion(RegionOfCode(in.ProjectCode)) {
		return Team{}, access.ErrOutsideAdminScope
	}
	minPersonnel := int32(0)
	if in.MinPersonnel != nil {
		minPersonnel = clampInt32(*in.MinPersonnel)
	}

	var out Team
	err := database.WithTenant(ctx, t.pool, actor.OrganizationID, func(q *db.Queries) error {
		// The project is locked: two admins adding a team at once take turns at the counter,
		// and a delete of the project cannot commit between this check and the insert.
		if _, err := q.GetProjectForUpdate(ctx, in.ProjectCode); errors.Is(err, pgx.ErrNoRows) {
			return ErrProjectNotFound
		} else if err != nil {
			return fmt.Errorf("get project: %w", err)
		}
		if err := t.checkOfficer(ctx, q, in.RelationshipOfficerPersonnelID); err != nil {
			return err
		}
		number, err := q.TakeNextTeamNumber(ctx, in.ProjectCode)
		if err != nil {
			return fmt.Errorf("take team number: %w", err)
		}
		code := BuildTeamCode(in.ProjectCode, int(number))
		row, err := q.CreateTeam(ctx, db.CreateTeamParams{
			Code: code, ProjectCode: in.ProjectCode, Description: strings.TrimSpace(in.Description),
			Phone: textPtr(in.Phone), MinPersonnel: minPersonnel, Notes: textPtr(in.Notes),
			ValidateShift:                  in.ValidateShift == nil || *in.ValidateShift,
			RejectAttendanceAfterTolerance: in.RejectAttendanceAfterTolerance != nil && *in.RejectAttendanceAfterTolerance,
			RelationshipOfficerPersonnelID: int8OrNull(in.RelationshipOfficerPersonnelID),
			Active:                         in.Active == nil || *in.Active, Now: ts(t.clock.Now()),
		})
		if err != nil {
			return err
		}
		out = toTeam(row)
		return recordAudit(ctx, q, actor, "team.create", "team", code, nil, teamMeta(out))
	})
	if database.IsUniqueViolation(err) {
		return Team{}, ErrCodeTaken
	}
	if err != nil {
		return Team{}, fmt.Errorf("create team: %w", err)
	}
	return out, nil
}

// Update changes a team's details and settings. A REGION_ADMIN may change only one under
// their region.
func (t *Teams) Update(ctx context.Context, actor auth.Principal, scope access.AdminScope, code string, in TeamUpdate) (Team, error) {
	if !scope.AllowsRegion(RegionOfCode(code)) {
		return Team{}, access.ErrOutsideAdminScope
	}
	var out Team
	err := database.WithTenant(ctx, t.pool, actor.OrganizationID, func(q *db.Queries) error {
		before, err := q.GetTeamForUpdate(ctx, code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTeamNotFound
		}
		if err != nil {
			return fmt.Errorf("get team: %w", err)
		}
		if err := t.checkOfficer(ctx, q, in.RelationshipOfficerPersonnelID); err != nil {
			return err
		}
		row, err := q.UpdateTeam(ctx, db.UpdateTeamParams{
			Code: code, Description: textPtr(trimmed(in.Description)), Phone: textPtr(in.Phone),
			MinPersonnel: int4OrNull(in.MinPersonnel), Notes: textPtr(in.Notes), ValidateShift: boolOrNull(in.ValidateShift),
			RejectAttendanceAfterTolerance: boolOrNull(in.RejectAttendanceAfterTolerance),
			RelationshipOfficerPersonnelID: int8OrNull(in.RelationshipOfficerPersonnelID),
			Active:                         boolOrNull(in.Active), Now: ts(t.clock.Now()),
		})
		if err != nil {
			return fmt.Errorf("update team: %w", err)
		}
		out = toTeam(row)
		return recordAudit(ctx, q, actor, "team.update", "team", code, teamMeta(toTeam(before)), teamMeta(out))
	})
	return out, err
}

// Delete removes a team (a soft delete: the row and its code stay, and its number is never
// handed out again, D19). A REGION_ADMIN may delete only one under their region.
//
// The team's memberships end with it: the ones in force end today (the project's today), and
// the ones that have not started yet go away. The rows of the ones that ended stay, so past
// attendance and reports still find the regu.
func (t *Teams) Delete(ctx context.Context, actor auth.Principal, scope access.AdminScope, code string) error {
	if !scope.AllowsRegion(RegionOfCode(code)) {
		return access.ErrOutsideAdminScope
	}
	return database.WithTenant(ctx, t.pool, actor.OrganizationID, func(q *db.Queries) error {
		before, err := q.GetTeamForUpdate(ctx, code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTeamNotFound
		}
		if err != nil {
			return fmt.Errorf("get team: %w", err)
		}
		today, err := todayOf(ctx, q, before, t.clock.Now())
		if err != nil {
			return err
		}
		ended, err := q.EndOpenMembershipsOfTeam(ctx, db.EndOpenMembershipsOfTeamParams{TeamCode: code, Today: dateParam(today), Now: ts(t.clock.Now())})
		if err != nil {
			return fmt.Errorf("end memberships of team: %w", err)
		}
		cancelled, err := q.DeleteFutureMembershipsOfTeam(ctx, db.DeleteFutureMembershipsOfTeamParams{TeamCode: code, Today: dateParam(today)})
		if err != nil {
			return fmt.Errorf("delete future memberships of team: %w", err)
		}
		if _, err := q.SoftDeleteTeam(ctx, db.SoftDeleteTeamParams{Code: code, Now: ts(t.clock.Now())}); err != nil {
			return fmt.Errorf("delete team: %w", err)
		}
		meta := teamMeta(toTeam(before))
		meta["memberships_ended"] = ended
		meta["memberships_cancelled"] = cancelled
		return recordAudit(ctx, q, actor, "team.delete", "team", code, meta, nil)
	})
}

// checkOfficer refuses a relationship officer who is not a person of the caller's
// organization. The foreign key alone would accept another tenant's person (it ignores RLS).
func (t *Teams) checkOfficer(ctx context.Context, q *db.Queries, id *int64) error {
	if id == nil {
		return nil
	}
	ok, err := q.PersonnelExists(ctx, *id)
	if err != nil {
		return fmt.Errorf("check relationship officer: %w", err)
	}
	if !ok {
		return validationError("relationship_officer_personnel_id", "Personil tidak ditemukan.")
	}
	return nil
}

func toTeam(r db.Team) Team {
	return Team{
		Code: r.Code, ProjectCode: r.ProjectCode, Description: r.Description, Phone: textValue(r.Phone),
		MinPersonnel: int(r.MinPersonnel), Notes: textValue(r.Notes), ValidateShift: r.ValidateShift,
		RejectAttendanceAfterTolerance: r.RejectAttendanceAfterTolerance,
		RelationshipOfficerPersonnelID: int64Value(r.RelationshipOfficerPersonnelID),
		Active:                         r.Active, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}
}

// teamMeta is what an audit row keeps of a team.
func teamMeta(t Team) map[string]any {
	return map[string]any{
		"project_code": t.ProjectCode, "description": t.Description, "phone": t.Phone,
		"min_personnel": t.MinPersonnel, "notes": t.Notes, "validate_shift": t.ValidateShift,
		"reject_attendance_after_tolerance": t.RejectAttendanceAfterTolerance,
		"relationship_officer_personnel_id": t.RelationshipOfficerPersonnelID, "active": t.Active,
	}
}
