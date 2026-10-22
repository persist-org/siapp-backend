package organization

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/timeutil"
)

// Field roles of a team member (DL-04).
const (
	RoleDanru  = "DANRU"
	RoleMember = "MEMBER"
)

// PeopleResolver finds or creates the local copy of a Personalia person by NIK (identity.People
// does). A person who never signed in can be put in a regu this way.
type PeopleResolver interface {
	Ensure(ctx context.Context, organizationID int64, nik string) (int64, error)
}

// Member is one membership of a person in a regu: a row of team_members with the person's
// name. EndDate is inclusive, nil while the membership has no end.
type Member struct {
	ID          int64
	TeamCode    string
	PersonnelID int64
	FullName    string
	NIK         string
	Role        string
	StartDate   timeutil.Date
	EndDate     *timeutil.Date
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// MemberInput is a request to add a person to a regu by NIK.
type MemberInput struct {
	NIK       string        `json:"nik" validate:"required,notblank,max=32"`
	Role      string        `json:"role" validate:"required,oneof=DANRU MEMBER"`
	StartDate timeutil.Date `json:"start_date"`
}

// MoveInput is a request to move a membership to another regu from a date.
type MoveInput struct {
	ToTeamCode    string        `json:"to_team_code" validate:"required"`
	EffectiveDate timeutil.Date `json:"effective_date"`
}

// ChangeRoleInput is a request to change the field role of a membership from a date.
type ChangeRoleInput struct {
	Role          string        `json:"role" validate:"required,oneof=DANRU MEMBER"`
	EffectiveDate timeutil.Date `json:"effective_date"`
}

// Members manages who serves in which regu.
//
// ⚠ History is kept (see migration 00011). Nothing here overwrites a membership: leaving ends
// it, and a move or a role change ends it the day before the new one starts, both in ONE
// transaction. Past attendance and reports then keep their original regu.
type Members struct {
	pool   *pgxpool.Pool
	clock  clock.Clock
	people PeopleResolver
}

// NewMembers returns the Members usecase. people may be nil only where nobody is added by NIK.
func NewMembers(pool *pgxpool.Pool, c clock.Clock, people PeopleResolver) *Members {
	return &Members{pool: pool, clock: c, people: people}
}

// List returns the members of a regu on a date (today in the project's timezone when on is
// nil): the DANRU first, then by name. A REGION_ADMIN may read only a regu of their region.
func (m *Members) List(ctx context.Context, actor auth.Principal, scope access.AdminScope, teamCode string, on *timeutil.Date, limit, offset int) ([]Member, int, error) {
	if !scope.AllowsRegion(RegionOfCode(teamCode)) {
		return nil, 0, access.ErrOutsideAdminScope
	}
	var out []Member
	var total int64
	err := database.WithTenant(ctx, m.pool, actor.OrganizationID, func(q *db.Queries) error {
		team, err := q.GetTeam(ctx, teamCode)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTeamNotFound
		}
		if err != nil {
			return fmt.Errorf("get team: %w", err)
		}
		var date timeutil.Date
		if on != nil {
			date = *on
		} else if date, err = todayOf(ctx, q, team, m.clock.Now()); err != nil {
			return err
		}
		if offset > math.MaxInt32 || limit > math.MaxInt32 {
			return nil
		}
		rows, err := q.ListTeamMembersOn(ctx, db.ListTeamMembersOnParams{
			TeamCode: teamCode, OnDate: dateParam(date),
			PageLimit: int32(limit), PageOffset: int32(offset), //nolint:gosec // bounded just above
		})
		if err != nil {
			return fmt.Errorf("list team members: %w", err)
		}
		for _, row := range rows {
			out = append(out, toMember(db.GetTeamMemberViewRow(row)))
		}
		total, err = q.CountTeamMembersOn(ctx, db.CountTeamMembersOnParams{TeamCode: teamCode, OnDate: dateParam(date)})
		if err != nil {
			return fmt.Errorf("count team members: %w", err)
		}
		return nil
	})
	return out, int(total), err
}

// Add puts a person, found by NIK, in a regu from start_date. A REGION_ADMIN may add only to
// a regu of their region.
//
// The person is resolved with PeopleResolver, so someone who never signed in can be added
// (their row is fetched from Personalia first). A person Personalia reports as inactive is
// refused. A person already in this regu on those dates is MEMBERSHIP_OVERLAP, and a second
// DANRU is TEAM_HAS_DANRU: both are the database's exclusion constraints speaking.
func (m *Members) Add(ctx context.Context, actor auth.Principal, scope access.AdminScope, teamCode string, in MemberInput) (Member, error) {
	if in.StartDate.IsZero() {
		return Member{}, validationError("start_date", "Wajib diisi dengan format YYYY-MM-DD.")
	}
	if !scope.AllowsRegion(RegionOfCode(teamCode)) {
		return Member{}, access.ErrOutsideAdminScope
	}
	// Cheap checks before Personalia is asked: a regu that does not exist costs no lookup.
	if err := database.WithTenant(ctx, m.pool, actor.OrganizationID, func(q *db.Queries) error {
		_, err := q.GetTeam(ctx, teamCode)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTeamNotFound
		}
		return err
	}); err != nil {
		return Member{}, err
	}
	personnelID, err := m.people.Ensure(ctx, actor.OrganizationID, in.NIK)
	if errors.Is(err, apperr.ErrNotFound) {
		return Member{}, ErrPersonnelNotFound
	}
	if err != nil {
		return Member{}, err
	}

	var out Member
	err = database.WithTenant(ctx, m.pool, actor.OrganizationID, func(q *db.Queries) error {
		// The regu is locked, so a delete of it cannot commit between this check and the insert.
		if _, err := q.GetTeamForUpdate(ctx, teamCode); errors.Is(err, pgx.ErrNoRows) {
			return ErrTeamNotFound
		} else if err != nil {
			return fmt.Errorf("get team: %w", err)
		}
		person, err := q.GetPersonnel(ctx, personnelID)
		if err != nil {
			return fmt.Errorf("get personnel: %w", err)
		}
		if person.Status == db.PersonnelStatusINACTIVE {
			return ErrPersonnelInactive
		}
		row, err := q.InsertTeamMember(ctx, db.InsertTeamMemberParams{
			TeamCode: teamCode, PersonnelID: personnelID, Role: db.TeamRole(in.Role),
			StartDate: dateParam(in.StartDate), Now: ts(m.clock.Now()),
		})
		if err != nil {
			return err
		}
		out, err = memberView(ctx, q, row.ID)
		if err != nil {
			return err
		}
		return recordMemberAudit(ctx, q, actor, "team_member.add", row.ID, map[string]any{
			"team_code": teamCode, "personnel_id": personnelID, "role": in.Role, "start_date": in.StartDate.String(),
		})
	})
	if err != nil {
		return Member{}, memberError(err, "add team member")
	}
	return out, nil
}

// End takes a person out of a regu as of today in the project's timezone: their open
// membership gets today as its end date, and the row stays. A REGION_ADMIN may end only one
// in a regu of their region.
//
// A membership that has not started yet (its start date is after today) never took effect,
// so there is no history to keep: it is removed.
func (m *Members) End(ctx context.Context, actor auth.Principal, scope access.AdminScope, teamCode string, personnelID int64) error {
	if !scope.AllowsRegion(RegionOfCode(teamCode)) {
		return access.ErrOutsideAdminScope
	}
	return database.WithTenant(ctx, m.pool, actor.OrganizationID, func(q *db.Queries) error {
		// ⚠ The regu is locked before any membership, as replace and a team delete do. Without
		// it a change of role in flight (old row ended at F-1, new row from F, not yet committed)
		// is invisible to the delete of planned rows below, and the new row would outlive this
		// call: the person comes back in the regu on F.
		team, err := q.GetTeamForUpdate(ctx, teamCode)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTeamNotFound
		}
		if err != nil {
			return fmt.Errorf("get team: %w", err)
		}
		today, err := todayOf(ctx, q, team, m.clock.Now())
		if err != nil {
			return err
		}
		// ⚠ Not only the open row: a move or a role change planned for a later date leaves the
		// row in force today with that later end date, and the planned row after it. Both must
		// go, or the person would stay in the regu until the planned date (or join it then).
		cancelled, err := q.DeletePlannedMembershipsOfPerson(ctx, db.DeletePlannedMembershipsOfPersonParams{TeamCode: teamCode, PersonnelID: personnelID, Today: dateParam(today)})
		if err != nil {
			return fmt.Errorf("delete planned memberships: %w", err)
		}
		ended, err := q.EndMembershipsOfPersonToday(ctx, db.EndMembershipsOfPersonTodayParams{TeamCode: teamCode, PersonnelID: personnelID, Today: dateParam(today), Now: ts(m.clock.Now())})
		if err != nil {
			return fmt.Errorf("end memberships: %w", err)
		}
		if len(cancelled) == 0 && len(ended) == 0 {
			return ErrMembershipNotFound
		}
		meta := map[string]any{"team_code": teamCode, "personnel_id": personnelID, "cancelled_ids": cancelled, "ended_ids": ended}
		id := slices.Concat(ended, cancelled)[0]
		if len(ended) > 0 {
			meta["end_date"] = today.String()
		}
		return recordMemberAudit(ctx, q, actor, "team_member.end", id, meta)
	})
}

// Move ends a membership the day before effective_date and starts the same role in another
// regu on that date, in one transaction. It returns the new membership. A REGION_ADMIN needs
// both regu in their region.
//
// ⚠ The old row is ended, not rewritten: a report about a day before the move still names the
// regu the person served in then. It also keeps the constraints honest: the new row is
// checked for overlap and for a second DANRU like any other.
func (m *Members) Move(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64, in MoveInput) (Member, error) {
	if in.EffectiveDate.IsZero() {
		return Member{}, validationError("effective_date", "Wajib diisi dengan format YYYY-MM-DD.")
	}
	if !scope.AllowsRegion(RegionOfCode(in.ToTeamCode)) {
		return Member{}, access.ErrOutsideAdminScope
	}
	return m.replace(ctx, actor, scope, id, in.EffectiveDate, "team_member.move", func(old db.TeamMember) (string, db.TeamRole, error) {
		if in.ToTeamCode == old.TeamCode {
			return "", "", validationError("to_team_code", "Harus regu yang berbeda dari regu sekarang.")
		}
		return in.ToTeamCode, old.Role, nil
	})
}

// ChangeRole ends a membership the day before effective_date and starts the new role in the
// same regu on that date, in one transaction. It returns the new membership.
func (m *Members) ChangeRole(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64, in ChangeRoleInput) (Member, error) {
	if in.EffectiveDate.IsZero() {
		return Member{}, validationError("effective_date", "Wajib diisi dengan format YYYY-MM-DD.")
	}
	return m.replace(ctx, actor, scope, id, in.EffectiveDate, "team_member.change_role", func(old db.TeamMember) (string, db.TeamRole, error) {
		if db.TeamRole(in.Role) == old.Role {
			return "", "", validationError("role", "Harus berbeda dari peran sekarang.")
		}
		return old.TeamCode, db.TeamRole(in.Role), nil
	})
}

// replace is what a move and a role change share: end the open membership on effective-1 and
// insert the follow-up row (the target regu and role decide returns) from effective.
func (m *Members) replace(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64, effective timeutil.Date,
	action string, target func(old db.TeamMember) (teamCode string, role db.TeamRole, err error),
) (Member, error) {
	var out Member
	err := database.WithTenant(ctx, m.pool, actor.OrganizationID, func(q *db.Queries) error {
		peek, err := q.GetTeamMember(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMembershipNotFound
		}
		if err != nil {
			return fmt.Errorf("get team member: %w", err)
		}
		if !scope.AllowsRegion(RegionOfCode(peek.TeamCode)) {
			return access.ErrOutsideAdminScope
		}
		// Only a membership still in force can be replaced; an ended one is history.
		if peek.EndDate.Valid {
			return ErrMembershipNotFound
		}
		toTeam, role, err := target(peek)
		if err != nil {
			return err
		}
		// ⚠ Lock order: the regu first (both, by code), then the membership. A team delete takes
		// its regu and then its memberships; locking the membership first here would deadlock
		// with it.
		for _, code := range slices.Compact(slices.Sorted(slices.Values([]string{peek.TeamCode, toTeam}))) {
			if _, err := q.GetTeamForUpdate(ctx, code); errors.Is(err, pgx.ErrNoRows) {
				if code == peek.TeamCode {
					return ErrMembershipNotFound // its regu was deleted, which ended it
				}
				return ErrTeamNotFound
			} else if err != nil {
				return fmt.Errorf("get team: %w", err)
			}
		}
		old, err := q.GetTeamMemberForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMembershipNotFound
		}
		if err != nil {
			return fmt.Errorf("get team member: %w", err)
		}
		if old.EndDate.Valid { // replaced or ended while this request waited for the locks
			return ErrMembershipNotFound
		}
		if !dateValue(old.StartDate).Before(effective) {
			return validationError("effective_date", "Harus setelah tanggal mulai keanggotaan ("+dateValue(old.StartDate).String()+").")
		}
		person, err := q.GetPersonnel(ctx, old.PersonnelID)
		if err != nil {
			return fmt.Errorf("get personnel: %w", err)
		}
		if person.Status == db.PersonnelStatusINACTIVE {
			return ErrPersonnelInactive.WithMessage("Personil tidak aktif di Personalia dan tidak dapat dipindah atau diubah perannya.")
		}
		if _, err := q.EndTeamMember(ctx, db.EndTeamMemberParams{ID: old.ID, EndDate: dateParam(effective.AddDays(-1)), Now: ts(m.clock.Now())}); err != nil {
			return fmt.Errorf("end team member: %w", err)
		}
		row, err := q.InsertTeamMember(ctx, db.InsertTeamMemberParams{
			TeamCode: toTeam, PersonnelID: old.PersonnelID, Role: role, StartDate: dateParam(effective), Now: ts(m.clock.Now()),
		})
		if err != nil {
			return err
		}
		out, err = memberView(ctx, q, row.ID)
		if err != nil {
			return err
		}
		return recordMemberAudit(ctx, q, actor, action, row.ID, map[string]any{
			"previous_id": old.ID, "personnel_id": old.PersonnelID, "effective_date": effective.String(),
			"from_team_code": old.TeamCode, "to_team_code": toTeam, "from_role": string(old.Role), "to_role": string(role),
		})
	})
	if err != nil {
		return Member{}, memberError(err, action)
	}
	return out, nil
}

// memberError turns the database's exclusion violations into their reasons.
func memberError(err error, what string) error {
	if database.IsExclusionViolation(err) {
		switch database.ConstraintName(err) {
		case "team_members_no_overlap":
			return ErrMembershipOverlap
		case "team_members_one_danru":
			return ErrTeamHasDanru
		}
	}
	var appErr *apperr.Error
	if errors.As(err, &appErr) {
		return err
	}
	return fmt.Errorf("%s: %w", what, err)
}

// todayOf is today's business date of a regu: the calendar date in its project's timezone
// (23:30 UTC is already tomorrow in all three Indonesian zones).
func todayOf(ctx context.Context, q *db.Queries, team db.Team, now time.Time) (timeutil.Date, error) {
	project, err := q.GetProject(ctx, team.ProjectCode)
	if err != nil {
		return timeutil.Date{}, fmt.Errorf("get project of team %s: %w", team.Code, err)
	}
	loc, err := time.LoadLocation(project.Timezone)
	if err != nil {
		return timeutil.Date{}, fmt.Errorf("project %s: load timezone %q: %w", project.Code, project.Timezone, err)
	}
	return timeutil.BusinessDate(now, loc), nil
}

func memberView(ctx context.Context, q *db.Queries, id int64) (Member, error) {
	row, err := q.GetTeamMemberView(ctx, id)
	if err != nil {
		return Member{}, fmt.Errorf("get team member view: %w", err)
	}
	return toMember(row), nil
}

func toMember(r db.GetTeamMemberViewRow) Member {
	m := Member{
		ID: r.ID, TeamCode: r.TeamCode, PersonnelID: r.PersonnelID, FullName: r.FullName, Role: string(r.Role),
		StartDate: dateValue(r.StartDate), CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}
	if r.Nik.Valid {
		m.NIK = r.Nik.String
	}
	if r.EndDate.Valid {
		end := dateValue(r.EndDate)
		m.EndDate = &end
	}
	return m
}

// recordMemberAudit writes one membership audit row. It names the membership by its number
// (team_members has one) and keeps the NIK out: the person is the personnel id.
func recordMemberAudit(ctx context.Context, q *db.Queries, actor auth.Principal, action string, id int64, meta map[string]any) error {
	actorID := actor.PersonnelID
	if err := audit.Record(ctx, q, audit.Entry{
		ActorPersonnelID: &actorID, Action: action, EntityType: "team_member", EntityID: id, Metadata: meta,
	}); err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	return nil
}

// dateParam is a business date as a date column parameter.
func dateParam(d timeutil.Date) pgtype.Date { return pgtype.Date{Time: d.Time(), Valid: true} }

// dateValue is a date column as a business date.
func dateValue(d pgtype.Date) timeutil.Date {
	y, mo, day := d.Time.Date()
	return timeutil.NewDate(y, mo, day)
}
