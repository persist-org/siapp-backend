package organization

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/identity"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/timeutil"
)

// Membership is one regu a person serves in, with the team's and project's names.
type Membership struct {
	ID          int64
	TeamCode    string
	TeamName    string
	ProjectCode string
	ProjectName string
	Role        string
	StartDate   timeutil.Date
	EndDate     *timeutil.Date
}

// TeamWithProject is a regu with the proyek it belongs to.
type TeamWithProject struct {
	Team    Team
	Project Project
}

// Query answers other modules' questions about who serves where. Every method takes the
// caller's organization id (from auth.From), never a request value, and every date is a
// business date in the project's timezone. A membership counts on D when it starts on or
// before D and ends on or after D (or has no end).
type Query struct {
	pool *pgxpool.Pool
}

// NewQuery returns Query.
func NewQuery(pool *pgxpool.Pool) *Query { return &Query{pool: pool} }

// ActiveMemberships returns the regu a person serves in on a date, by team code. A deleted
// regu is not one of them.
func (qy *Query) ActiveMemberships(ctx context.Context, organizationID, personnelID int64, on timeutil.Date) ([]Membership, error) {
	var out []Membership
	err := database.WithTenant(ctx, qy.pool, organizationID, func(q *db.Queries) error {
		rows, err := q.ListMembershipsOfPersonnelOn(ctx, db.ListMembershipsOfPersonnelOnParams{PersonnelID: personnelID, OnDate: dateParam(on)})
		if err != nil {
			return fmt.Errorf("list memberships: %w", err)
		}
		for _, r := range rows {
			out = append(out, toMembership(r.ID, r.TeamCode, r.TeamName, r.ProjectCode, r.ProjectName, string(r.Role), r.StartDate, r.EndDate))
		}
		return nil
	})
	return out, err
}

// TeamMembers returns everyone who serves in a regu on a date: the DANRU first, then by name.
func (qy *Query) TeamMembers(ctx context.Context, organizationID int64, teamCode string, on timeutil.Date) ([]Member, error) {
	var out []Member
	err := database.WithTenant(ctx, qy.pool, organizationID, func(q *db.Queries) error {
		rows, err := q.ListTeamMembersOn(ctx, db.ListTeamMembersOnParams{TeamCode: teamCode, OnDate: dateParam(on), PageLimit: 100000})
		if err != nil {
			return fmt.Errorf("list team members: %w", err)
		}
		for _, r := range rows {
			out = append(out, toMember(db.GetTeamMemberViewRow(r)))
		}
		return nil
	})
	return out, err
}

// TeamDanru returns the DANRU of a regu on a date, or nil when it has none.
func (qy *Query) TeamDanru(ctx context.Context, organizationID int64, teamCode string, on timeutil.Date) (*Member, error) {
	var out *Member
	err := database.WithTenant(ctx, qy.pool, organizationID, func(q *db.Queries) error {
		row, err := q.GetTeamDanruOn(ctx, db.GetTeamDanruOnParams{TeamCode: teamCode, OnDate: dateParam(on)})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("get team danru: %w", err)
		}
		m := toMember(db.GetTeamMemberViewRow(row))
		out = &m
		return nil
	})
	return out, err
}

// IsMemberOfProject reports whether a person serves in any regu of the project on a date. A
// DANRU is a member of the project of their regu, and so is a MEMBER (there is no separate
// project assignment).
func (qy *Query) IsMemberOfProject(ctx context.Context, organizationID, personnelID int64, projectCode string, on timeutil.Date) (bool, error) {
	var ok bool
	err := database.WithTenant(ctx, qy.pool, organizationID, func(q *db.Queries) error {
		var err error
		ok, err = q.IsMemberOfProjectOn(ctx, db.IsMemberOfProjectOnParams{PersonnelID: personnelID, ProjectCode: projectCode, OnDate: dateParam(on)})
		return err
	})
	return ok, err
}

// IsDanruOfProject reports whether a person is the DANRU of any regu of the project on a date.
func (qy *Query) IsDanruOfProject(ctx context.Context, organizationID, personnelID int64, projectCode string, on timeutil.Date) (bool, error) {
	var ok bool
	err := database.WithTenant(ctx, qy.pool, organizationID, func(q *db.Queries) error {
		var err error
		ok, err = q.IsDanruOfProjectOn(ctx, db.IsDanruOfProjectOnParams{PersonnelID: personnelID, ProjectCode: projectCode, OnDate: dateParam(on)})
		return err
	})
	return ok, err
}

// TeamWithProject returns a regu and its proyek. TEAM_NOT_FOUND for a regu that does not
// exist, was deleted, or belongs to another organization.
func (qy *Query) TeamWithProject(ctx context.Context, organizationID int64, teamCode string) (TeamWithProject, error) {
	var out TeamWithProject
	err := database.WithTenant(ctx, qy.pool, organizationID, func(q *db.Queries) error {
		team, err := q.GetTeam(ctx, teamCode)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTeamNotFound
		}
		if err != nil {
			return fmt.Errorf("get team: %w", err)
		}
		project, err := q.GetProject(ctx, team.ProjectCode)
		if err != nil {
			return fmt.Errorf("get project of team %s: %w", teamCode, err)
		}
		out = TeamWithProject{Team: toTeam(team), Project: toProject(project)}
		return nil
	})
	return out, err
}

// Directory answers identity's two questions about a person's regu: which koordinator wilayah
// they serve under (what a REGION_ADMIN's scope is checked against) and which regu they are in
// (what /auth/me lists). "Today" is each regu's own business date, so it is computed per
// project timezone in SQL. It runs in the caller's transaction (the q it is handed).
type Directory struct {
	clock clock.Clock
}

// NewDirectory returns the Directory identity.Usecase takes through UseRegionLookup and
// UseMembershipLookup.
func NewDirectory(c clock.Clock) *Directory { return &Directory{clock: c} }

var (
	_ identity.RegionLookup     = (*Directory)(nil)
	_ identity.MembershipLookup = (*Directory)(nil)
)

// RegionCodesOf returns the koordinator wilayah a person serves under today, each once.
func (d *Directory) RegionCodesOf(ctx context.Context, q *db.Queries, personnelID int64) ([]string, error) {
	rows, err := q.ListCurrentMembershipsOfPersonnel(ctx, db.ListCurrentMembershipsOfPersonnelParams{PersonnelID: personnelID, Now: ts(d.clock.Now())})
	if err != nil {
		return nil, fmt.Errorf("list current memberships: %w", err)
	}
	var regions []string
	for _, r := range rows {
		if region := RegionOfCode(r.TeamCode); region != "" && !slices.Contains(regions, region) {
			regions = append(regions, region)
		}
	}
	return regions, nil
}

// MembershipsOf returns the regu a person serves in today.
func (d *Directory) MembershipsOf(ctx context.Context, q *db.Queries, personnelID int64) ([]identity.Membership, error) {
	rows, err := q.ListCurrentMembershipsOfPersonnel(ctx, db.ListCurrentMembershipsOfPersonnelParams{PersonnelID: personnelID, Now: ts(d.clock.Now())})
	if err != nil {
		return nil, fmt.Errorf("list current memberships: %w", err)
	}
	out := make([]identity.Membership, 0, len(rows))
	for _, r := range rows {
		out = append(out, identity.Membership{
			TeamCode: r.TeamCode, TeamName: r.TeamName, ProjectCode: r.ProjectCode, ProjectName: r.ProjectName,
			BusinessLine: string(r.BusinessLine), Role: string(r.Role), StartDate: dateValue(r.StartDate),
		})
	}
	return out, nil
}

func toMembership(id int64, teamCode, teamName, projectCode, projectName, role string, start, end pgtype.Date) Membership {
	m := Membership{
		ID: id, TeamCode: teamCode, TeamName: teamName, ProjectCode: projectCode, ProjectName: projectName,
		Role: role, StartDate: dateValue(start),
	}
	if end.Valid {
		e := dateValue(end)
		m.EndDate = &e
	}
	return m
}
